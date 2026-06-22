package exec

import (
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// shortestPathOp finds the shortest path(s) between two already-bound endpoint
// nodes (shortestPath / allShortestPaths, doc 09 §3.4, doc 12 §4.4). For each
// input row it reads the source and target nodes, runs a level-synchronized
// breadth-first search from the source respecting the type set, direction, and
// hop range, and reconstructs the shortest path(s) to the target. shortestPath
// emits one path (none when the target is unreachable); allShortestPaths emits
// every path of the minimum length. The relationship variable binds the path's
// relationship list and, when the pattern is named, the path variable binds the
// full walk including its intermediate nodes.
//
// Searching node-by-node and marking each node at its first-discovery level makes
// every path it finds simple (no node repeats), which is also relationship-unique
// (doc 02 §4.3), so the trail constraint holds without a separate check. The
// search is the naïve correct form M2 ships; the bidirectional refinement that
// halves the explored frontier is M4 (doc 25 §5.4).
type shortestPathOp struct {
	spec  *plan.ShortestPath
	input operator
	peers []string // sibling relationship variables a path's edges must avoid
	ctx   *Ctx

	relTok engine.Token
	allow  map[engine.Token]bool
	noType bool
	min    int
	max    int // -1 for unbounded

	out []eval.Row
	pos int
}

func (o *shortestPathOp) open(ctx *Ctx) error {
	o.ctx, o.out, o.pos = ctx, nil, 0
	o.relTok, o.allow, o.noType = resolveTypes(o.spec.Types)
	if o.spec.VarLen != nil {
		o.min, o.max = normVarLen(o.spec.VarLen)
	} else {
		o.min, o.max = 1, 1 // a fixed single-hop shortest path
	}
	return o.input.open(ctx)
}

func (o *shortestPathOp) next() (eval.Row, bool, error) {
	for {
		if o.pos < len(o.out) {
			row := o.out[o.pos]
			o.pos++
			return row, true, nil
		}
		in, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		if err := o.loadPaths(in); err != nil {
			return nil, false, err
		}
	}
}

// predEdge records how a node was reached during the search: the predecessor node
// and the relationship traversed to it.
type predEdge struct {
	node engine.NodeID
	rel  engine.RelID
}

// loadPaths runs the search for one input row and buffers its result rows. A null
// endpoint (an unmatched OPTIONAL variable) or an unreachable target yields none.
// allShortestPaths walks the full predecessor DAG with a one-directional BFS;
// shortestPath uses a bidirectional BFS that meets in the middle, so it explores
// two radius-L/2 frontiers instead of one radius-L frontier (doc 25 §5.4).
func (o *shortestPathOp) loadPaths(row eval.Row) error {
	o.out, o.pos = o.out[:0], 0
	srcV, ok := row[o.spec.From].AsNode()
	if !ok {
		return nil
	}
	dstV, ok := row[o.spec.To].AsNode()
	if !ok {
		return nil
	}
	src, dst := engine.NodeID(srcV), engine.NodeID(dstV)

	// The zero-length path: the endpoints coincide and the range admits zero hops.
	// With coincident endpoints the empty path is the shortest, so a positive
	// minimum, which it cannot satisfy, yields nothing.
	if src == dst {
		if o.min == 0 {
			o.emitWalk(row, []engine.NodeID{src}, nil)
		}
		return nil
	}
	if o.noType {
		return nil // no relationship type can match, so no non-empty path exists
	}

	forbidden := collectPeerRels(row, o.peers)
	if o.spec.All {
		return o.searchAll(row, src, dst, forbidden)
	}
	return o.searchBidi(row, src, dst, forbidden)
}

// searchAll runs a single-directional level-synchronized BFS from the source and
// emits every shortest path by walking the predecessor DAG (allShortestPaths).
// The level at which the target is first discovered is the shortest distance;
// finishing that level captures every equal-length predecessor before stopping.
func (o *shortestPathOp) searchAll(row eval.Row, src, dst engine.NodeID, forbidden map[engine.RelID]bool) error {
	level := map[engine.NodeID]int{src: 0}
	preds := map[engine.NodeID][]predEdge{}
	frontier := []engine.NodeID{src}
	dir := toEngineDir(o.spec.Dir)
	found := false
	for d := 0; len(frontier) > 0 && !found; d++ {
		if o.max >= 0 && d >= o.max {
			break
		}
		var nextFront []engine.NodeID
		for _, n := range frontier {
			err := o.ctx.Tx.Expand(n, o.relTok, dir, func(nb engine.Neighbor) error {
				if o.allow != nil && !o.allow[nb.Type] {
					return nil
				}
				if forbidden[nb.Rel] {
					return nil
				}
				if nl, seen := level[nb.Node]; !seen {
					level[nb.Node] = d + 1
					preds[nb.Node] = append(preds[nb.Node], predEdge{n, nb.Rel})
					nextFront = append(nextFront, nb.Node)
					if nb.Node == dst {
						found = true
					}
				} else if nl == d+1 {
					// Another shortest predecessor reached at the same level.
					preds[nb.Node] = append(preds[nb.Node], predEdge{n, nb.Rel})
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		frontier = nextFront
	}

	dist, ok := level[dst]
	if !ok || dist < o.min {
		return nil // unreachable, or shorter than the range admits
	}
	o.emitAll(row, preds, src, dst)
	return nil
}

// searchBidi runs a bidirectional BFS for a single shortest path. It grows a
// forward frontier from the source and a backward frontier from the target (the
// backward search walks the reverse direction), expanding whichever side is
// cheaper to expand next, measured by the total degree of its frontier nodes, so
// the search grows the smaller frontier and meets near the middle. A node settled
// on both sides is a meeting point; the shortest distance is the smallest
// forward-plus-backward depth over all meeting points.
//
// The stop rule fdepth+bdepth >= best is sound for any alternation, not only a
// balanced one. Each side completes whole levels, so after fdepth forward levels
// and bdepth backward levels every node within fdepth of the source and within
// bdepth of the target has its exact distance. A shortest path of length L has a
// node at forward distance k for every k in 0..L, and the node at k=fdepth then
// sits at backward distance L-fdepth; once fdepth+bdepth >= L that node is settled
// on both sides, so the meeting on the optimal path is found and best <= L. Thus
// when fdepth+bdepth reaches best no shorter path can remain unfound, whatever
// order the two sides were expanded in. Expanding the cheaper side just minimizes
// the work to reach that point; it does not change which paths are found.
//
// The result is one shortest path. Any walk whose length equals the shortest
// distance is necessarily simple (a repeated node would let it be shortened below
// the minimum), so the reconstructed path satisfies the trail constraint without a
// separate check, exactly as the one-directional search does.
func (o *shortestPathOp) searchBidi(row eval.Row, src, dst engine.NodeID, forbidden map[engine.RelID]bool) error {
	dir := toEngineDir(o.spec.Dir)
	rev := reverseDir(dir)
	fdist := map[engine.NodeID]int{src: 0}
	bdist := map[engine.NodeID]int{dst: 0}
	fpred := map[engine.NodeID]predEdge{} // node -> how the forward search reached it
	bnext := map[engine.NodeID]predEdge{} // node -> the forward edge toward the target
	ff := []engine.NodeID{src}
	bf := []engine.NodeID{dst}
	fdepth, bdepth := 0, 0
	best := -1
	var meet engine.NodeID
	consider := func(x engine.NodeID) {
		fa, okF := fdist[x]
		fb, okB := bdist[x]
		if okF && okB {
			if c := fa + fb; best < 0 || c < best {
				best, meet = c, x
			}
		}
	}

	for len(ff) > 0 || len(bf) > 0 {
		if best >= 0 && fdepth+bdepth >= best {
			break // no unfound meeting can beat the one already found
		}
		if o.max >= 0 && fdepth+bdepth >= o.max {
			break // a longer path would exceed the hop bound
		}
		// Expand the cheaper, non-empty side: the one whose frontier has the smaller
		// total degree, so the search grows the smaller of the two frontiers. The
		// engine's degree is O(1) per node, so scoring both frontiers is far cheaper
		// than the expand it steers. With one side empty, expand the other.
		forward := len(ff) > 0
		if forward && len(bf) > 0 {
			fc, err := o.frontierCost(ff, dir)
			if err != nil {
				return err
			}
			bc, err := o.frontierCost(bf, rev)
			if err != nil {
				return err
			}
			forward = fc <= bc
		}
		if forward {
			var nf []engine.NodeID
			for _, n := range ff {
				err := o.ctx.Tx.Expand(n, o.relTok, dir, func(nb engine.Neighbor) error {
					if o.allow != nil && !o.allow[nb.Type] {
						return nil
					}
					if forbidden[nb.Rel] {
						return nil
					}
					if _, seen := fdist[nb.Node]; !seen {
						fdist[nb.Node] = fdepth + 1
						fpred[nb.Node] = predEdge{n, nb.Rel}
						nf = append(nf, nb.Node)
						consider(nb.Node)
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			ff, fdepth = nf, fdepth+1
		} else {
			var nb2 []engine.NodeID
			for _, n := range bf {
				err := o.ctx.Tx.Expand(n, o.relTok, rev, func(nb engine.Neighbor) error {
					if o.allow != nil && !o.allow[nb.Type] {
						return nil
					}
					if forbidden[nb.Rel] {
						return nil
					}
					if _, seen := bdist[nb.Node]; !seen {
						bdist[nb.Node] = bdepth + 1
						// The reverse expand reached nb from n, so the forward edge runs
						// from nb to n: nb's next hop toward the target is that edge.
						bnext[nb.Node] = predEdge{n, nb.Rel}
						nb2 = append(nb2, nb.Node)
						consider(nb.Node)
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			bf, bdepth = nb2, bdepth+1
		}
	}

	if best < 0 || best < o.min {
		return nil // unreachable, or shorter than the range admits
	}

	// Reconstruct the single path in source-first order: the forward half walks
	// fpred from the meeting node back to the source, the backward half walks bnext
	// from the meeting node forward to the target.
	var nodes []engine.NodeID
	var rels []engine.RelID
	revN := []engine.NodeID{meet}
	var revR []engine.RelID
	for cur := meet; cur != src; {
		pe := fpred[cur]
		revR = append(revR, pe.rel)
		revN = append(revN, pe.node)
		cur = pe.node
	}
	for i := len(revN) - 1; i >= 0; i-- {
		nodes = append(nodes, revN[i])
	}
	for i := len(revR) - 1; i >= 0; i-- {
		rels = append(rels, revR[i])
	}
	for cur := meet; cur != dst; {
		be := bnext[cur]
		rels = append(rels, be.rel)
		nodes = append(nodes, be.node)
		cur = be.node
	}
	o.emitForward(row, nodes, rels)
	return nil
}

// frontierCost sums the degree of a frontier's nodes along a direction, the
// number of edges expanding that frontier one level would traverse. It steers the
// bidirectional search toward the cheaper side. The relationship token is the
// resolved single type or the wildcard for a multi-type or untyped pattern, so the
// sum is an upper bound when an allow-set filters the expand, which is fine for a
// heuristic that only has to order two sides.
func (o *shortestPathOp) frontierCost(front []engine.NodeID, dir engine.Direction) (int64, error) {
	var sum int64
	for _, n := range front {
		d, err := o.ctx.Tx.Degree(n, o.relTok, dir)
		if err != nil {
			return 0, err
		}
		sum += d
	}
	return sum, nil
}

// reverseDir flips an expand direction for the backward half of a bidirectional
// search: outgoing and incoming swap, both stays both.
func reverseDir(d engine.Direction) engine.Direction {
	switch d {
	case engine.Outgoing:
		return engine.Incoming
	case engine.Incoming:
		return engine.Outgoing
	default:
		return engine.Both
	}
}

// emitForward emits one path given its nodes and relationships in source-first
// order. emitWalk takes them target-first, so this reverses before delegating.
func (o *shortestPathOp) emitForward(row eval.Row, nodes []engine.NodeID, rels []engine.RelID) {
	nr := make([]engine.NodeID, len(nodes))
	for i, n := range nodes {
		nr[len(nodes)-1-i] = n
	}
	rr := make([]engine.RelID, len(rels))
	for i, r := range rels {
		rr[len(rels)-1-i] = r
	}
	o.emitWalk(row, nr, rr)
}

// emitAll enumerates every shortest path by walking the full predecessor DAG from
// the target back to the source, one emitted path per distinct predecessor chain.
func (o *shortestPathOp) emitAll(row eval.Row, preds map[engine.NodeID][]predEdge, src, dst engine.NodeID) {
	var walk func(cur engine.NodeID, nodes []engine.NodeID, rels []engine.RelID)
	walk = func(cur engine.NodeID, nodes []engine.NodeID, rels []engine.RelID) {
		if cur == src {
			o.emitWalk(row, nodes, rels)
			return
		}
		for _, pe := range preds[cur] {
			// Copy the accumulators so sibling branches do not share backing arrays.
			nn := append(append([]engine.NodeID(nil), nodes...), pe.node)
			rr := append(append([]engine.RelID(nil), rels...), pe.rel)
			walk(pe.node, nn, rr)
		}
	}
	walk(dst, []engine.NodeID{dst}, nil)
}

// emitWalk appends one result row for a path given its nodes and relationships in
// target-first order (the order reconstruction produces). It reverses them to
// source-first, builds the relationship-list binding and, for a named pattern,
// the path value, and clones the input row to carry both.
func (o *shortestPathOp) emitWalk(row eval.Row, nodesRev []engine.NodeID, relsRev []engine.RelID) {
	n := len(nodesRev)
	nodes := make([]engine.NodeID, n)
	for i, nd := range nodesRev {
		nodes[n-1-i] = nd
	}
	m := len(relsRev)
	rels := make([]engine.RelID, m)
	for i, r := range relsRev {
		rels[m-1-i] = r
	}

	out := cloneRow(row)
	out[o.spec.Rel] = relList(rels)
	if o.spec.PathVar != "" {
		elems := make([]value.Value, 0, len(nodes)+len(rels))
		for i, nd := range nodes {
			elems = append(elems, value.Node(uint64(nd)))
			if i < len(rels) {
				elems = append(elems, value.Rel(uint64(rels[i])))
			}
		}
		out[o.spec.PathVar] = value.Path(elems...)
	}
	o.out = append(o.out, out)
}

func (o *shortestPathOp) close() error { return o.input.close() }
