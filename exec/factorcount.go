package exec

import (
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// expandCountOp is the executor for a factorized count (doc 11 §7, §8). It stands
// in for an Aggregate over an Expand: instead of building one row per edge and
// counting the rows, it counts the edges each input row expands to and emits one
// tally row. The count it produces is exactly the row count the Expand+Aggregate
// would have, because it applies the same per-edge filters the Expand applied (the
// type set and relationship-uniqueness against sibling edges) and the replaced
// expand carried no target-label, expand-into, or variable-length constraint (the
// rewrite only fires when those are absent), so every edge it counts is one the
// expand would have emitted a row for.
//
// Like a grouping-free aggregate it always emits exactly one row, the count zero
// included: an empty input, a null source on every row, or an all-unknown type set
// each yield a single row carrying zero, the empty-group rule the aggregate it
// replaced follows.
type expandCountOp struct {
	spec  *plan.ExpandCount
	input operator
	peers []string // sibling relationship variables a counted edge must differ from
	ctx   *Ctx

	cur  eval.Row // the input row whose edges are being counted
	done bool

	relTok engine.Token          // the single type token to expand, or 0 for all
	allow  map[engine.Token]bool // post-filter type set when more than one type
	noType bool                  // every named type is unknown: no edge matches
}

func (o *expandCountOp) open(ctx *Ctx) error {
	o.ctx, o.cur, o.done = ctx, nil, false
	o.relTok, o.allow, o.noType = resolveTypes(o.spec.Types)
	return o.input.open(ctx)
}

func (o *expandCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	if !o.noType {
		for {
			in, ok, err := o.input.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			n, err := o.countRow(in)
			if err != nil {
				return nil, false, err
			}
			total += n
		}
	}
	// One tally row stands in for total flat rows, so the factorization ratio is total: an
	// Expand+Aggregate would have built total rows to count, this built one (doc 20 §6.3).
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// countRow counts the edges one input row expands to, applying the type set and
// relationship-uniqueness filters the replaced Expand applied. A null source (an
// unmatched OPTIONAL variable) contributes nothing.
func (o *expandCountOp) countRow(in eval.Row) (int64, error) {
	src, ok := in[o.spec.From].AsNode()
	if !ok {
		return 0, nil
	}
	o.cur = in
	var n int64
	dir := toEngineDir(o.spec.Dir)
	err := o.ctx.Tx.Expand(engine.NodeID(src), o.relTok, dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.allow != nil && !o.allow[nb.Type] {
			return nil
		}
		if !o.unique(nb.Rel) {
			return nil
		}
		n++
		return nil
	})
	return n, err
}

// unique enforces relationship-uniqueness: a counted edge must not already be bound
// to a sibling relationship variable in the same pattern (doc 02 §4.3), the same
// check the Expand it replaced applied.
func (o *expandCountOp) unique(rel engine.RelID) bool {
	for _, p := range o.peers {
		if v, ok := o.cur[p]; ok && relValueContains(v, rel) {
			return false
		}
	}
	return true
}

func (o *expandCountOp) close() error { return o.input.close() }

// productCountOp is the executor for a factorized product count (doc 11 §7, §8): it
// counts the rows the cross-product of two or more independent expands from a shared
// source would produce, without building that product. For each input row it reads
// the source node and counts the matching edges along each leg, multiplies the
// per-leg degrees, and sums the product over the source rows. Because the legs leave
// the source along disjoint relationship types, no edge is counted by two legs and no
// relationship-uniqueness couples them, so the product is exactly the row count the
// naive plan's cross-product would have.
//
// Like a grouping-free aggregate it always emits exactly one row, zero included: an
// empty input or a source whose every leg has degree zero yields a single tally row.
type productCountOp struct {
	spec  *plan.ProductCount
	input operator
	ctx   *Ctx

	done bool
	legs []resolvedLeg
}

// resolvedLeg is one leg's expand parameters resolved once at open: the type token
// to expand (or zero for all), the multi-type allow set, whether the type set
// matches nothing, and the engine direction.
type resolvedLeg struct {
	relTok engine.Token
	allow  map[engine.Token]bool
	noType bool
	dir    engine.Direction
}

func (o *productCountOp) open(ctx *Ctx) error {
	o.ctx, o.done = ctx, false
	o.legs = make([]resolvedLeg, len(o.spec.Legs))
	for i, l := range o.spec.Legs {
		tok, allow, none := resolveTypes(l.Types)
		o.legs[i] = resolvedLeg{relTok: tok, allow: allow, noType: none, dir: toEngineDir(l.Dir)}
	}
	return o.input.open(ctx)
}

func (o *productCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	for {
		in, ok, err := o.input.next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			break
		}
		n, err := o.countRow(in)
		if err != nil {
			return nil, false, err
		}
		total += n
	}
	// One tally row stands in for the cross-product's total rows, so the ratio is total: the
	// naive plan would have built total product rows, this built one (doc 20 §6.3).
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// countRow returns the product of the source's per-leg degrees for one input row, a
// null or absent source contributing nothing. A leg whose type set matches nothing
// has degree zero, so the whole product is zero for that row.
func (o *productCountOp) countRow(in eval.Row) (int64, error) {
	src, ok := in[o.spec.From].AsNode()
	if !ok {
		return 0, nil
	}
	product := int64(1)
	for _, leg := range o.legs {
		if leg.noType {
			return 0, nil
		}
		var deg int64
		err := o.ctx.Tx.Expand(engine.NodeID(src), leg.relTok, leg.dir, func(nb engine.Neighbor) error {
			o.ctx.countScan(1)
			if leg.allow != nil && !leg.allow[nb.Type] {
				return nil
			}
			deg++
			return nil
		})
		if err != nil {
			return 0, err
		}
		if deg == 0 {
			return 0, nil
		}
		product *= deg
	}
	return product, nil
}

func (o *productCountOp) close() error { return o.input.close() }

// intersectCountOp is the executor for a factorized triangle count (doc 11 §7; doc
// 12 §5.2): it stands in for an Aggregate over an Intersect. Instead of building one
// row per closed triangle and counting the rows, it intersects the two legs' neighbor
// sets per input row and tallies the closings, emitting one count row. The tally is
// exactly the Intersect's row count, because it applies the same per-edge filters the
// Intersect applied (each leg's type set, relationship-uniqueness against the sibling
// edges bound below, and the apex's labels) and pairs a leg-0 edge with a leg-1 edge
// for the same apex exactly when the Intersect would have emitted that closing.
//
// The fast path keys leg-0's peer-unique edges by apex into a reusable integer map,
// then walks leg-1 adding, for each apex both legs reach, leg-0's per-apex count: the
// product of the two legs' degrees at that apex, summed over apexes. The map is reused
// across input rows, so the per-row intersection allocates nothing once it is warm,
// the whole point of counting instead of materializing. The rare row whose two legs
// leave the same node is handled edge by edge so an edge is never paired with itself.
//
// Like a grouping-free aggregate it always emits exactly one row, zero included.
type intersectCountOp struct {
	spec  *plan.IntersectCount
	input operator
	peers []string // sibling relationship variables a counted edge must differ from
	ctx   *Ctx

	cur  eval.Row // the input row whose closings are being counted
	done bool

	leg  [2]intersectLeg
	seen map[engine.NodeID]int64 // reusable: apex node -> peer-unique leg-0 edge count
}

func (o *intersectCountOp) open(ctx *Ctx) error {
	o.ctx, o.cur, o.done = ctx, nil, false
	for i := range o.spec.Legs {
		tok, allow, none := resolveTypes(o.spec.Legs[i].Types)
		o.leg[i] = intersectLeg{tok: tok, allow: allow, noType: none, dir: toEngineDir(o.spec.Legs[i].Dir)}
	}
	if o.seen == nil {
		o.seen = map[engine.NodeID]int64{}
	}
	return o.input.open(ctx)
}

func (o *intersectCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	if !o.leg[0].noType && !o.leg[1].noType {
		for {
			in, ok, err := o.input.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			n, err := o.countRow(in)
			if err != nil {
				return nil, false, err
			}
			total += n
		}
	}
	// One tally row stands in for total closing rows, so the factorization ratio is total:
	// the Intersect+Aggregate would have built total rows to count, this built one.
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// countRow counts the closings one input row contributes. With the two legs leaving
// distinct nodes (the common case the rewrite's distinct-variable guard makes the
// rule on simple graphs), a leg-0 edge and a leg-1 edge are always distinct
// relationships, so the count is the per-apex product of the two legs' degrees. When
// the two legs leave the same node it falls back to an edge-aware count so an edge is
// never paired with itself.
func (o *intersectCountOp) countRow(in eval.Row) (int64, error) {
	from0, ok := in[o.spec.Legs[0].From].AsNode()
	if !ok {
		return 0, nil
	}
	from1, ok := in[o.spec.Legs[1].From].AsNode()
	if !ok {
		return 0, nil
	}
	o.cur = in
	if engine.NodeID(from0) == engine.NodeID(from1) {
		return o.countSameSource(engine.NodeID(from0))
	}

	clear(o.seen)
	err := o.ctx.Tx.Expand(engine.NodeID(from0), o.leg[0].tok, o.leg[0].dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.accept(0, nb) && o.unique(nb.Rel) {
			o.seen[nb.Node]++
		}
		return nil
	})
	if err != nil || len(o.seen) == 0 {
		return 0, err
	}
	var n int64
	err = o.ctx.Tx.Expand(engine.NodeID(from1), o.leg[1].tok, o.leg[1].dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if !o.accept(1, nb) || !o.unique(nb.Rel) {
			return nil
		}
		c := o.seen[nb.Node]
		if c == 0 {
			return nil
		}
		ok, err := o.hasLabels(nb.Node)
		if err != nil || !ok {
			return err
		}
		n += c
		return nil
	})
	return n, err
}

