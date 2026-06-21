package plan

import "sort"

// JoinOrder shapes the hash joins of a plan from estimated cardinalities (doc 11
// §3, §4; doc 12 §5.1). It makes two related choices on the normalized tree.
//
// The first is the build side of a single join. The executor builds a join's hash
// table over its right input and probes it with the left, so the right side is the
// one held in memory and the plan runs leanest when the right side is the smaller.
//
// The second is the order of a cartesian join chain. The builder joins disjoint
// patterns left-deep in pattern order, which is not a size choice, so a query that
// joins three unrelated patterns builds its intermediate products in whatever order
// they were written. JoinOrder flattens such a chain, orders its inputs by
// ascending estimated size, and rebuilds it smallest first, so every partial
// product stays as small as the inputs allow.
//
// It runs on the normalized tree so the estimates see filter pushdown: an input
// whose equality was pushed down to its scan is costed at its reduced size. With
// nil statistics it is the identity, so the structural plan keeps the builder's
// pattern-order shape and the planner goldens are unchanged.
//
// Every choice is meaning-preserving. A hash join is an inner equijoin, or a
// cartesian product when the keys are empty, and a row is a map keyed by variable,
// so merging a left row with a right row yields the same binding whichever side
// each came from; a cartesian product is commutative and associative, so reordering
// its inputs yields the same multiset. Only the order rows are emitted changes,
// which an unordered Cypher result does not observe.
func JoinOrder(o Op, st Statistics) Op {
	if st == nil {
		return o
	}
	return joinOrder(o, st)
}

func joinOrder(o Op, st Statistics) Op {
	j, ok := o.(*Join)
	if !ok {
		return mapChildren(o, func(c Op) Op { return joinOrder(c, st) })
	}
	// A correlated join (one side rooted on an Argument fed by an outer row) or a
	// keyed equijoin is not a free cartesian chain to reorder: its sides are tied to
	// the executor's build side or to a shared key. Recurse into it and make only the
	// build-side choice, keeping the smaller side on the right.
	if len(j.On) != 0 || hasArgument(j) {
		j = mapChildren(o, func(c Op) Op { return joinOrder(c, st) }).(*Join)
		if !hasArgument(j) && EstimateRows(j.Left, st) < EstimateRows(j.Right, st) {
			return &Join{Left: j.Right, Right: j.Left, On: j.On}
		}
		return j
	}
	// A cartesian chain: collect its inputs, order each internally, sort the inputs by
	// ascending size, and rebuild left-deep smallest first.
	leaves := flattenCartesian(j)
	for i := range leaves {
		leaves[i] = joinOrder(leaves[i], st)
	}
	sort.SliceStable(leaves, func(a, b int) bool {
		return EstimateRows(leaves[a], st) < EstimateRows(leaves[b], st)
	})
	return buildLeftDeep(leaves, st)
}

// flattenCartesian collects the inputs of a left-deep cartesian join chain in
// pattern order. It walks down the left spine while each node is a cartesian join
// (empty key set), taking each right input as a leaf, and stops at the first node
// that is not such a join, which becomes the leftmost leaf. A keyed join met on the
// spine is taken whole as one leaf, since reassociating across a shared key is not
// the cartesian case.
func flattenCartesian(j *Join) []Op {
	var leaves []Op
	cur := Op(j)
	for {
		jn, ok := cur.(*Join)
		if !ok || len(jn.On) != 0 {
			leaves = append([]Op{cur}, leaves...)
			return leaves
		}
		leaves = append([]Op{jn.Right}, leaves...)
		cur = jn.Left
	}
}

// buildLeftDeep rebuilds a cartesian join over the given inputs, left-deep, keeping
// the smaller side of each join on the right build side. The inputs arrive sorted
// ascending, so the running product is the smaller side of the next join until it
// outgrows a later input, at which point the swap keeps that input building.
func buildLeftDeep(leaves []Op, st Statistics) Op {
	cur := leaves[0]
	for _, leaf := range leaves[1:] {
		l, r := cur, leaf
		if EstimateRows(l, st) < EstimateRows(r, st) {
			l, r = r, l
		}
		cur = &Join{Left: l, Right: r}
	}
	return cur
}

// hasArgument reports whether a subtree roots a correlated input anywhere, the
// Argument leaf an outer row feeds. It walks the operator children the same way the
// renderers and the cost model do.
func hasArgument(o Op) bool {
	if _, ok := o.(*Argument); ok {
		return true
	}
	for _, c := range nodeChildren(o) {
		if hasArgument(c) {
			return true
		}
	}
	return false
}
