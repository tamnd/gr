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
	ec := factorizeAgg(agg)
	if ec == nil {
		return o
	}
	if pc := factorizeProduct(ec); pc != nil {
		return pc
	}
	return ec
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
