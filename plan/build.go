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

// Plan is the structural planning output: build the raw logical tree, apply the
// rule-based planner subset (anchor and direction choice on the structural proxy,
// doc 11 §3, §6 simple form), then normalize to canonical form. It is a pure
// function of the bound query, with no statistics, so it is the stable shape the
// planner golden tests assert against. The read path calls [PlanWithStats] to
// drive the same choices on cost instead (doc 10 §8.1).
func Plan(b *bind.Bound) Op { return PlanWithStats(b, nil) }

// PlanWithStats is Plan with a cost model: Optimize orders each linear chain's
// anchor and expand direction by estimated cardinality rather than the structural
// proxy (doc 11 §3, §4). With nil statistics it is exactly Plan, so the two share
// one path and the structural choice is the cost path's fallback, not a separate
// planner.
func PlanWithStats(b *bind.Bound, st Statistics) Op {
	return JoinOrder(Normalize(Optimize(Build(b), st)), st)
}

// builder carries the per-build state: the bound query (for resolved tokens) and
// the counter that names anonymous pattern elements.
type builder struct {
	b    *bind.Bound
	anon int
	// nodeNames and relNames memoize the synthetic names assigned to anonymous
	// pattern elements, keyed by the AST node, so every reference to one element
	// (the expand that binds it and the path that lists it) sees the same name.
	nodeNames map[*ast.NodePattern]string
	relNames  map[*ast.RelPattern]string
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
		case *ast.Create:
			cur = bd.create(cl, cur, bound)
		case *ast.Merge:
			cur = bd.merge(cl, cur, bound)
		case *ast.Set:
			cur = bd.set(cl, cur)
		case *ast.Remove:
			cur = bd.remove(cl, cur)
		case *ast.Delete:
			cur = bd.deleteClause(cl, cur)
		case *ast.Foreach:
			cur = bd.foreach(cl, cur, bound)
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
	if pp.Shortest != ast.NotShortest {
		return bd.shortestPath(pp, cur, bound)
	}
	start := bd.nodeName(pp.Start)
	cur = bd.joinNode(cur, pp.Start, start, bound)
	cur = bd.expandChain(cur, start, bd.b.NodeLabels(pp.Start), pp.Chain, bound)
	if pp.Var != "" {
		cur = &BindPath{Input: cur, Var: pp.Var, Elems: bd.pathElems(pp)}
	}
	return cur
}

// create lowers a CREATE clause into a single Create operator over the running
// tree (a leading CREATE runs over a Unit row). Every pattern's new nodes and
// relationships are folded into the one operator; a pattern element naming an
// already-bound variable is a reference, not a creation. A named path is bound
// above the operator from its element variables, exactly as in MATCH.
func (bd *builder) create(c *ast.Create, cur Op, bound map[string]bool) Op {
	if cur == nil {
		cur = &Unit{}
	}
	cr := &Create{Input: cur}
	for _, pp := range c.Patterns {
		bd.lowerCreatePattern(pp, cr, bound)
	}
	var out Op = cr
	for _, pp := range c.Patterns {
		if pp.Var != "" {
			out = &BindPath{Input: out, Var: pp.Var, Elems: bd.pathElems(pp)}
			bound[pp.Var] = true
		}
	}
	return out
}

// lowerCreatePattern folds one CREATE path pattern into the operator: each node is
// created (unless already bound), then each step's relationship, oriented so From
// points at To regardless of how the pattern was written.
func (bd *builder) lowerCreatePattern(pp *ast.PathPattern, cr *Create, bound map[string]bool) {
	start := bd.nodeName(pp.Start)
	bd.addCreateNode(cr, pp.Start, start, bound)
	prev := start
	for _, step := range pp.Chain {
		to := bd.nodeName(step.Node)
		bd.addCreateNode(cr, step.Node, to, bound)
		rel := bd.relName(step.Rel)
		from, dst := prev, to
		if step.Rel.Dir == ast.DirIn {
			from, dst = to, prev
		}
		cr.Rels = append(cr.Rels, RelCreate{
			Var:   rel,
			From:  from,
			To:    dst,
			Type:  firstRef(bd.b.RelTypes(step.Rel)),
			Props: bd.propSets(step.Rel.Properties),
		})
		bound[rel] = true
		prev = to
	}
}

