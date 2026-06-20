package plan

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// Optimize is the rule-based planner subset (doc 11 §3, §6 simple form; doc 25
// §5.2 deliverable 5). M2 has no cost model, so the only physical choices it
// makes are the ones a rule, not a cost, can make: where a fresh linear pattern
// anchors its scan, and which way each relationship is traversed. It re-roots a
// scan/expand chain at its most structurally selective node and reverses the
// expands so they radiate outward from that anchor.
//
// The choice is meaning-preserving: the executor produces the same bindings
// whatever the anchor or direction (a relationship is stored in both CSR
// directions, doc 04 §5.2), so this only changes the plan's shape, never its
// result. The cost-based planner that would weigh degree statistics and index
// access paths is M4 ([11](11-query-planner.md) §2, §4); this subset is the
// deliberately naïve stand-in M2 ships ([25](../25-roadmap.md) §5.4).
func Optimize(o Op) Op {
	if ch, ok := extractChain(o); ok {
		return reanchor(ch)
	}
	return mapChildren(o, Optimize)
}

// plannedNode is one node of a linearized pattern chain: its variable and the
// labels its scan or expand carried.
type plannedNode struct {
	name   string
	labels []bind.NameRef
}

// plannedRel is one relationship of a linearized chain. dir is written from
// nodes[i] toward nodes[i+1]; reversing the traversal flips it.
type plannedRel struct {
	name  string
	types []bind.NameRef
	dir   ast.Direction
}

// linChain is a fresh, acyclic, fixed-length scan/expand chain peeled out of the
// built tree: the nodes and relationships in pattern order, every filter found
// along it, and the original root (returned unchanged when the anchor does not
// move).
type linChain struct {
	root    Op
	filters []ast.Expr
	nodes   []plannedNode
	rels    []plannedRel
}

// extractChain recognizes a re-anchorable chain: a run of Expands over a single
// NodeScan, with Filters interleaved freely. It bails on anything the simple
// re-anchoring cannot safely move — a variable-length step, an expand-into
// (which marks a cycle back to a bound node), a non-linear continuity, or a
// repeated node variable — and on a bare scan with no expand (nothing to
// re-anchor). A subtree rooted at a Join, an Argument, or any other operator is
// not a fresh linear chain, so it is left to mapChildren.
func extractChain(o Op) (linChain, bool) {
	var filters []ast.Expr
	var exps []*Expand
	cur := o
walk:
	for {
		switch x := cur.(type) {
		case *Filter:
			filters = append(filters, x.Pred)
			cur = x.Input
		case *Expand:
			if x.VarLen != nil || x.ToBound {
				return linChain{}, false
			}
			exps = append(exps, x)
			cur = x.Input
		case *NodeScan:
			ch, ok := assemble(o, x, exps, filters)
			if !ok {
				return linChain{}, false
			}
			return ch, true
		default:
			break walk
		}
	}
	return linChain{}, false
}

// assemble turns the peeled scan and the top-down Expand stack into a chain in
// pattern order, verifying linear continuity and distinct node variables.
func assemble(root Op, scan *NodeScan, exps []*Expand, filters []ast.Expr) (linChain, bool) {
	if len(exps) == 0 {
		return linChain{}, false // a lone scan: nothing to re-anchor
	}
	nodes := []plannedNode{{name: scan.Var, labels: scan.Labels}}
	rels := make([]plannedRel, 0, len(exps))
	seen := map[string]bool{scan.Var: true}
	// exps is top-down (last step first); pattern order is the reverse.
	for i := len(exps) - 1; i >= 0; i-- {
		ex := exps[i]
		prev := nodes[len(nodes)-1]
		if ex.From != prev.name {
			return linChain{}, false // not a single linear chain
		}
		if seen[ex.To] {
			return linChain{}, false // a cycle: re-anchoring is the cost planner's job
		}
		seen[ex.To] = true
		nodes = append(nodes, plannedNode{name: ex.To, labels: ex.ToLabels})
		rels = append(rels, plannedRel{name: ex.Rel, types: ex.Types, dir: ex.Dir})
	}
	return linChain{root: root, filters: filters, nodes: nodes, rels: rels}, true
}

// reanchor rebuilds the chain rooted at its most selective node, or returns the
// original tree when that node is already the anchor.
func reanchor(ch linChain) Op {
	k := pickAnchor(ch)
	if k == 0 {
		return ch.root
	}
	// The scan sits at the anchor; expands radiate right (in written direction)
	// and left (reversed) from it.
	var cur Op = &NodeScan{Var: ch.nodes[k].name, Labels: ch.nodes[k].labels}
	for i := k; i < len(ch.rels); i++ {
		cur = &Expand{
			Input:    cur,
			From:     ch.nodes[i].name,
			Rel:      ch.rels[i].name,
			To:       ch.nodes[i+1].name,
			Types:    ch.rels[i].types,
			ToLabels: ch.nodes[i+1].labels,
			Dir:      ch.rels[i].dir,
		}
	}
	for i := k - 1; i >= 0; i-- {
		cur = &Expand{
			Input:    cur,
			From:     ch.nodes[i+1].name,
			Rel:      ch.rels[i].name,
			To:       ch.nodes[i].name,
			Types:    ch.rels[i].types,
			ToLabels: ch.nodes[i].labels,
			Dir:      reverseDir(ch.rels[i].dir),
		}
	}
	// Re-stack every filter on top; Normalize then pushes each to its lowest
	// valid point, so the re-anchored chain keeps the same pushdown as any other.
	for i := len(ch.filters) - 1; i >= 0; i-- {
		cur = &Filter{Input: cur, Pred: ch.filters[i]}
	}
	return cur
}

