package plan

// JoinOrder picks the build side of each hash join from estimated cardinalities
// (doc 11 §3, §4; doc 12 §5.1). The executor builds its hash table over a join's
// right input and probes it with the left, so the right side is the one held in
// memory: the plan runs leanest when the right side is the smaller of the two.
// The builder always puts the newly joined leaf on the right, in pattern order,
// which is not a size choice, so this rewrite swaps a join whose left side the
// cost model estimates is smaller, leaving the smaller side on the build side.
//
// It runs on the normalized tree so the estimates see filter pushdown: a side
// whose equality was pushed down to its scan is costed at its reduced size, not
// the size before the filter moved. With nil statistics it is the identity, so
// the structural plan keeps the builder's pattern-order sides and the planner
// goldens are unchanged.
//
// The swap is meaning-preserving. A hash join is an inner equijoin (or a
// cartesian product when the keys are empty), and a row is a map keyed by
// variable, so merging a left row with a right row yields the same binding
// whichever side each came from. Swapping changes only the order rows are
// emitted, which an unordered Cypher result does not observe.
func JoinOrder(o Op, st Statistics) Op {
	if st == nil {
		return o
	}
	return joinOrder(o, st)
}

func joinOrder(o Op, st Statistics) Op {
	o = mapChildren(o, func(c Op) Op { return joinOrder(c, st) })
	j, ok := o.(*Join)
	if !ok {
		return o
	}
	// A correlated join (one side rooted on an Argument fed by an outer row) is
	// left as built: its sides are not two free inputs to size against each other,
	// and the executor feeds the outer row in on a fixed side. Only a join of two
	// independent patterns is a build-side choice.
	if hasArgument(j.Left) || hasArgument(j.Right) {
		return o
	}
	if EstimateRows(j.Left, st) < EstimateRows(j.Right, st) {
		return &Join{Left: j.Right, Right: j.Left, On: j.On}
	}
	return o
}

// hasArgument reports whether a subtree roots a correlated input anywhere, the
// Argument leaf an outer row feeds. It walks the operator children the same way
// the renderers and the cost model do.
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
