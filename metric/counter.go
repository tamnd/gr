package metric

import "sync/atomic"

// Counter is a monotonically non-decreasing cumulative count of events (doc 20 §2.3):
// queries served, conflicts detected, bytes written. It only ever goes up, and a dashboard
// reads its rate (the derivative) rather than its raw value, so a reset to zero on restart is
// harmless because the rate is computed from the delta.
//
// A counter is sharded for the hot path (doc 20 §7.4): it holds an array of cache-line-padded
// cells, one per scheduler P, and Inc adds to the caller's own cell with a single atomic add
// and no contention against a counter incremented on another core. Value sums the cells, the
// only place the shards meet, and that cost falls on the reader at exposition, not the writer
// on the hot path.
type Counter struct {
	cells []counterCell
}

// counterCell is one shard's count, padded to a full cache line so two shards never share a
// line and an increment on one core never invalidates another core's cached cell (doc 20
// §7.4). The uint64 is the count; the pad fills the rest of the line.
type counterCell struct {
	v atomic.Uint64
	_ [cacheLine - 8]byte
}

// newCounter builds a counter with one cell per scheduler P, the sharding that lets each core
// increment its own cell without contention (doc 20 §7.4).
func newCounter() *Counter {
	return &Counter{cells: make([]counterCell, shardCount())}
}

// Inc adds one to the counter on the caller's shard (doc 20 §7.4): one atomic add to one
// cache line, no lock, no allocation, the production-always hot path.
func (c *Counter) Inc() {
	c.Add(1)
}

// Add adds n to the counter on the caller's shard. n is unsigned because a counter only goes
// up; a caller that wants to record a decrease wants a gauge, not a counter (doc 20 §2.3).
func (c *Counter) Add(n uint64) {
	c.cells[shardIndex(len(c.cells))].v.Add(n)
}

// Value sums the shards into the counter's total (doc 20 §7.4): the read-side cost that keeps
// the write side a single atomic add. It is called at exposition, not on the hot path, so
// summing across cores here is the right place to pay for the sharding.
func (c *Counter) Value() uint64 {
	var sum uint64
	for i := range c.cells {
		sum += c.cells[i].v.Load()
	}
	return sum
}
