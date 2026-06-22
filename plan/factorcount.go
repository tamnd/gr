package plan

import (
	"strings"

	"github.com/tamnd/gr/ast"
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
	return ec
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
