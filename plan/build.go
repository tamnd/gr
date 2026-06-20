package plan

import (
	"sort"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// Build turns a bound query into a raw (un-normalized) logical operator tree
// (doc 10 §6). The tree is a correct, mechanical lowering of the clause
// pipeline: a MATCH becomes scans and expands, a WHERE a filter, a WITH/RETURN a
// projection or aggregation, in clause order. It is not optimized — that is
// [Normalize] (canonicalization) and the cost-based planner's job.
func Build(b *bind.Bound) Op {
	bd := &builder{b: b}
	left := bd.single(b.Query.First)
	for _, tail := range b.Query.Rest {
		right := bd.single(tail.Query)
		left = &Union{Left: left, Right: right, All: tail.All}
	}
	return left
}

// Plan builds and then normalizes: the pipeline's full logical-planning output,
// the canonical tree handed to the cost-based planner (doc 10 §8.1).
func Plan(b *bind.Bound) Op { return Normalize(Build(b)) }

// builder carries the per-build state: the bound query (for resolved tokens) and
// the counter that names anonymous pattern elements.
type builder struct {
	b    *bind.Bound
	anon int
}

// single lowers one UNION arm's clause sequence. bound tracks the variables in
// scope so a pattern can root on an already-bound node (an expand) rather than
// rescanning it.
func (bd *builder) single(sq *ast.SingleQuery) Op {
	var cur Op
	bound := map[string]bool{}
	for _, c := range sq.Clauses {
		switch cl := c.(type) {
		case *ast.Match:
			cur = bd.match(cl, cur, bound)
		case *ast.Unwind:
			cur = &Unwind{Input: cur, Expr: cl.Expr, Var: cl.Var}
			bound[cl.Var] = true
		case *ast.With:
			cur = bd.projection(&cl.Projection, cur, bound)
			if cl.Where != nil {
				cur = &Filter{Input: cur, Pred: cl.Where}
			}
			resetScope(bound, &cl.Projection, bd)
		case *ast.Return:
			cur = bd.projection(&cl.Projection, cur, bound)
			resetScope(bound, &cl.Projection, bd)
		}
	}
	return cur
}

// match lowers a MATCH or OPTIONAL MATCH. A plain MATCH extends the current tree
// in place (scans for new nodes, expands along relationships, cartesian joins
// for disconnected patterns). An OPTIONAL MATCH builds each pattern as a
// correlated subtree and left-outer-joins it onto the current tree.
func (bd *builder) match(m *ast.Match, cur Op, bound map[string]bool) Op {
	for _, pp := range m.Patterns {
		if m.Optional {
			inner := bd.pathCorrelated(pp, bound)
			if cur == nil {
				cur = &Unit{}
			}
			cur = &Optional{Input: cur, Inner: inner}
		} else {
			cur = bd.path(pp, cur, bound)
		}
	}
	if m.Where != nil {
		cur = &Filter{Input: cur, Pred: m.Where}
	}
	return cur
}

// path lowers one path pattern into the running tree. The start node is a scan
// (if new) joined onto the tree, or the tree's already-bound node; each chain
// step is an expand.
func (bd *builder) path(pp *ast.PathPattern, cur Op, bound map[string]bool) Op {
	start := bd.nodeName(pp.Start)
	if !bound[start] {
		leaf := bd.scanNode(pp.Start, start)
		bound[start] = true
		if cur == nil {
			cur = leaf
		} else {
			cur = &Join{Left: cur, Right: leaf, On: sharedVars(cur, leaf)}
		}
	}
	return bd.expandChain(cur, start, pp.Chain, bound)
}

// pathCorrelated lowers a pattern for the inner side of an OPTIONAL MATCH. A
// start node already bound by the outer scope becomes an Argument (the outer row
// supplies it); a new start node is scanned.
func (bd *builder) pathCorrelated(pp *ast.PathPattern, outer map[string]bool) Op {
	inner := map[string]bool{}
	for v := range outer {
		inner[v] = true
	}
	start := bd.nodeName(pp.Start)
	var cur Op
	if outer[start] {
		cur = &Argument{Vars: sortedKeys(outer)}
	} else {
		cur = bd.scanNode(pp.Start, start)
		inner[start] = true
	}
	cur = bd.expandChain(cur, start, pp.Chain, inner)
	// the outer scope gains the pattern's variables (they are visible, possibly
	// null, after the optional match).
	for v := range inner {
		outer[v] = true
	}
	return cur
}

// expandChain appends one Expand per relationship step, lowering label and
// property constraints on each reached node and relationship.
func (bd *builder) expandChain(cur Op, prev string, chain []ast.PatternChain, bound map[string]bool) Op {
	for _, step := range chain {
		rel := bd.relName(step.Rel)
		to := bd.nodeName(step.Node)
		toBound := bound[to]
		ex := &Expand{
			Input:    cur,
			From:     prev,
			Rel:      rel,
			To:       to,
			Types:    bd.b.RelTypes(step.Rel),
			ToLabels: bd.b.NodeLabels(step.Node),
			Dir:      step.Rel.Dir,
			VarLen:   step.Rel.VarLen,
			ToBound:  toBound,
		}
		bound[rel] = true
		bound[to] = true
		cur = withPropFilters(ex, rel, step.Rel.Properties)
		cur = withPropFilters(cur, to, step.Node.Properties)
		prev = to
	}
	return cur
}

// scanNode builds a NodeScan and lowers any property-map constraint to equality
// filters above it.
func (bd *builder) scanNode(np *ast.NodePattern, name string) Op {
	var cur Op = &NodeScan{Var: name, Labels: bd.b.NodeLabels(np)}
	return withPropFilters(cur, name, np.Properties)
}

// withPropFilters wraps an operator in equality filters, one per property-map
// entry, so {name: $x} on a variable v becomes a filter v.name = $x.
func withPropFilters(cur Op, v string, props []ast.PropEntry) Op {
	for _, pe := range props {
		eq := &ast.Binary{
			Op: ast.OpEq,
			L:  &ast.Property{Base: &ast.Variable{Name: v}, Key: pe.Key},
			R:  pe.Value,
		}
		cur = &Filter{Input: cur, Pred: eq}
	}
	return cur
}

// projection lowers a WITH or RETURN. An aggregating projection becomes an
// Aggregate (grouping by its non-aggregating columns); a plain one a Project.
// The ORDER BY / SKIP / LIMIT tail wraps the result in that order.
func (bd *builder) projection(p *ast.Projection, cur Op, bound map[string]bool) Op {
	if cur == nil {
		cur = &Unit{}
	}
	cols := projectionCols(p, bound)
	var base Op
	if anyAggregate(cols) {
		var groups, aggs []Col
		for _, c := range cols {
			if exprHasAggregate(c.Expr) {
				aggs = append(aggs, c)
			} else {
				groups = append(groups, c)
			}
		}
		base = &Aggregate{Input: cur, GroupKeys: groups, Aggs: aggs, Distinct: p.Distinct}
	} else {
		base = &Project{Input: cur, Cols: cols, Distinct: p.Distinct}
	}
	if len(p.OrderBy) > 0 {
		keys := make([]SortKey, len(p.OrderBy))
		for i, s := range p.OrderBy {
			keys[i] = SortKey{Expr: s.Expr, Desc: s.Desc}
		}
		base = &Sort{Input: base, Keys: keys}
	}
	if p.Skip != nil {
		base = &Skip{Input: base, N: p.Skip}
	}
	if p.Limit != nil {
		base = &Limit{Input: base, N: p.Limit}
	}
	return base
}

// projectionCols expands a projection into output columns: a star contributes
// the in-scope variables (sorted for determinism), then each explicit item
// contributes its expression under its column name.
func projectionCols(p *ast.Projection, bound map[string]bool) []Col {
	var cols []Col
	if p.Star {
		for _, v := range sortedKeys(bound) {
			cols = append(cols, Col{Expr: &ast.Variable{Name: v}, Name: v})
		}
	}
	for _, it := range p.Items {
		cols = append(cols, Col{Expr: it.Expr, Name: itemName(it)})
	}
	return cols
}

// itemName is a projected item's output name: its alias, its variable name, its
// property path, or the printed expression.
func itemName(it ast.ProjItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	switch x := it.Expr.(type) {
	case *ast.Variable:
		return x.Name
	case *ast.Property:
		return ast.Print(x)
	default:
		return ast.Print(it.Expr)
	}
}

// resetScope replaces the bound set with the variables a projection outputs, the
// scope the next clause sees (doc 09 §4.4).
func resetScope(bound map[string]bool, p *ast.Projection, bd *builder) {
	out := projectionCols(p, bound)
	for k := range bound {
		delete(bound, k)
	}
	for _, c := range out {
		bound[c.Name] = true
	}
}

// --- anonymous naming ---

// nodeName returns a node pattern's variable, assigning a unique synthetic name
// to an anonymous node. The '@' prefix cannot appear in a user identifier, so a
// synthetic name never collides with a real one.
func (bd *builder) nodeName(np *ast.NodePattern) string {
	if np.Var != "" {
		return np.Var
	}
	n := "@n" + itoa(bd.anon)
	bd.anon++
	return n
}

func (bd *builder) relName(rp *ast.RelPattern) string {
	if rp.Var != "" {
		return rp.Var
	}
	n := "@r" + itoa(bd.anon)
	bd.anon++
	return n
}

// --- helpers ---

// sharedVars returns the variable names two subtrees both produce, the natural
// join keys between them.
func sharedVars(a, b Op) []string {
	av, bv := outputVars(a), outputVars(b)
	var shared []string
	for v := range av {
		if bv[v] {
			shared = append(shared, v)
		}
	}
	sort.Strings(shared)
	return shared
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- aggregate detection (the read path's aggregate function set, doc 09 §8.1) ---

var aggregateNames = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
	"collect": true, "stdev": true, "stdevp": true,
	"percentilecont": true, "percentiledisc": true,
}

func anyAggregate(cols []Col) bool {
	for _, c := range cols {
		if exprHasAggregate(c.Expr) {
			return true
		}
	}
	return false
}

func exprHasAggregate(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.FunctionCall:
		if aggregateNames[strings.ToLower(x.Name)] {
			return true
		}
		for _, a := range x.Args {
			if exprHasAggregate(a) {
				return true
			}
		}
		return false
	case *ast.Property:
		return exprHasAggregate(x.Base)
	case *ast.Index:
		return exprHasAggregate(x.Base) || exprHasAggregate(x.Index)
	case *ast.Slice:
		return exprHasAggregate(x.Base) ||
			(x.Lo != nil && exprHasAggregate(x.Lo)) ||
			(x.Hi != nil && exprHasAggregate(x.Hi))
	case *ast.Unary:
		return exprHasAggregate(x.X)
	case *ast.Binary:
		return exprHasAggregate(x.L) || exprHasAggregate(x.R)
	case *ast.IsNull:
		return exprHasAggregate(x.X)
	case *ast.ListLit:
		for _, el := range x.Elems {
			if exprHasAggregate(el) {
				return true
			}
		}
		return false
	case *ast.MapLit:
		for _, ent := range x.Entries {
			if exprHasAggregate(ent.Value) {
				return true
			}
		}
		return false
	case *ast.Case:
		if x.Subject != nil && exprHasAggregate(x.Subject) {
			return true
		}
		for _, w := range x.Whens {
			if exprHasAggregate(w.When) || exprHasAggregate(w.Then) {
				return true
			}
		}
		return x.Else != nil && exprHasAggregate(x.Else)
	default:
		return false
	}
}