// merge lowers a MERGE clause (doc 13 §11) into a Merge operator over the running
// tree (a leading MERGE runs over a Unit row). It builds two views of the one
// pattern: a correlated read sub-plan (the match probe, reusing the OPTIONAL MATCH
// lowering so the pattern is found under the writer's snapshot) and a create spec
// (the same fold CREATE uses, the create branch). The variables the pattern
// introduces beyond the entry scope become bound after the clause, exactly as a
// matched-or-created element should. A named path is bound above the operator from
// its element variables, the same as CREATE and MATCH.
func (bd *builder) merge(m *ast.Merge, cur Op, bound map[string]bool) Op {
	if cur == nil {
		cur = &Unit{}
	}
	entry := copyBound(bound)
	var newVars []string
	for _, e := range bd.pathElems(m.Pattern) {
		if !entry[e] {
			newVars = append(newVars, e)
		}
	}
	match := bd.mergeMatch(m.Pattern, copyBound(entry))
	cr := &Create{}
	bd.lowerCreatePattern(m.Pattern, cr, copyBound(entry))
	mg := &Merge{
		Input:    cur,
		Match:    match,
		Nodes:    cr.Nodes,
		Rels:     cr.Rels,
		NewVars:  newVars,
		OnCreate: bd.lowerSetItems(m.OnCreate),
		OnMatch:  bd.lowerSetItems(m.OnMatch),
	}
	for _, v := range newVars {
		bound[v] = true
	}
	var out Op = mg
	if m.Pattern.Var != "" {
		out = &BindPath{Input: out, Var: m.Pattern.Var, Elems: bd.pathElems(m.Pattern)}
		bound[m.Pattern.Var] = true
	}
	return out
}

// mergeMatch builds the correlated read sub-plan a MERGE uses to probe for an
// existing match. Unlike the OPTIONAL MATCH lowering, a MERGE pattern's property
// filters routinely reference variables from the entry scope: MERGE (u:User
// {email: e}) after UNWIND ... AS e reads e on every probe. So the subtree always
// roots on an Argument carrying the entry scope, cross-joins a scan for any new
// start node, and applies every property filter above the scan and expand, where
// both the outer variable and the matched element are in scope. The single-row
// Argument is fed the current outer row each time mergeOp reopens this subtree.
func (bd *builder) mergeMatch(pp *ast.PathPattern, outer map[string]bool) Op {
	inner := copyBound(outer)
	start := bd.nodeName(pp.Start)
	var cur Op
	if len(outer) > 0 {
		cur = &Argument{Vars: sortedKeys(outer)}
	}
	if !outer[start] {
		leaf := &NodeScan{Var: start, Labels: bd.b.NodeLabels(pp.Start)}
		if cur == nil {
			cur = leaf
		} else {
			cur = &Join{Left: cur, Right: leaf, On: sharedVars(cur, leaf)}
		}
		inner[start] = true
	}
	cur = withPropFilters(cur, start, pp.Start.Properties)
	prev := start
	prevLabels := bd.b.NodeLabels(pp.Start)
	for _, step := range pp.Chain {
		rel := bd.relName(step.Rel)
		to := bd.nodeName(step.Node)
		ex := &Expand{
			Input:      cur,
			From:       prev,
			Rel:        rel,
			To:         to,
			Types:      bd.b.RelTypes(step.Rel),
			FromLabels: prevLabels,
			ToLabels:   bd.b.NodeLabels(step.Node),
			Dir:        step.Rel.Dir,
			VarLen:     step.Rel.VarLen,
			ToBound:    inner[to],
		}
		cur = withPropFilters(ex, rel, step.Rel.Properties)
		cur = withPropFilters(cur, to, step.Node.Properties)
		inner[rel] = true
		inner[to] = true
		prev = to
		prevLabels = bd.b.NodeLabels(step.Node)
	}
	if pp.Var != "" {
		cur = &BindPath{Input: cur, Var: pp.Var, Elems: bd.pathElems(pp)}
	}
	return cur
}

