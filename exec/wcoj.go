package exec

import (
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// intersectOp executes a plan.Intersect: per input row it produces the apex nodes
// adjacent to both bound endpoints as the intersection of their neighbor sets, the
// worst-case-optimal join for a triangle (doc 12 §5.2). It builds a map of one
// endpoint's neighbors keyed by node, then walks the other endpoint's neighbors and
// emits a row for each that lands in the map, so it never materializes the full
// fan-out of either side the way the binary expand-into plan does.
//
// The rows it emits are exactly the binary plan's: the same (apex, leg-0 edge, leg-1
// edge) triples, with the same relationship-uniqueness and apex-label constraints
// applied, so swapping the binary plan for this operator is meaning-preserving. The
// emission order differs, which an unordered Cypher result does not observe.
type intersectOp struct {
	spec  *plan.Intersect
	input operator
	peers []string // earlier relationship variables both legs must differ from
	ctx   *Ctx

	leg [2]intersectLeg
	out []eval.Row
	pos int
}

// intersectLeg is one leg's resolved expansion: the engine type token and direction
// to walk, with an optional post-filter for a multi-type set, mirroring expandOp.
type intersectLeg struct {
	tok    engine.Token
	allow  map[engine.Token]bool
	noType bool
	dir    engine.Direction
}

func (o *intersectOp) open(ctx *Ctx) error {
	o.ctx, o.out, o.pos = ctx, nil, 0
	for i := range o.spec.Legs {
		tok, allow, none := resolveTypes(o.spec.Legs[i].Types)
		o.leg[i] = intersectLeg{tok: tok, allow: allow, noType: none, dir: toEngineDir(o.spec.Legs[i].Dir)}
	}
	return o.input.open(ctx)
}

func (o *intersectOp) next() (eval.Row, bool, error) {
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
		if err := o.build(in); err != nil {
			return nil, false, err
		}
	}
}

// build computes the apex matches for one input row and stages them in o.out. A leg
// whose every named type is unknown matches nothing, and a null endpoint (an
// unmatched OPTIONAL variable) expands to nothing, so either short-circuits to no
// rows.
func (o *intersectOp) build(in eval.Row) error {
	o.out, o.pos = o.out[:0], 0
	if o.leg[0].noType || o.leg[1].noType {
		return nil
	}
	from0, ok := in[o.spec.Legs[0].From].AsNode()
	if !ok {
		return nil
	}
	from1, ok := in[o.spec.Legs[1].From].AsNode()
	if !ok {
		return nil
	}
	// Build the first leg's neighbors keyed by apex node, so the second leg's walk can
	// probe for the intersection. Keying on the smaller side would touch fewer entries,
	// a refinement the cost model already biases by costing the smaller degree; the set
	// of matches is the same whichever side builds.
	side := map[engine.NodeID][]engine.Neighbor{}
	err := o.ctx.Tx.Expand(engine.NodeID(from0), o.leg[0].tok, o.leg[0].dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.accept(0, nb) {
			side[nb.Node] = append(side[nb.Node], nb)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(side) == 0 {
		return nil
	}
	return o.ctx.Tx.Expand(engine.NodeID(from1), o.leg[1].tok, o.leg[1].dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if !o.accept(1, nb) {
			return nil
		}
		for _, nb0 := range side[nb.Node] {
			if err := o.emit(in, nb0, nb); err != nil {
				return err
			}
		}
		return nil
	})
}

// accept applies a leg's multi-type post-filter, the same trim expandOp does when a
// leg names more than one known type.
func (o *intersectOp) accept(leg int, nb engine.Neighbor) bool {
	return o.leg[leg].allow == nil || o.leg[leg].allow[nb.Type]
}

// emit stages one apex match: it enforces relationship-uniqueness (both leg edges
// distinct from each other and from every earlier-bound relationship) and the apex's
// labels, then binds the apex node and the two leg relationships into a fresh row.
func (o *intersectOp) emit(in eval.Row, nb0, nb1 engine.Neighbor) error {
	if nb0.Rel == nb1.Rel || !o.unique(in, nb0.Rel) || !o.unique(in, nb1.Rel) {
		return nil
	}
	ok, err := o.hasLabels(nb0.Node)
	if err != nil || !ok {
		return err
	}
	row := cloneRow(in)
	row[o.spec.Var] = value.Node(uint64(nb0.Node))
	row[o.spec.Legs[0].Rel] = value.Rel(uint64(nb0.Rel))
	row[o.spec.Legs[1].Rel] = value.Rel(uint64(nb1.Rel))
	o.out = append(o.out, row)
	return nil
}

// unique reports whether a relationship is not already bound to an earlier sibling
// variable, the relationship-uniqueness rule the binary plan's expands enforce step
// by step (doc 02 §4.3).
func (o *intersectOp) unique(in eval.Row, rel engine.RelID) bool {
	for _, p := range o.peers {
		if v, ok := in[p]; ok && relValueContains(v, rel) {
			return false
		}
	}
	return true
}

func (o *intersectOp) hasLabels(id engine.NodeID) (bool, error) {
	for _, l := range o.spec.Labels {
		if !l.Known {
			return false, nil
		}
		has, err := o.ctx.Tx.HasLabel(id, l.Token)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

func (o *intersectOp) close() error { return o.input.close() }
