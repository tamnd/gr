package exec

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// varExpandOp is the variable-length expand: a bounded traversal from each
// source node producing one row per relationship-unique path (a trail) whose
// hop count falls in the pattern's range (doc 12 §4.3, doc 09 §3.4). The
// relationship variable binds the path's relationship list (a value.List of
// value.Rel), not a single relationship; the reached node binds To unless it was
// already bound (an expand-into). Relationship-uniqueness (doc 09 §3.5) bounds
// even an unbounded * : no relationship repeats within a path, so a path is at
// most as long as the graph has edges and the traversal terminates.
type varExpandOp struct {
	spec  *plan.Expand
	input operator
	peers []string // sibling relationship variables a path's edges must avoid
	ctx   *Ctx

	relTok engine.Token
	allow  map[engine.Token]bool
	noType bool
	min    int // normalized lower bound (0 includes the zero-hop path)
	max    int // normalized upper bound, or -1 for unbounded

	out []eval.Row // the paths enumerated for the current input row
	pos int
}

func (o *varExpandOp) open(ctx *Ctx) error {
	o.ctx, o.out, o.pos = ctx, nil, 0
	o.relTok, o.allow, o.noType = resolveTypes(o.spec.Types)
	o.min, o.max = normVarLen(o.spec.VarLen)
	return o.input.open(ctx)
}

// normVarLen resolves the omitted bounds the binder left as -1: the lower bound
// defaults to one hop, the upper bound to unbounded (doc 09 §3.4).
func normVarLen(v *ast.VarLength) (min, max int) {
	min, max = v.Min, v.Max
	if min < 0 {
		min = 1
	}
	// max < 0 stays -1, the unbounded sentinel.
	return min, max
}

func (o *varExpandOp) next() (eval.Row, bool, error) {
	for {
		if o.pos < len(o.out) {
			row := o.out[o.pos]
			o.pos++
			return row, true, nil
		}
		in, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		if err := o.loadPaths(in); err != nil {
			return nil, false, err
		}
	}
}

// loadPaths enumerates every matching trail from the input row's source into the
// output buffer. A null source (an unmatched OPTIONAL variable) yields nothing.
func (o *varExpandOp) loadPaths(row eval.Row) error {
	o.out, o.pos = o.out[:0], 0
	srcV, ok := row[o.spec.From].AsNode()
	if !ok {
		return nil
	}
	// One source position is being expanded recursively, the variable-length traversal rate (doc 20
	// §6.1). Counted here, where a real source commits to a walk, the same point a fixed expand counts.
	o.ctx.countVarLenExpand(o.relTok)
	if o.noType && o.min > 0 {
		// No type can match, so only the zero-hop path (if allowed) survives.
		return o.emitZeroHopOnly(row, engine.NodeID(srcV))
	}
	if o.spec.ToBound {
		if _, ok := row[o.spec.To].AsNode(); !ok {
			return nil // the expand-into target is unbound (null): no path matches
		}
	}
	forbidden := collectPeerRels(row, o.peers)
	var rels []engine.RelID
	var walk func(node engine.NodeID, depth int) error
	walk = func(node engine.NodeID, depth int) error {
		if depth >= o.min {
			if err := o.emit(row, node, rels); err != nil {
				return err
			}
		}
		if (o.max >= 0 && depth >= o.max) || o.noType {
			return nil
		}
		return o.ctx.Tx.Expand(node, o.relTok, toEngineDir(o.spec.Dir), func(nb engine.Neighbor) error {
			o.ctx.countScan(1)
			if o.allow != nil && !o.allow[nb.Type] {
				return nil
			}
			if forbidden[nb.Rel] || containsRel(rels, nb.Rel) {
				return nil
			}
			rels = append(rels, nb.Rel)
			err := walk(nb.Node, depth+1)
			rels = rels[:len(rels)-1]
			return err
		})
	}
	return walk(engine.NodeID(srcV), 0)
}

// emitZeroHopOnly handles the degenerate case where no relationship type can
// match but the range still admits the zero-hop path.
func (o *varExpandOp) emitZeroHopOnly(row eval.Row, src engine.NodeID) error {
	if o.min == 0 {
		return o.emit(row, src, nil)
	}
	return nil
}

// emit appends one result row for a path that reaches node over rels, applying
// the target-label and expand-into constraints. The relationship variable binds
// the path's relationship list; the reached node binds To unless it was already
// bound.
func (o *varExpandOp) emit(row eval.Row, node engine.NodeID, rels []engine.RelID) error {
	if o.spec.ToBound {
		want, ok := row[o.spec.To].AsNode()
		if !ok || engine.NodeID(want) != node {
			return nil
		}
	}
	ok, err := hasAllLabels(o.ctx.Tx, node, o.spec.ToLabels)
	if err != nil || !ok {
		return err
	}
	out := cloneRow(row)
	out[o.spec.Rel] = relList(rels)
	if !o.spec.ToBound {
		out[o.spec.To] = value.Node(uint64(node))
	}
	o.out = append(o.out, out)
	// One path of len(rels) hops reached, the depth distribution whose deep tail is an expensive
	// traversal (doc 20 §6.1). Observed per emitted path, after the path passes its constraints.
	o.ctx.countVarLenDepth(o.relTok, len(rels))
	return nil
}

func (o *varExpandOp) close() error { return o.input.close() }

// relList builds the relationship-list binding for a variable-length path: a
// value.List of value.Rel in traversal order.
func relList(rels []engine.RelID) value.Value {
	vs := make([]value.Value, len(rels))
	for i, r := range rels {
		vs[i] = value.Rel(uint64(r))
	}
	return value.List(vs...)
}

func containsRel(rels []engine.RelID, r engine.RelID) bool {
	for _, x := range rels {
		if x == r {
			return true
		}
	}
	return false
}

// hasAllLabels reports whether a node carries every label in the set. An unknown
// label (never interned) matches nothing.
func hasAllLabels(tx engine.Tx, id engine.NodeID, labels []bind.NameRef) (bool, error) {
	for _, l := range labels {
		if !l.Known {
			return false, nil
		}
		has, err := tx.HasLabel(id, l.Token)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

// collectPeerRels gathers the relationship ids already bound by sibling
// relationship variables in the same pattern, so a path can avoid reusing them
// (relationship-uniqueness across the whole pattern, doc 02 §4.3). A sibling may
// be a single relationship (a fixed hop) or a list (another variable-length
// step), so both shapes are flattened.
func collectPeerRels(row eval.Row, peers []string) map[engine.RelID]bool {
	set := map[engine.RelID]bool{}
	for _, p := range peers {
		if v, ok := row[p]; ok {
			addRelIDs(set, v)
		}
	}
	return set
}

func addRelIDs(set map[engine.RelID]bool, v value.Value) {
	if r, ok := v.AsRel(); ok {
		set[engine.RelID(r)] = true
		return
	}
	if lst, ok := v.AsList(); ok {
		for _, e := range lst {
			addRelIDs(set, e)
		}
	}
}

// relValueContains reports whether a binding (a single relationship or a
// variable-length path's list) already holds a relationship id, the
// uniqueness check a single-hop expand makes against each sibling.
func relValueContains(v value.Value, r engine.RelID) bool {
	if rid, ok := v.AsRel(); ok {
		return engine.RelID(rid) == r
	}
	if lst, ok := v.AsList(); ok {
		for _, e := range lst {
			if relValueContains(e, r) {
				return true
			}
		}
	}
	return false
}
