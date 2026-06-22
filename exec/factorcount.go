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
