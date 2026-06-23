package exec

import (
	"time"

	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
)

// Profile collects the actual per-operator measurements of one instrumented run (doc 20 §9.2). It
// is the profiling shim's accumulator: each operator the executor pulls from reports the rows it
// produced and the wall-clock time spent inside it, keyed by the plan node it executes, so the
// render can place each measurement against the operator EXPLAIN shows. The plan tree EXPLAIN would
// print and the tree PROFILE prints are the same tree; the shim only reads, never reshapes, so the
// physical plan PROFILE measures is byte-for-byte the one it would run uninstrumented (doc 20 §9.3).
//
// A nil *Profile is the uninstrumented path: Open inserts no shim and the executor pays nothing.
type Profile struct {
	stats map[plan.Op]*OpProfile
}

// OpProfile is one operator's actual measurements (doc 20 §9.1): the rows it produced, the number
// of times its parent pulled from it, and the wall-clock time spent inside its next including the
// time spent in its children (inclusive). The exclusive time, the operator's own work with its
// children's subtracted out, the render derives from the tree, since the shim sees only inclusive
// time at each node.
type OpProfile struct {
	Rows      int64
	Calls     int64
	Inclusive time.Duration
}

// NewProfile returns an empty profile ready to be wired onto a [Ctx]. The executor fills it as the
// instrumented run pulls rows, and the library reads it once the cursor drains.
func NewProfile() *Profile {
	return &Profile{stats: map[plan.Op]*OpProfile{}}
}

// Get returns the measurements gathered for one plan operator, or nil when none ran. An operator a
// short-circuit never pulled from (the input below a LIMIT 0, say) has no entry, which the render
// shows as actual rows zero rather than inventing a measurement that never happened.
func (p *Profile) Get(o plan.Op) *OpProfile {
	if p == nil {
		return nil
	}
	return p.stats[o]
}

// statFor returns the measurement cell for an operator, creating it on first use. It runs only at
// compile time, single-threaded, before any row flows, so the map needs no lock.
func (p *Profile) statFor(o plan.Op) *OpProfile {
	s := p.stats[o]
	if s == nil {
		s = &OpProfile{}
		p.stats[o] = s
	}
	return s
}

// wrap inserts the profiling shim around op, the operator compiled from plan node o, so each pull
// records into o's measurement cell. With a nil *Profile it returns op unwrapped, the
// uninstrumented path, so an ordinary run is never slowed by a shim it does not need.
func (p *Profile) wrap(o plan.Op, op operator) operator {
	if p == nil {
		return op
	}
	return &profileOp{inner: op, stat: p.statFor(o)}
}

// profileOp is the additive profiling shim (doc 20 §9.2): it forwards open, next, and close to the
// operator it wraps and, around each next, records the time the pull took and whether it yielded a
// row. It changes no result and reshapes no plan; remove every shim and the run is identical, which
// is what lets PROFILE measure the same execution EXPLAIN describes.
type profileOp struct {
	inner operator
	stat  *OpProfile
}

func (o *profileOp) open(ctx *Ctx) error { return o.inner.open(ctx) }

func (o *profileOp) close() error { return o.inner.close() }

// next times the wrapped operator's pull and records it. The time is inclusive: it covers the
// child pulls this operator made to produce its row, so a parent's inclusive time always covers its
// children's, and the render recovers each operator's own (exclusive) cost by subtracting them.
func (o *profileOp) next() (eval.Row, bool, error) {
	start := time.Now()
	row, ok, err := o.inner.next()
	o.stat.Inclusive += time.Since(start)
	o.stat.Calls++
	if ok {
		o.stat.Rows++
	}
	return row, ok, err
}