// foreach lowers a FOREACH clause (doc 13 §10) into a Foreach operator over the
// running tree (a leading FOREACH runs over a Unit row). The clause is a
// write-only loop: FOREACH (x IN list | writes) runs the writes once per list
// element. It lowers to a correlated write sub-plan rooted on an Argument carrying
// the outer scope and an Unwind of the list that binds the loop variable, with the
// body's write operators stacked on top. The Argument is fed the current outer row
// each time foreachOp reopens the sub-plan. The body's bindings stay inside that
// sub-plan and never reach the surrounding query (§10.3), so nothing is added to
// the outer scope.
func (bd *builder) foreach(f *ast.Foreach, cur Op, bound map[string]bool) Op {
	if cur == nil {
		cur = &Unit{}
	}
	outer := copyBound(bound)
	var body Op
	if len(outer) > 0 {
		body = &Argument{Vars: sortedKeys(outer)}
	}
	body = &Unwind{Input: body, Expr: f.List, Var: f.Var}
	inner := copyBound(outer)
	inner[f.Var] = true
	for _, c := range f.Body {
		body = bd.foreachBody(c, body, inner)
	}
	return &Foreach{Input: cur, Body: body}
}

// foreachBody lowers one clause inside a FOREACH body, threading the running
// sub-plan and the loop's inner scope. The body allows only write clauses (the
// binder rejects the rest, doc 13 §10.2), each lowered with the same builder the
// top level uses; a nested FOREACH recurses.
func (bd *builder) foreachBody(c ast.Clause, cur Op, bound map[string]bool) Op {
	switch cl := c.(type) {
	case *ast.Create:
		return bd.create(cl, cur, bound)
	case *ast.Merge:
		return bd.merge(cl, cur, bound)
	case *ast.Set:
		return bd.set(cl, cur)
	case *ast.Remove:
		return bd.remove(cl, cur)
	case *ast.Delete:
		return bd.deleteClause(cl, cur)
	case *ast.Foreach:
		return bd.foreach(cl, cur, bound)
	}
	return cur
}

// copyBound returns a shallow copy of a bound-variable set, so a sub-plan builder
// (a MERGE match probe, an OPTIONAL inner) can extend its own scope without
// disturbing the caller's.
func copyBound(b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(b))
	for k, v := range b {
		out[k] = v
	}
	return out
}

// set lowers a SET clause into a Set operator over the running tree, resolving
// each item's property key or labels through the bound query.
func (bd *builder) set(s *ast.Set, cur Op) Op {
	if cur == nil {
		cur = &Unit{}
	}
	return &Set{Input: cur, Items: bd.lowerSetItems(s.Items)}
}

// lowerSetItems lowers a list of AST SET items to plan items, resolving each
// item's property key or labels through the bound query. It backs the SET clause
// and MERGE's ON CREATE / ON MATCH sub-clauses (doc 13 §11.5).
func (bd *builder) lowerSetItems(items []ast.SetItem) []SetItem {
	out := make([]SetItem, 0, len(items))
	for _, it := range items {
		switch it.Op {
		case ast.SetProperty:
			out = append(out, SetItem{
				Kind: SetItemProp,
				Var:  it.Var,
				Key:  bd.b.PropKey(it.Key),
				Expr: it.Value,
			})
		case ast.SetLabels:
			out = append(out, SetItem{
				Kind:   SetItemLabels,
				Var:    it.Var,
				Labels: bd.labelRefs(it.Labels),
			})
		case ast.SetMerge:
			out = append(out, SetItem{
				Kind: SetItemMerge,
				Var:  it.Var,
				Expr: it.Value,
			})
		case ast.SetReplace:
			out = append(out, SetItem{
				Kind: SetItemReplace,
				Var:  it.Var,
				Expr: it.Value,
			})
		}
	}
	return out
}

// remove lowers a REMOVE clause into a Remove operator over the running tree.
func (bd *builder) remove(r *ast.Remove, cur Op) Op {
	if cur == nil {
		cur = &Unit{}
	}
	op := &Remove{Input: cur}
	for _, it := range r.Items {
		if len(it.Labels) > 0 {
			op.Items = append(op.Items, RemoveItem{
				Var:    it.Var,
				Labels: bd.labelRefs(it.Labels),
			})
		} else {
			op.Items = append(op.Items, RemoveItem{
				Var: it.Var,
				Key: bd.b.PropKey(it.Key),
			})
		}
	}
	return op
}

