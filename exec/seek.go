package exec

import (
	"math"
	"time"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// nodeIndexSeekOp produces the nodes a NodeScan would, but reaches them through a
// declared property index instead of a label scan (doc 12, the index access path
// the planner's seek rewrite chose). It evaluates the sought value once at open,
// probes the index, residual-checks the reached node by any other required labels,
// and yields each surviving node. The equality Filter the planner leaves above it
// trims the probe result to the exact eq3 matches, so the probe only has to return
// a superset.
//
// The probe widens to honor Cypher's cross-type numeric equality, where the
// integer 1 equals the float 1.0 (doc 02 §7) but the index keys them apart: an
// integral value is probed under both its integer and its float spelling. A value
// the index cannot serve as point probes (a non-scalar, or an integral float too
// large to map back to an integer without loss) falls back to the full label scan,
// which is still correct because the retained Filter does the trimming.
type nodeIndexSeekOp struct {
	spec *plan.NodeIndexSeek
	ctx  *Ctx

	buf  []engine.NodeID
	pos  int
	rest []engine.Token // additional labels the reached node must also carry
	none bool           // an unknown label, key, or null value: the seek is empty
}

func (o *nodeIndexSeekOp) open(ctx *Ctx) error {
	o.ctx, o.buf, o.pos, o.rest, o.none = ctx, nil, 0, nil, false
	if !o.spec.Label.Known || !o.spec.Prop.Known {
		o.none = true
		return nil
	}
	for _, l := range o.spec.Rest {
		if !l.Known {
			o.none = true
			return nil
		}
		o.rest = append(o.rest, l.Token)
	}
	label, key := o.spec.Label.Token, o.spec.Prop.Token
	v, err := eval.Eval(o.spec.Value, ctx.env(eval.Row{}))
	if err != nil {
		return err
	}
	if v.IsNull() {
		// Nothing equals null under eq3, and the index stores no nulls.
		o.none = true
		return nil
	}
	keys, pointProbe := seekKeys(v)
	if !pointProbe {
		return ctx.Tx.ScanLabel(label, o.collect)
	}
	// Time the index descent so the lookup reports its anchor cost (doc 20 §6.4). The probe may
	// touch more than one key for the cross-type numeric case, but it is one logical lookup, so the
	// whole descent is timed and reported once below.
	dstart := time.Now()
	for _, k := range keys {
		served, err := ctx.Tx.IndexSeek(label, key, k, o.collect)
		if err != nil {
			return err
		}
		if !served {
			// The index was dropped between planning and execution; fall back to the
			// scan so the query still answers correctly. This took the scan path, not the
			// index, so it is not reported as an index lookup.
			o.buf = nil
			return ctx.Tx.ScanLabel(label, o.collect)
		}
	}
	o.ctx.countIndexSeek(label, key, "point", time.Since(dstart))
	return nil
}

// collect appends a reached node to the buffer. The probe keys have distinct
// index keys, so the per-key probes return disjoint node sets and no node is
// buffered twice.
func (o *nodeIndexSeekOp) collect(id engine.NodeID) error {
	o.ctx.countScan(1)
	o.buf = append(o.buf, id)
	return nil
}

func (o *nodeIndexSeekOp) next() (eval.Row, bool, error) {
	if o.none {
		return nil, false, nil
	}
	for o.pos < len(o.buf) {
		id := o.buf[o.pos]
		o.pos++
		ok, err := o.hasAll(id)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return eval.Row{o.spec.Var: value.Node(uint64(id))}, true, nil
		}
	}
	return nil, false, nil
}

func (o *nodeIndexSeekOp) hasAll(id engine.NodeID) (bool, error) {
	for _, t := range o.rest {
		has, err := o.ctx.Tx.HasLabel(id, t)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

func (o *nodeIndexSeekOp) close() error { return nil }

// seekKeys returns the index keys to probe so the union of their results is a
// superset of v's eq3 matches, and whether a point probe is possible at all. A
// false return tells the caller to fall back to a full label scan.
//
// A number is probed under both numeric spellings so the cross-type case is
// covered: an integer i matches stored integers equal to i and stored floats equal
// to float64(i), so probe both; a float f matches stored floats equal to f and, when
// f is integral and small enough to round-trip through an int, stored integers equal
// to int64(f). A non-numeric scalar (string, bool, bytes) is its own single key. A
// non-scalar value, or an integral float beyond the exact integer range, has no
// safe finite probe set, so the caller scans.
func seekKeys(v value.Value) ([]value.Value, bool) {
	switch v.Type() {
	case value.TypeInt:
		i, _ := v.AsInt()
		return []value.Value{v, value.Float(float64(i))}, true
	case value.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
			return []value.Value{v}, true
		}
		if math.Abs(f) >= 1<<53 {
			// Beyond 2^53 several integers share one float, so int64(f) would miss its
			// siblings; scan instead of risking a non-superset probe.
			return nil, false
		}
		return []value.Value{v, value.Int(int64(f))}, true
	case value.TypeString, value.TypeBool, value.TypeBytes:
		return []value.Value{v}, true
	default:
		return nil, false
	}
}
