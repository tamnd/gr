package gr

import (
	"context"
	"errors"
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
	t := time.NewTimer(a.wait)
	defer t.Stop()
	select {
	case a.slots <- struct{}{}:
		return func() { <-a.slots }, nil
	case <-t.C:
		return nil, ErrOverloaded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// InFlight reports how many slots are currently claimed, for metrics and tests (doc 18
// §13.5). A nil gate reports zero.
func (a *Admission) InFlight() int {
	if a == nil {
		return 0
	}
	return len(a.slots)
}
