package metric

import "sync/atomic"

// Gauge is an instantaneous value that goes up and down (doc 20 §2.4): pages cached right now,
// transactions in flight, bytes of memory held. Unlike a counter, the current value is the
// thing of interest, not its rate, so a dashboard plots the value directly.
//
// A gauge is a single atomic int64 rather than sharded (doc 20 §2.4): a gauge is read as an
// instantaneous snapshot, so the cells would have to be summed into one consistent value
// anyway, and a gauge updated from one owning goroutine (the pool size, the in-flight count)
// does not have the cross-core write contention a counter does. Set, Add, and Sub are all one
// atomic op, and Value is one load.
//
// A gauge may instead be computed at read time from a function (doc 20 §2.4): the page-cache
// size is cheaper to ask the cache for on exposition than to keep a gauge in step with on
// every insert and eviction. A computed gauge holds the function and ignores Set/Add/Sub.
type Gauge struct {
	v       atomic.Int64
	compute func() int64 // when set, Value calls this instead of reading v (doc 20 §2.4)
}

// newGauge builds a settable gauge whose value starts at zero.
func newGauge() *Gauge {
	return &Gauge{}
}

// newComputedGauge builds a gauge whose value is produced by fn at read time (doc 20 §2.4),
// for a quantity a subsystem can report on demand more cheaply than it can keep a gauge in
// step with on every change.
func newComputedGauge(fn func() int64) *Gauge {
	return &Gauge{compute: fn}
}

// Set stores v as the gauge's current value, one atomic store. A computed gauge ignores Set,
// because its value comes from its function, not from a stored cell.
func (g *Gauge) Set(v int64) {
	if g.compute != nil {
		return
	}
	g.v.Store(v)
}

// Add adds delta (which may be negative) to the gauge, the up-down move a gauge allows and a
// counter does not (doc 20 §2.4). A computed gauge ignores it.
func (g *Gauge) Add(delta int64) {
	if g.compute != nil {
		return
	}
	g.v.Add(delta)
}

// Inc adds one to the gauge, the common case for a count entering a set (a transaction
// starting, a page entering the cache).
func (g *Gauge) Inc() {
	g.Add(1)
}

// Dec subtracts one from the gauge, the common case for a count leaving a set.
func (g *Gauge) Dec() {
	g.Add(-1)
}

// Value returns the gauge's current value: the stored cell, or the computed function's
// result when the gauge is computed (doc 20 §2.4).
func (g *Gauge) Value() int64 {
	if g.compute != nil {
		return g.compute()
	}
	return g.v.Load()
}