// countSameSource counts closings for the rare input row whose two legs leave the
// same node, where a single edge can satisfy both legs and so must not be paired with
// itself. It keeps each apex's leg-0 edge ids, then for every leg-1 edge counts the
// leg-0 edges to that apex that are a different relationship, the edge-level form of
// the same per-apex product. It allocates a fresh map only on this cold path, so the
// hot path stays allocation-free.
func (o *intersectCountOp) countSameSource(src engine.NodeID) (int64, error) {
	byApex := map[engine.NodeID][]engine.RelID{}
	err := o.ctx.Tx.Expand(src, o.leg[0].tok, o.leg[0].dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.accept(0, nb) && o.unique(nb.Rel) {
			byApex[nb.Node] = append(byApex[nb.Node], nb.Rel)
		}
		return nil
	})
	if err != nil || len(byApex) == 0 {
		return 0, err
	}
	var n int64
	err = o.ctx.Tx.Expand(src, o.leg[1].tok, o.leg[1].dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if !o.accept(1, nb) || !o.unique(nb.Rel) {
			return nil
		}
		rels := byApex[nb.Node]
		if len(rels) == 0 {
			return nil
		}
		ok, err := o.hasLabels(nb.Node)
		if err != nil || !ok {
			return err
		}
		for _, r0 := range rels {
			if r0 != nb.Rel {
				n++
			}
		}
		return nil
	})
	return n, err
}

// accept applies a leg's multi-type post-filter, the same trim intersectOp does.
func (o *intersectCountOp) accept(leg int, nb engine.Neighbor) bool {
	return o.leg[leg].allow == nil || o.leg[leg].allow[nb.Type]
}

// unique enforces relationship-uniqueness against the sibling relationship variables
// bound below the count, the same check the Intersect's emit applied to each leg edge.
func (o *intersectCountOp) unique(rel engine.RelID) bool {
	for _, p := range o.peers {
		if v, ok := o.cur[p]; ok && relValueContains(v, rel) {
			return false
		}
	}
	return true
}

// hasLabels reports whether an apex node carries every label the pattern required of
// it, the same constraint the Intersect's emit applied before binding the apex.
func (o *intersectCountOp) hasLabels(id engine.NodeID) (bool, error) {
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

func (o *intersectCountOp) close() error { return o.input.close() }