// pickAnchor scores each node and returns the index of the most selective, ties
// broken toward the leftmost (the lowest pattern index, for a deterministic
// plan). The score is the M2 structural proxy for selectivity (doc 11 §3.1): a
// labeled scan reads fewer nodes than an all-nodes scan, and a node pinned by an
// equality predicate yields fewer rows downstream. The degree-aware and
// index-aware refinements (doc 11 §3.4, §6.2) need live statistics and index
// access paths, so they are the cost planner's, not this subset's.
func pickAnchor(ch linChain) int {
	pinned := pinnedVars(ch.filters)
	best, bestScore := 0, -1
	for i, n := range ch.nodes {
		s := nodeScore(len(n.labels) > 0, pinned[n.name])
		if s > bestScore {
			best, bestScore = i, s
		}
	}
	return best
}

func nodeScore(labeled, pinned bool) int {
	switch {
	case labeled && pinned:
		return 4
	case labeled:
		return 3
	case pinned:
		return 2
	default:
		return 1
	}
}

// pinnedVars collects the variables an equality predicate pins to a constant
// (v.key = <literal-or-param>), the predicates that make a node selective. Only
// these narrow a node enough to prefer it as an anchor; a range or an
// inter-variable comparison does not pin a single node to a constant.
func pinnedVars(filters []ast.Expr) map[string]bool {
	pinned := map[string]bool{}
	for _, f := range filters {
		bin, ok := f.(*ast.Binary)
		if !ok || bin.Op != ast.OpEq {
			continue
		}
		if v, ok := pinnedSide(bin.L, bin.R); ok {
			pinned[v] = true
		}
		if v, ok := pinnedSide(bin.R, bin.L); ok {
			pinned[v] = true
		}
	}
	return pinned
}

// pinnedSide reports the variable a property-equality pins when prop is a bare
// variable's property and other is a constant.
func pinnedSide(prop, other ast.Expr) (string, bool) {
	p, ok := prop.(*ast.Property)
	if !ok {
		return "", false
	}
	v, ok := p.Base.(*ast.Variable)
	if !ok {
		return "", false
	}
	if !isConst(other) {
		return "", false
	}
	return v.Name, true
}

// isConst reports whether an expression is constant within a query: a literal or
// a parameter (its value is fixed before execution).
func isConst(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Literal, *ast.Param:
		return true
	default:
		return false
	}
}

func reverseDir(d ast.Direction) ast.Direction {
	switch d {
	case ast.DirOut:
		return ast.DirIn
	case ast.DirIn:
		return ast.DirOut
	default:
		return ast.DirBoth
	}
}

// mapChildren returns a copy of o with f applied to each operator child. It is
// the structural recursion the optimizer rides over the non-chain operators
// (projections, joins, optionals, unions, and the like).
func mapChildren(o Op, f func(Op) Op) Op {
	switch x := o.(type) {
	case *Filter:
		return &Filter{Input: f(x.Input), Pred: x.Pred}
	case *BindPath:
		return &BindPath{Input: f(x.Input), Var: x.Var, Elems: x.Elems}
	case *Expand:
		return &Expand{
			Input: f(x.Input), From: x.From, Rel: x.Rel, To: x.To,
			Types: x.Types, ToLabels: x.ToLabels, Dir: x.Dir,
			VarLen: x.VarLen, ToBound: x.ToBound,
		}
	case *Project:
		return &Project{Input: f(x.Input), Cols: x.Cols, Distinct: x.Distinct}
	case *Aggregate:
		return &Aggregate{Input: f(x.Input), GroupKeys: x.GroupKeys, Aggs: x.Aggs, Distinct: x.Distinct}
	case *Join:
		return &Join{Left: f(x.Left), Right: f(x.Right), On: x.On}
	case *Optional:
		return &Optional{Input: f(x.Input), Inner: f(x.Inner)}
	case *Unwind:
		var in Op
		if x.Input != nil {
			in = f(x.Input)
		}
		return &Unwind{Input: in, Expr: x.Expr, Var: x.Var}
	case *Sort:
		return &Sort{Input: f(x.Input), Keys: x.Keys}
	case *Skip:
		return &Skip{Input: f(x.Input), N: x.N}
	case *Limit:
		return &Limit{Input: f(x.Input), N: x.N}
	case *Union:
		return &Union{Left: f(x.Left), Right: f(x.Right), All: x.All}
	default:
		// Unit, Argument, NodeScan: no operator children.
		return o
	}
}
