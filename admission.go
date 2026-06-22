package gr

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// ErrOverloaded is returned by Admission.Acquire when the in-flight-query gate stays
// full past its queue wait (doc 18 §8.8, §8.9). The server surfaces it as a retryable
// transient, so a driver backs off and retries rather than the server queueing
// unboundedly: the HTTP surface maps it to a Neo.TransientError status and the Bolt
// surface to a transient StatusError.
var ErrOverloaded = errors.New("gr: too many queries in flight")

// defaultAdmitWait is how long a query waits for a free slot before the gate sheds it
// when the caller asks for the default (a zero queue wait). It is short on purpose: the
// point of the gate is to shed quickly under overload so a driver retries, not to build a
// deep server-side queue (doc 18 §8.9).
const defaultAdmitWait = 100 * time.Millisecond

// Admission is the in-flight-query admission gate that bounds how many queries execute
// at once across every connection (doc 18 §8.8). It protects the one process serving
// many clients: a thousand idle connections cost a thousand cheap goroutines but zero
// engine slots, while the gate caps how many queries contend for the CPU at once, which
// is the resource that actually saturates. Both server surfaces share one gate, so the
// bound holds across the whole process, not per transport.
//
// A nil *Admission is a disabled gate: it admits every query with a no-op release, the
// embedded-friendly default when no limit is configured.
type Admission struct {
	slots chan struct{}
	wait  time.Duration
	shed  atomic.Int64

	// onWait, if set, is called once for every query that did not get a slot immediately, with
	// the time it spent queued, whether it was then admitted, shed, or cancelled (doc 20 §3.1).
	// db.InstrumentAdmission wires it to the admission metrics. A query that found a free slot at
	// once never calls it, so it counts only true queueing.
	onWait func(wait time.Duration)
}

// NewAdmission builds an admission gate that allows maxInFlight queries to execute at
// once, shedding a query that waits longer than queueWait for a slot (doc 18 §8.9). A
// maxInFlight of zero or less returns nil, a disabled gate. A queueWait of zero or less
// uses defaultAdmitWait.
func NewAdmission(maxInFlight int, queueWait time.Duration) *Admission {
	if maxInFlight <= 0 {
		return nil
	}
	if queueWait <= 0 {
		queueWait = defaultAdmitWait
	}
	return &Admission{slots: make(chan struct{}, maxInFlight), wait: queueWait}
}

// Acquire claims an execution slot and returns a release function the caller must call
// when the query finishes (doc 18 §8.9). A query that finds a free slot proceeds at
// once; one that finds the gate full waits up to the queue wait and then sheds with
// ErrOverloaded, so a driver retries rather than the server queueing without bound. A
// cancelled context (a RESET or a timeout while queued) returns the context error.
//
// A nil gate is disabled and admits the query immediately with a no-op release, so a
// caller can always call Acquire without first checking whether a gate is configured.
func (a *Admission) Acquire(ctx context.Context) (func(), error) {
	if a == nil || a.slots == nil {
		return func() {}, nil
	}
	// Fast path: a free slot means no queueing, the common case, which records nothing so the
	// queued metric counts only genuine waits (doc 20 §3.1).
	select {
	case a.slots <- struct{}{}:
		return func() { <-a.slots }, nil
	default:
	}
	// Slow path: the gate is full, so the query queues for a slot. Time the wait and report it
	// however the wait ends, admitted, shed, or cancelled.
	start := time.Now()
	t := time.NewTimer(a.wait)
	defer t.Stop()
	select {
	case a.slots <- struct{}{}:
		a.reportWait(start)
		return func() { <-a.slots }, nil
	case <-t.C:
		a.shed.Add(1)
		a.reportWait(start)
		return nil, ErrOverloaded
	case <-ctx.Done():
		a.reportWait(start)
		return nil, ctx.Err()
	}
}

// reportWait fires the queue-wait hook with the time since start, if a hook is wired.
func (a *Admission) reportWait(start time.Time) {
	if a.onWait != nil {
		a.onWait(time.Since(start))
	}
}

// InstrumentAdmission wires a gate's queue-wait hook to this database's admission metrics (doc 20
// §3.1): once wired, a query that waits for a slot counts in gr_query_queued_total and its wait
// lands in gr_query_queue_wait_seconds, rendered by the same db.Metrics surface the rest of the
// catalogue is. A server that shares one gate across its HTTP and Bolt surfaces wires it once, so
// the queueing both surfaces drive is counted in the one registry. A nil gate (admission
// disabled) is a no-op.
func (db *DB) InstrumentAdmission(a *Admission) {
	if a == nil {
		return
	}
	a.onWait = func(wait time.Duration) { db.metrics.recordQueued(wait) }
}

// InFlight reports how many slots are currently claimed, for metrics and tests (doc 18
// §13.5). A nil gate reports zero.
func (a *Admission) InFlight() int {
	if a == nil {
		return 0
	}
	return len(a.slots)
}

// Shed reports how many queries the gate has shed under overload since it was created
// (doc 18 §13.5). A nonzero value means clients hit the in-flight bound and were asked to
// retry, the signal an operator reads to decide whether to raise the limit. A nil gate
// reports zero.
func (a *Admission) Shed() int64 {
	if a == nil {
		return 0
	}
	return a.shed.Load()
}
