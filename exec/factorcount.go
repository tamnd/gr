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

// intersectCountOp is the executor for a fused triangle count (doc 11 §5, §7; doc 12
// §5.2). It stands in for an Aggregate over a WCOJ Intersect: instead of feeding the
// intersection from a materialized mid expand and counting one apex row at a time, it
// drives the mid expand itself and merge-counts the apex matches per middle node, so
// it builds neither a mid row nor an apex row. For each anchor it reads the hub leg's
// apex candidates once (they depend only on the anchor), expands the anchor along the
// mid edge to enumerate the middle node, and for each middle node merge-intersects its
// mid-leg neighbors with the hub candidates on dense position, tallying every apex the
// two reach where the three triangle edges are pairwise distinct and the apex carries
// the required labels. The tally equals the row count the Intersect+Aggregate would
// produce, because it applies the same per-leg type filters, the same
// relationship-uniqueness, and the same apex labels the Intersect applied.
//
// Like a grouping-free aggregate it always emits exactly one row, zero included.
type intersectCountOp struct {
	spec  *plan.IntersectCount
	input operator
	ctx   *Ctx

	done bool

	midTok    engine.Token          // the mid expand's single type token, or 0 for all
	midAllow  map[engine.Token]bool // mid expand post-filter when more than one type
	midNoType bool                  // the mid expand's every named type is unknown
	midDir    engine.Direction

	hub resolvedLeg // the leg reaching the apex from the anchor
	mid resolvedLeg // the leg reaching the apex from the middle node

	// merge-intersection scratch: the hub candidates (read once per anchor) and the
	// mid-leg neighbors (read once per middle node), each a distinct buffer so the
	// hub list stays valid while the mid list is refilled (engine.Adjacency contract).
	adjx   engine.Adjacency
	bufHub []engine.PosNeighbor
	bufMid []engine.PosNeighbor
}

func (o *intersectCountOp) open(ctx *Ctx) error {
	o.ctx, o.done = ctx, false
	o.midTok, o.midAllow, o.midNoType = resolveTypes(o.spec.MidTypes)
	htok, hallow, hnone := resolveTypes(o.spec.HubLeg.Types)
	o.hub = resolvedLeg{relTok: htok, allow: hallow, noType: hnone, dir: toEngineDir(o.spec.HubLeg.Dir)}
	mtok, mallow, mnone := resolveTypes(o.spec.MidLeg.Types)
	o.mid = resolvedLeg{relTok: mtok, allow: mallow, noType: mnone, dir: toEngineDir(o.spec.MidLeg.Dir)}
	o.midDir = toEngineDir(o.spec.MidDir)
	o.adjx, _ = ctx.Tx.(engine.Adjacency)
	return o.input.open(ctx)
}

