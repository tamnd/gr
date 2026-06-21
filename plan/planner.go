package plan

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// Optimize re-roots each fresh linear scan/expand chain at the node it should
// anchor on, reversing the expands so they radiate outward from that anchor
// (doc 11 §3, §4). The choice is the join order of a linear pattern: which node
// scans first and which direction each expand drives.
//
// With statistics it is a cost decision. A chain ties on final cardinality
// whichever node it anchors at, so the planner orders the candidate anchors by
// the sum of their intermediate row counts ([planCost]) and picks the cheapest,
// the one that keeps every partial result smallest. With nil statistics it falls
// back to the structural proxy M2 shipped: anchor on the most selective node by
// label and equality predicate ([nodeScore]). The two share one chain builder,
// so the cost path and the structural path produce the same plan shape, only
// chosen on a different metric.
//
// The choice is meaning-preserving whichever metric drives it: the executor
// produces the same bindings whatever the anchor or direction (a relationship is
// stored in both CSR directions, doc 04 §5.2), so this only changes the plan's
// shape, never its result.
func Optimize(o Op, st Statistics) Op {
	if ch, ok := extractChain(o); ok {
		return reanchor(ch, st)
	}
	if w, ok := wcojRewrite(o, st); ok {
		return w
	}
	return mapChildren(o, func(c Op) Op { return Optimize(c, st) })
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

// reanchor rebuilds the chain rooted at its chosen anchor, or returns the
// original tree when that node is already the anchor.
func reanchor(ch linChain, st Statistics) Op {
	k := pickAnchor(ch, st)
	if k == 0 {
		return ch.root
	}
	return buildAnchored(ch, k)
}

// buildAnchored rebuilds the chain rooted at node k: the scan sits at the anchor,
// the expands radiate right (in written direction) and left (reversed) from it,
// and every filter is re-stacked on top. Normalize then pushes each filter to its
// lowest valid point, so the re-anchored chain keeps the same pushdown as any
// other. It is the one builder both the cost path and the structural path use, so
// each candidate anchor can be costed by building it and reading its plan cost.
func buildAnchored(ch linChain, k int) Op {
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
	for i := len(ch.filters) - 1; i >= 0; i-- {
		cur = &Filter{Input: cur, Pred: ch.filters[i]}
	}
	return cur
}

// pickAnchor returns the index of the node the chain should anchor on, ties
// broken toward the leftmost (the lowest pattern index, for a deterministic
// plan).
//
// With statistics it is a cost choice: each candidate anchor is built and costed
// by the sum of its intermediate row counts ([planCost]), and the cheapest wins.
// Two anchors of one chain tie on final cardinality, so this metric, not the
// final count, is what separates them: anchoring at the rarest node keeps every
// partial result small (doc 11 §3, §4).
//
// With nil statistics it falls back to the M2 structural proxy (doc 11 §3.1): a
// labeled scan reads fewer nodes than an all-nodes scan, and a node pinned by an
// equality predicate yields fewer rows downstream ([nodeScore]).
func pickAnchor(ch linChain, st Statistics) int {
	if st != nil {
		return costAnchor(ch, st)
	}
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

// costAnchor picks the anchor whose rebuilt chain has the lowest plan cost, ties
// broken toward the leftmost. It builds each candidate, normalizes it so filter
// pushdown is reflected in the cost, and keeps the cheapest. Normalizing here
// matters: an equality pushed down to its node turns a scan into a far smaller
// estimate, so the candidate that puts a pinned node early is costed with that
// gain rather than against the un-pushed shape.
func costAnchor(ch linChain, st Statistics) int {
	best, bestCost := 0, -1.0
	for k := range ch.nodes {
		cost := planCost(Normalize(buildAnchored(ch, k)), st)
		if bestCost < 0 || cost < bestCost {
			best, bestCost = k, cost
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
	case *Create:
		y := *x
		y.Input = f(x.Input)
		return &y
	case *Set:
		y := *x
		y.Input = f(x.Input)
		return &y
	case *Remove:
		y := *x
		y.Input = f(x.Input)
		return &y
	case *Delete:
		y := *x
		y.Input = f(x.Input)
		return &y
	case *Filter:
		return &Filter{Input: f(x.Input), Pred: x.Pred}
	case *BindPath:
		return &BindPath{Input: f(x.Input), Var: x.Var, Elems: x.Elems}
	case *ShortestPath:
		y := *x
		y.Input = f(x.Input)
		return &y
	case *Expand:
		return &Expand{
			Input: f(x.Input), From: x.From, Rel: x.Rel, To: x.To,
			Types: x.Types, ToLabels: x.ToLabels, Dir: x.Dir,
			VarLen: x.VarLen, ToBound: x.ToBound,
		}
	case *Intersect:
		return &Intersect{Input: f(x.Input), Var: x.Var, Labels: x.Labels, Legs: x.Legs}
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
