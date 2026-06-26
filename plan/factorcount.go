package plan

import (
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// FactorizeCount rewrites a grouping-free count(*) directly over a plain expand
// into an ExpandCount, the factorized count operator (doc 11 §7, §8; ADR-8). The
// naive plan for `MATCH (a)-[r]->(b) RETURN count(*)` scans a, expands to one row
// per edge, then counts the rows; the rewrite counts the edges each source row
// expands to without ever building those rows, turning a fan-out-heavy traversal
// into a tally. It is a structural rewrite, not a cost decision: the factorized
// form is never worse than enumerating, so it fires whenever its shape matches.
//
// The pass recurses through the whole tree (a count(*) can sit under a Sort or
// Limit, or inside a UNION arm), rewriting each Aggregate whose shape it matches
// and leaving every other operator untouched.
func FactorizeCount(o Op) Op {
	if o == nil {
		return nil
	}
	o = mapChildren(o, FactorizeCount)
	agg, ok := o.(*Aggregate)
	if !ok {
		return o
	}
	if ec := factorizeAgg(agg); ec != nil {
		if pc := factorizeProduct(ec); pc != nil {
			return pc
		}
		return ec
	}
	if ic := factorizeIntersect(agg); ic != nil {
		return ic
	}
	return o
}

// factorizeIntersect returns the IntersectCount an Aggregate rewrites to, or nil when
// the aggregate does not match: a single grouping-free, non-distinct count(*) over an
// Intersect, the closing of a WCOJ triangle (doc 11 §7; doc 12 §5.2), possibly under a
// chain of Filters carrying a post-closing constraint on the apex. Those guards make
// the count of closings the intersection finds exactly the row count the
// Intersect+Aggregate would have produced, so the tally stands in for the materialized
// closings. The Intersect already carries the apex labels and the two legs the count
// must respect, so the rewrite copies them across unchanged, and any peeled apex filter
// rides along as the apex predicate the count evaluates once per closing apex.
func factorizeIntersect(agg *Aggregate) *IntersectCount {
	if len(agg.GroupKeys) != 0 || len(agg.Aggs) != 1 || agg.Distinct {
		return nil
	}
	col := agg.Aggs[0]
	if !isCountStar(col.Expr) {
		return nil
	}
	// A Filter can sit between the count and the Intersect: the post-closing
	// constraint on the apex, the undirected triangle's `id(b) < id(c)` ordering that
	// counts each triangle once. Peel the whole chain (the optimizer may leave more
	// than one) and carry the conjunction as the apex predicate, evaluated per apex
	// the legs share instead of by building a row to filter.
	cur := agg.Input
	var apex ast.Expr
	for {
		f, ok := cur.(*Filter)
		if !ok {
			break
		}
		apex = andPred(apex, f.Pred)
		cur = f.Input
	}
	in, ok := cur.(*Intersect)
	if !ok {
		return nil
	}
	// Absorbing the apex predicate into the per-apex count is sound only when its
	// truth is constant across that apex's edge pairs, so it must not read either
	// leg's relationship variable, whose value differs per closing edge. A predicate
	// that does is left as a Filter over the materialized Intersect (no factorization).
	if apex != nil && readsLegRel(apex, in) {
		return nil
	}
	return &IntersectCount{
		Input:    in.Input,
		Var:      in.Var,
		Labels:   in.Labels,
		Legs:     in.Legs,
		Col:      col.Name,
		ApexPred: apex,
	}
}

// andPred conjoins two predicates, returning the second when the first is nil so a
// chain of filters folds into one AND tree.
func andPred(a, b ast.Expr) ast.Expr {
	if a == nil {
		return b
	}
	return &ast.Binary{Op: ast.OpAnd, L: a, R: b}
}

// readsLegRel reports whether a predicate references either of an Intersect's leg
// relationship variables, the variables whose value varies per closing edge and so
// would make a per-apex gate unsound.
func readsLegRel(e ast.Expr, in *Intersect) bool {
	vars := freeVars(e)
	return vars[in.Legs[0].Rel] || vars[in.Legs[1].Rel]
}

// FusePolygonAnchor recognizes the IntersectCount shape the fused cyclic count drives
// directly through the engine SPI, returning the anchor scan, the anchor expand chain
// in scan-to-apex order, and the predicate of any Filter that sat between the count's
// input and that chain (the undirected triangle's `id(a) < id(b)` ordering), or nil for
// the scan when the shape does not match.
//
// The shape is a simple path of plain Expands (no variable length, no expand-into) over
// a NodeScan, possibly under a chain of Filters, whose two closing legs leave exactly
// the path's two ends, the scan root and the last hop's target. The triangle is the one
// hop case (anchor a->b, legs from a and b); the directed four-cycle is the two hop case
// (anchor a->b->c, legs from a and c closing at the apex d). The middle nodes of a longer
// path are pure structure, neither closing leg leaves them, so the fused operator binds
// them only to walk through. The anchor predicate is evaluated with the path nodes bound
// but no relationship the operator never builds, so it must not read any hop's
// relationship variable; one that does keeps the general path, the Filter riding along in
// the input pipeline.
func FusePolygonAnchor(x *IntersectCount) (*NodeScan, []*Expand, ast.Expr) {
	cur := x.Input
	var anchor ast.Expr
	for {
		f, ok := cur.(*Filter)
		if !ok {
			break
		}
		anchor = andPred(anchor, f.Pred)
		cur = f.Input
	}
	// Walk the Expand chain from the apex end down to the scan, collecting hops in
	// reverse, then flip them into scan-to-apex order. Each hop must be plain and chain
	// onto the one below it (its From is the previous hop's To), so the bound path is a
	// simple a -> b -> c -> ... walk.
	var rev []*Expand
	for {
		ex, ok := cur.(*Expand)
		if !ok || ex.VarLen != nil || ex.ToBound {
			break
		}
		if n := len(rev); n > 0 && rev[n-1].From != ex.To {
			return nil, nil, nil
		}
		rev = append(rev, ex)
		cur = ex.Input
	}
	ns, ok := cur.(*NodeScan)
	if !ok || len(rev) == 0 {
		return nil, nil, nil
	}
	hops := make([]*Expand, len(rev))
	for i, ex := range rev {
		hops[len(rev)-1-i] = ex
	}
	if ns.Var != hops[0].From {
		return nil, nil, nil
	}
	// The two closing legs must leave exactly the path's two ends: the scan root and the
	// last hop's target. A leg leaving a middle node is a different join shape the fused
	// path does not handle.
	root, apex := hops[0].From, hops[len(hops)-1].To
	l0, l1 := x.Legs[0].From, x.Legs[1].From
	if !((l0 == root && l1 == apex) || (l0 == apex && l1 == root)) {
		return nil, nil, nil
	}
	if anchor != nil {
		vars := freeVars(anchor)
		for _, ex := range hops {
			if vars[ex.Rel] {
				return nil, nil, nil
			}
		}
	}
	return ns, hops, anchor
}

// factorizeProduct turns an ExpandCount whose input is one or more further plain
// expands from the same source into a ProductCount, the count of independent
// fan-outs from a shared anchor (the recommendation shape, doc 11 §7). It peels
// the chain below the count: each operator must be a plain expand leaving the same
// source variable as the count, with a known type set disjoint from every leg
// already collected, so no edge matches two legs and no relationship-uniqueness
// couples them. The peel stops at the first operator that is not such an expand,
// which becomes the product's shared input. It returns nil (keep the ExpandCount)
// unless it collected at least two legs over an input that binds no relationship a
// leg would have to stay distinct from.
func factorizeProduct(ec *ExpandCount) *ProductCount {
	legs := []ProductLeg{{Types: ec.Types, Dir: ec.Dir}}
	seen := [][]bind.NameRef{ec.Types}
	cur := ec.Input
	for {
		ex, ok := cur.(*Expand)
		if !ok || ex.From != ec.From {
			break
		}
		if ex.VarLen != nil || ex.ToBound || len(ex.ToLabels) != 0 {
			break
		}
		if !disjointFromAll(ex.Types, seen) {
			break
		}
		legs = append(legs, ProductLeg{Types: ex.Types, Dir: ex.Dir})
		seen = append(seen, ex.Types)
		cur = ex.Input
	}
	if len(legs) < 2 || bindsRelVar(cur) {
		return nil
	}
	return &ProductCount{Input: cur, From: ec.From, Legs: legs, Col: ec.Col}
}

// disjointFromAll reports whether a type set is all-known and shares no type with
// any already-collected set. An empty set is the type wildcard, which overlaps
// every type, and an unknown type cannot be proven distinct, so either makes the
// set non-disjoint and stops the peel; only a concrete, provably non-overlapping
// leg joins the product.
func disjointFromAll(types []bind.NameRef, seen [][]bind.NameRef) bool {
	if len(types) == 0 {
		return false
	}
	for _, t := range types {
		if !t.Known {
			return false
		}
		for _, s := range seen {
			for _, u := range s {
				if u.Known && u.Token == t.Token {
					return false
				}
			}
		}
	}
	return true
}

// bindsRelVar reports whether an operator subtree binds any relationship variable.
// A ProductCount counts its legs without a uniqueness check, sound only when no
// edge a leg counts could already be bound on the input row, so the rewrite folds
// the product only over an input that binds no relationship at all (the anchor
// scan and its filters, the common shape).
func bindsRelVar(o Op) bool {
	switch o.(type) {
	case *Expand, *Intersect, *ShortestPath, *ExpandCount, *ProductCount:
		return true
	}
	for _, c := range nodeChildren(o) {
		if bindsRelVar(c) {
			return true
		}
	}
	return false
}

// factorizeAgg returns the ExpandCount an Aggregate rewrites to, or nil when the
// aggregate does not match the factorizable shape: a single count(*) aggregate, no
// grouping keys, not distinct, whose direct input is a plain expand (no
// variable length, no expand-into, no target-label constraint). Those guards make
// the count of matching edges exactly the row count the aggregate would produce.
func factorizeAgg(agg *Aggregate) *ExpandCount {
	if len(agg.GroupKeys) != 0 || len(agg.Aggs) != 1 || agg.Distinct {
		return nil
	}
	col := agg.Aggs[0]
	if !isCountStar(col.Expr) {
		return nil
	}
	ex, ok := agg.Input.(*Expand)
	if !ok {
		return nil
	}
	if ex.VarLen != nil || ex.ToBound || len(ex.ToLabels) != 0 {
		return nil
	}
	return &ExpandCount{
		Input: ex.Input,
		From:  ex.From,
		Rel:   ex.Rel,
		Types: ex.Types,
		Dir:   ex.Dir,
		Col:   col.Name,
	}
}

// isCountStar reports whether an expression is exactly count(*): the count
// function, the star form, not distinct (count(DISTINCT *) is not a thing, but the
// guard keeps the rewrite honest). count(x) is excluded because it skips null x,
// so its tally is not the row count.
func isCountStar(e ast.Expr) bool {
	fc, ok := e.(*ast.FunctionCall)
	if !ok {
		return false
	}
	return strings.EqualFold(fc.Name, "count") && fc.Star && !fc.Distinct
}