func (o *intersectCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	if !o.midNoType && !o.hub.noType && !o.mid.noType {
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
	// One tally row stands in for total flat rows, so the factorization ratio is total:
	// the Intersect+Aggregate would have built total apex rows to count, this built one
	// (doc 20 §6.3).
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// countRow counts the triangles closed at one anchor row, a null or absent anchor
// contributing nothing. It takes the merge-intersection path when the engine exposes
// position-sorted adjacency, falling back to a hash probe otherwise.
func (o *intersectCountOp) countRow(in eval.Row) (int64, error) {
	a, ok := in[o.spec.Hub].AsNode()
	if !ok {
		return 0, nil
	}
	if o.adjx != nil {
		return o.countMerge(engine.NodeID(a))
	}
	return o.countHash(engine.NodeID(a))
}

// countMerge reads the anchor's hub candidates once, drives the mid expand to
// enumerate the middle node, and for each middle node merge-intersects its mid-leg
// neighbors with the hub candidates on dense position. It never materializes an apex
// row: each apex match is tallied in place.
func (o *intersectCountOp) countMerge(a engine.NodeID) (int64, error) {
	hub, err := o.adjx.NeighborsByPos(a, o.hub.relTok, o.hub.dir, o.bufHub)
	if err != nil {
		return 0, err
	}
	o.bufHub = hub
	if len(hub) == 0 {
		return 0, nil
	}
	o.ctx.countScan(int64(len(hub)))
	var total int64
	err = o.ctx.Tx.Expand(a, o.midTok, o.midDir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.midAllow != nil && !o.midAllow[nb.Type] {
			return nil
		}
		mid, e := o.adjx.NeighborsByPos(nb.Node, o.mid.relTok, o.mid.dir, o.bufMid)
		if e != nil {
			return e
		}
		o.bufMid = mid
		if len(mid) == 0 {
			return nil
		}
		o.ctx.countScan(int64(len(mid)))
		n, e := o.mergeCount(hub, mid, nb.Rel)
		if e != nil {
			return e
		}
		total += n
		return nil
	})
	return total, err
}

// mergeCount walks the hub candidates and a middle node's mid-leg neighbors, both
// sorted ascending by dense position, with two cursors and tallies every apex they
// both reach (doc 12 §5.2). Equal positions denote the same apex; a multigraph can
// have several edges to it on each side, so the matching runs are paired as a small
// Cartesian product, counting only pairs whose three edges (the mid expand edge rel1
// and the two leg edges) are pairwise distinct, for an apex carrying the labels.
func (o *intersectCountOp) mergeCount(hub, mid []engine.PosNeighbor, rel1 engine.RelID) (int64, error) {
	var total int64
	i, j := 0, 0
	for i < len(hub) && j < len(mid) {
		switch {
		case hub[i].Pos < mid[j].Pos:
			i++
		case hub[i].Pos > mid[j].Pos:
			j++
		default:
			p := hub[i].Pos
			i0, j0 := i, j
			for i < len(hub) && hub[i].Pos == p {
				i++
			}
			for j < len(mid) && mid[j].Pos == p {
				j++
			}
			ok, err := o.hasLabels(hub[i0].Node)
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			for _, x := range hub[i0:i] {
				if o.hub.allow != nil && !o.hub.allow[x.Type] {
					continue
				}
				if x.Rel == rel1 {
					continue
				}
				for _, y := range mid[j0:j] {
					if o.mid.allow != nil && !o.mid.allow[y.Type] {
						continue
					}
					if y.Rel == rel1 || y.Rel == x.Rel {
						continue
					}
					total++
				}
			}
		}
	}
	return total, nil
}

// countHash is the fallback when the engine exposes no position-sorted adjacency: it
// keys the anchor's hub candidates by apex node, then drives the mid expand and, per
// middle node, probes the hub map with its mid-leg neighbors. The set of triangles it
// tallies is the same the merge path tallies; only the access path differs.
func (o *intersectCountOp) countHash(a engine.NodeID) (int64, error) {
	hub := map[engine.NodeID][]engine.RelID{}
	err := o.ctx.Tx.Expand(a, o.hub.relTok, o.hub.dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.hub.allow != nil && !o.hub.allow[nb.Type] {
			return nil
		}
		hub[nb.Node] = append(hub[nb.Node], nb.Rel)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(hub) == 0 {
		return 0, nil
	}
	var total int64
	err = o.ctx.Tx.Expand(a, o.midTok, o.midDir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.midAllow != nil && !o.midAllow[nb.Type] {
			return nil
		}
		rel1 := nb.Rel
		return o.ctx.Tx.Expand(nb.Node, o.mid.relTok, o.mid.dir, func(mb engine.Neighbor) error {
			o.ctx.countScan(1)
			if o.mid.allow != nil && !o.mid.allow[mb.Type] {
				return nil
			}
			if mb.Rel == rel1 {
				return nil
			}
			hrels := hub[mb.Node]
			if len(hrels) == 0 {
				return nil
			}
			ok, err := o.hasLabels(mb.Node)
			if err != nil || !ok {
				return err
			}
			for _, hr := range hrels {
				if hr == rel1 || hr == mb.Rel {
					continue
				}
				total++
			}
			return nil
		})
	})
	return total, err
}

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