// deleteClause lowers a DELETE or DETACH DELETE into a Delete operator over the
// running tree. It carries the target expressions through verbatim; the executor
// evaluates each per row.
func (bd *builder) deleteClause(d *ast.Delete, cur Op) Op {
	if cur == nil {
		cur = &Unit{}
	}
	return &Delete{Input: cur, Detach: d.Detach, Targets: d.Targets}
}

// labelRefs resolves a SET/REMOVE label-name list to its NameRefs through the
// bound query's label map.
func (bd *builder) labelRefs(names []string) []bind.NameRef {
	out := make([]bind.NameRef, len(names))
	for i, n := range names {
		out[i] = bd.b.Label(n)
	}
	return out
}

// addCreateNode appends a node creation unless the variable is already bound, in
// which case the pattern references an existing node and creates nothing.
func (bd *builder) addCreateNode(cr *Create, np *ast.NodePattern, name string, bound map[string]bool) {
	if bound[name] {
		return
	}
	cr.Nodes = append(cr.Nodes, NodeCreate{
		Var:    name,
		Labels: bd.b.NodeLabels(np),
		Props:  bd.propSets(np.Properties),
	})
	bound[name] = true
}

// propSets lowers a pattern's property map into resolved key/expression pairs.
func (bd *builder) propSets(props []ast.PropEntry) []PropSet {
	if len(props) == 0 {
		return nil
	}
	out := make([]PropSet, len(props))
	for i, pe := range props {
		out[i] = PropSet{Key: bd.b.PropKey(pe.Key), Expr: pe.Value}
	}
	return out
}

// firstRef returns the first resolved name of a set, the single type a created
// relationship carries (the binder guarantees CREATE names exactly one).
func firstRef(refs []bind.NameRef) bind.NameRef {
	if len(refs) == 0 {
		return bind.NameRef{}
	}
	return refs[0]
}

// joinNode ensures a node variable is bound, scanning it (with its property-map
// constraints) and joining the scan onto the running tree when it is new. An
// already-bound node leaves the tree unchanged.
func (bd *builder) joinNode(cur Op, np *ast.NodePattern, name string, bound map[string]bool) Op {
	if bound[name] {
		return cur
	}
	leaf := bd.scanNode(np, name)
	bound[name] = true
	if cur == nil {
		return leaf
	}
	return &Join{Left: cur, Right: leaf, On: sharedVars(cur, leaf)}
}

// shortestPath lowers a shortestPath / allShortestPaths pattern. Both endpoints
// are made bound first (a scan joined in for a new one, an already-bound one left
// in place), then a ShortestPath operator searches between them. The binder
// guarantees the pattern carries exactly one relationship step.
func (bd *builder) shortestPath(pp *ast.PathPattern, cur Op, bound map[string]bool) Op {
	start := bd.nodeName(pp.Start)
	cur = bd.joinNode(cur, pp.Start, start, bound)
	step := pp.Chain[0]
	end := bd.nodeName(step.Node)
	cur = bd.joinNode(cur, step.Node, end, bound)
	rel := bd.relName(step.Rel)
	bound[rel] = true
	if pp.Var != "" {
		bound[pp.Var] = true
	}
	return &ShortestPath{
		Input:   cur,
		From:    start,
		To:      end,
		Rel:     rel,
		PathVar: pp.Var,
		Types:   bd.b.RelTypes(step.Rel),
		Dir:     step.Rel.Dir,
		VarLen:  step.Rel.VarLen,
		All:     pp.Shortest == ast.ShortestAll,
	}
}

// pathElems returns a pattern's element variable names in traversal order: the
// start node, then each step's relationship and node. The names are the binder's
// (synthetic for anonymous elements), so a path materializes even when the
// pattern names none of its elements.
func (bd *builder) pathElems(pp *ast.PathPattern) []string {
	elems := []string{bd.nodeName(pp.Start)}
	for _, step := range pp.Chain {
		elems = append(elems, bd.relName(step.Rel), bd.nodeName(step.Node))
	}
	return elems
}

