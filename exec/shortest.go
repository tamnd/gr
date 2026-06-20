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
	if src == dst && o.min == 0 {
		o.emitWalk(row, []engine.NodeID{src}, nil)
		return nil
	}
	if o.noType {
		return nil // no relationship type can match, so no non-empty path exists
	}

	forbidden := collectPeerRels(row, o.peers)
	level := map[engine.NodeID]int{src: 0}
	preds := map[engine.NodeID][]predEdge{}
	frontier := []engine.NodeID{src}
	dir := toEngineDir(o.spec.Dir)
	found := false
	// Expand one BFS level at a time. The level at which the target is first
	// discovered is the shortest distance; finishing that level captures every
	// equal-length predecessor (needed for allShortestPaths) before stopping.
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
	if o.spec.All {
		o.emitAll(row, preds, src, dst)
	} else {
		o.emitOne(row, preds, src, dst)
	}
	return nil
}

// emitOne reconstructs a single shortest path by following the first recorded
// predecessor at each node back from the target to the source.
func (o *shortestPathOp) emitOne(row eval.Row, preds map[engine.NodeID][]predEdge, src, dst engine.NodeID) {
	nodes := []engine.NodeID{dst}
	var rels []engine.RelID
	for cur := dst; cur != src; {
		pe := preds[cur][0]
		rels = append(rels, pe.rel)
		nodes = append(nodes, pe.node)
		cur = pe.node
	}
	o.emitWalk(row, nodes, rels)
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
