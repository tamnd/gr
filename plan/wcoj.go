package plan

// This file holds the worst-case-optimal join rewrite: the planner recognizes a
// triangle, the smallest cyclic pattern, and replaces its binary-join plan with an
// Intersect when the cost model says the intermediate would blow up (doc 11 §5).
//
// The binary plan for a triangle, as the builder lowers it, is a closing expand-into
// over the expand that produces the apex:
//
//	Expand{From: m, To: a, ToBound: true}   // the closing edge m-a
//	  Expand{From: x, To: m}                 // produces the apex m from x
//	    <subtree binding x and a>
//
// The inner expand enumerates every neighbor of x as a candidate apex, deg(x) of
// them, and the closing expand-into then keeps only those that also reach a. For a
// high-degree x that intermediate dwarfs the closing matches. The Intersect computes
// the apex as the intersection of x's and a's neighbor sets instead, so it touches
// only the smaller side and produces no large intermediate.

// wcojRewrite rewrites a triangle's closing expand-into into an Intersect when the
// cost model prefers it. It matches a closing expand-into directly over the expand
// that produced its source node, with both the source's origin and the closing
// target already bound below, so the produced apex is adjacent to two bound nodes.
// The subtree binding those two nodes is optimized first, so the rewrite composes
// with the rest of the planner.
//
// With nil statistics it does nothing, so the structural Plan keeps the builder's
// binary shape and the planner goldens hold; the rewrite is taken only when the cost
// of the Intersect plan is below the cost of the binary plan, the same cost-or-keep
// discipline every M4 planner decision uses (doc 11 §5.5).
func wcojRewrite(o Op, st Statistics) (Op, bool) {
	if st == nil {
		return nil, false
	}
	closing, ok := o.(*Expand)
	if !ok || !closing.ToBound || closing.VarLen != nil {
		return nil, false
	}
	inner, ok := closing.Input.(*Expand)
	if !ok || inner.ToBound || inner.VarLen != nil {
		return nil, false
	}
	// The closing edge must leave the node the inner expand just produced, and reach a
	// node bound below the inner expand. That makes the produced apex (inner.To) the one
	// node adjacent to two already-bound endpoints (inner.From and closing.To).
	apex := inner.To
	if closing.From != apex {
		return nil, false
	}
	bound := outputVars(inner.Input)
	if !bound[inner.From] || !bound[closing.To] {
		return nil, false
	}
	// A self-loop apex (the apex is also one of the endpoints) is not a triangle the
	// intersection models, so leave it to the binary plan.
	if apex == inner.From || apex == closing.To || inner.From == closing.To {
		return nil, false
	}
	sub := Optimize(inner.Input, st)
	binary := &Expand{
		Input: &Expand{
			Input: sub, From: inner.From, Rel: inner.Rel, To: inner.To,
			Types: inner.Types, ToLabels: inner.ToLabels, Dir: inner.Dir,
		},
		From: closing.From, Rel: closing.Rel, To: closing.To,
		Types: closing.Types, ToLabels: closing.ToLabels, Dir: closing.Dir, ToBound: true,
	}
	wcoj := &Intersect{
		Input:  sub,
		Var:    apex,
		Labels: inner.ToLabels,
		Legs: [2]IntersectLeg{
			// One leg expands to the apex from the inner edge's origin, written direction.
			{From: inner.From, Rel: inner.Rel, Types: inner.Types, Dir: inner.Dir},
			// The other reaches the apex from the closing edge's target: the closing edge
			// runs apex toward that target, so from the target the apex is the reverse
			// direction.
			{From: closing.To, Rel: closing.Rel, Types: closing.Types, Dir: reverseDir(closing.Dir)},
		},
	}
	if planCost(Normalize(wcoj), st) < planCost(Normalize(binary), st) {
		return wcoj, true
	}
	return binary, true
}