// pathCorrelated lowers a pattern for the inner side of an OPTIONAL MATCH. A
// start node already bound by the outer scope becomes an Argument (the outer row
// supplies it); a new start node is scanned.
func (bd *builder) pathCorrelated(pp *ast.PathPattern, outer map[string]bool) Op {
	inner := map[string]bool{}
	for v := range outer {
		inner[v] = true
	}
	if pp.Shortest != ast.NotShortest {
		return bd.shortestPathCorrelated(pp, outer, inner)
	}
	start := bd.nodeName(pp.Start)
	var cur Op
	if outer[start] {
		cur = &Argument{Vars: sortedKeys(outer)}
	} else {
		cur = bd.scanNode(pp.Start, start)
		inner[start] = true
	}
	cur = bd.expandChain(cur, start, bd.b.NodeLabels(pp.Start), pp.Chain, inner)
	if pp.Var != "" {
		cur = &BindPath{Input: cur, Var: pp.Var, Elems: bd.pathElems(pp)}
		inner[pp.Var] = true
	}
	// the outer scope gains the pattern's variables (they are visible, possibly
	// null, after the optional match).
	for v := range inner {
		outer[v] = true
	}
	return cur
}

// shortestPathCorrelated lowers a shortestPath / allShortestPaths pattern on the
// inner side of an OPTIONAL MATCH. The subtree roots on an Argument carrying the
// outer scope (so an endpoint the outer query already bound is supplied per row),
// joining a fresh scan for any endpoint the pattern introduces, then a
// ShortestPath searches between the two.
func (bd *builder) shortestPathCorrelated(pp *ast.PathPattern, outer, inner map[string]bool) Op {
	var cur Op = &Argument{Vars: sortedKeys(outer)}
	start := bd.nodeName(pp.Start)
	if !outer[start] {
		leaf := bd.scanNode(pp.Start, start)
		inner[start] = true
		cur = &Join{Left: cur, Right: leaf, On: sharedVars(cur, leaf)}
	}
	step := pp.Chain[0]
	end := bd.nodeName(step.Node)
	if !inner[end] {
		leaf := bd.scanNode(step.Node, end)
		inner[end] = true
		cur = &Join{Left: cur, Right: leaf, On: sharedVars(cur, leaf)}
	}
	rel := bd.relName(step.Rel)
	inner[rel] = true
	if pp.Var != "" {
		inner[pp.Var] = true
	}
	for v := range inner {
		outer[v] = true
	}
	return &ShortestPath{
		Input:   cur,
		From:    start,
		To:      end,
		Rel:     rel,
		PathVar: pp.Var,
		Types:   bd.b.RelTypes(step.Rel),
		Dir:     step.Rel.Dir,
		VarLen:  step.Rel.VarLen,
		All:     pp.Shortest == ast.ShortestAll,
	}
}

// expandChain appends one Expand per relationship step, lowering label and
// property constraints on each reached node and relationship. prevLabels is the
// source node's labels, threaded so each step's Expand can carry the labels of the
// node it expands from (the start node for the first step, the previous step's
// reached node thereafter), which the cost model conditions the fan-out on.
func (bd *builder) expandChain(cur Op, prev string, prevLabels []bind.NameRef, chain []ast.PatternChain, bound map[string]bool) Op {
	for _, step := range chain {
		rel := bd.relName(step.Rel)
		to := bd.nodeName(step.Node)
		toBound := bound[to]
		ex := &Expand{
			Input:      cur,
			From:       prev,
			Rel:        rel,
			To:         to,
			Types:      bd.b.RelTypes(step.Rel),
			FromLabels: prevLabels,
			ToLabels:   bd.b.NodeLabels(step.Node),
			Dir:        step.Rel.Dir,
			VarLen:     step.Rel.VarLen,
			ToBound:    toBound,
		}
		bound[rel] = true
		bound[to] = true
		cur = withPropFilters(ex, rel, step.Rel.Properties)
		cur = withPropFilters(cur, to, step.Node.Properties)
		prev = to
		prevLabels = bd.b.NodeLabels(step.Node)
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
	if n, ok := bd.nodeNames[np]; ok {
		return n
	}
	n := "@n" + itoa(bd.anon)
	bd.anon++
	if bd.nodeNames == nil {
		bd.nodeNames = map[*ast.NodePattern]string{}
	}
	bd.nodeNames[np] = n
	return n
}

func (bd *builder) relName(rp *ast.RelPattern) string {
	if rp.Var != "" {
		return rp.Var
	}
	if n, ok := bd.relNames[rp]; ok {
		return n
	}
	n := "@r" + itoa(bd.anon)
	bd.anon++
	if bd.relNames == nil {
		bd.relNames = map[*ast.RelPattern]string{}
	}
	bd.relNames[rp] = n
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
