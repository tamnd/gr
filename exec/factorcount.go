package exec

import (
	"runtime"
	"sync"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// expandCountOp is the executor for a factorized count (doc 11 §7, §8). It stands
// in for an Aggregate over an Expand: instead of building one row per edge and
// counting the rows, it counts the edges each input row expands to and emits one
// tally row. The count it produces is exactly the row count the Expand+Aggregate
// would have, because it applies the same per-edge filters the Expand applied (the
// type set and relationship-uniqueness against sibling edges) and the replaced
// expand carried no target-label, expand-into, or variable-length constraint (the
// rewrite only fires when those are absent), so every edge it counts is one the
// expand would have emitted a row for.
//
// Like a grouping-free aggregate it always emits exactly one row, the count zero
// included: an empty input, a null source on every row, or an all-unknown type set
// each yield a single row carrying zero, the empty-group rule the aggregate it
// replaced follows.
type expandCountOp struct {
	spec  *plan.ExpandCount
	input operator
	peers []string // sibling relationship variables a counted edge must differ from
	ctx   *Ctx

	cur  eval.Row // the input row whose edges are being counted
	done bool

	relTok engine.Token          // the single type token to expand, or 0 for all
	allow  map[engine.Token]bool // post-filter type set when more than one type
	noType bool                  // every named type is unknown: no edge matches
}

func (o *expandCountOp) open(ctx *Ctx) error {
	o.ctx, o.cur, o.done = ctx, nil, false
	o.relTok, o.allow, o.noType = resolveTypes(o.spec.Types)
	return o.input.open(ctx)
}

func (o *expandCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	if !o.noType {
		for {
			in, ok, err := o.input.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			n, err := o.countRow(in)
			if err != nil {
				return nil, false, err
			}
			total += n
		}
	}
	// One tally row stands in for total flat rows, so the factorization ratio is total: an
	// Expand+Aggregate would have built total rows to count, this built one (doc 20 §6.3).
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// countRow counts the edges one input row expands to, applying the type set and
// relationship-uniqueness filters the replaced Expand applied. A null source (an
// unmatched OPTIONAL variable) contributes nothing.
func (o *expandCountOp) countRow(in eval.Row) (int64, error) {
	src, ok := in[o.spec.From].AsNode()
	if !ok {
		return 0, nil
	}
	o.cur = in
	var n int64
	dir := toEngineDir(o.spec.Dir)
	err := o.ctx.Tx.Expand(engine.NodeID(src), o.relTok, dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.allow != nil && !o.allow[nb.Type] {
			return nil
		}
		if !o.unique(nb.Rel) {
			return nil
		}
		n++
		return nil
	})
	return n, err
}

// unique enforces relationship-uniqueness: a counted edge must not already be bound
// to a sibling relationship variable in the same pattern (doc 02 §4.3), the same
// check the Expand it replaced applied.
func (o *expandCountOp) unique(rel engine.RelID) bool {
	for _, p := range o.peers {
		if v, ok := o.cur[p]; ok && relValueContains(v, rel) {
			return false
		}
	}
	return true
}

func (o *expandCountOp) close() error { return o.input.close() }

// productCountOp is the executor for a factorized product count (doc 11 §7, §8): it
// counts the rows the cross-product of two or more independent expands from a shared
// source would produce, without building that product. For each input row it reads
// the source node and counts the matching edges along each leg, multiplies the
// per-leg degrees, and sums the product over the source rows. Because the legs leave
// the source along disjoint relationship types, no edge is counted by two legs and no
// relationship-uniqueness couples them, so the product is exactly the row count the
// naive plan's cross-product would have.
//
// Like a grouping-free aggregate it always emits exactly one row, zero included: an
// empty input or a source whose every leg has degree zero yields a single tally row.
type productCountOp struct {
	spec  *plan.ProductCount
	input operator
	ctx   *Ctx

	done bool
	legs []resolvedLeg
}

// resolvedLeg is one leg's expand parameters resolved once at open: the type token
// to expand (or zero for all), the multi-type allow set, whether the type set
// matches nothing, and the engine direction.
type resolvedLeg struct {
	relTok engine.Token
	allow  map[engine.Token]bool
	noType bool
	dir    engine.Direction
}

func (o *productCountOp) open(ctx *Ctx) error {
	o.ctx, o.done = ctx, false
	o.legs = make([]resolvedLeg, len(o.spec.Legs))
	for i, l := range o.spec.Legs {
		tok, allow, none := resolveTypes(l.Types)
		o.legs[i] = resolvedLeg{relTok: tok, allow: allow, noType: none, dir: toEngineDir(l.Dir)}
	}
	return o.input.open(ctx)
}

func (o *productCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	for {
		in, ok, err := o.input.next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			break
		}
		n, err := o.countRow(in)
		if err != nil {
			return nil, false, err
		}
		total += n
	}
	// One tally row stands in for the cross-product's total rows, so the ratio is total: the
	// naive plan would have built total product rows, this built one (doc 20 §6.3).
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// countRow returns the product of the source's per-leg degrees for one input row, a
// null or absent source contributing nothing. A leg whose type set matches nothing
// has degree zero, so the whole product is zero for that row.
func (o *productCountOp) countRow(in eval.Row) (int64, error) {
	src, ok := in[o.spec.From].AsNode()
	if !ok {
		return 0, nil
	}
	product := int64(1)
	for _, leg := range o.legs {
		if leg.noType {
			return 0, nil
		}
		var deg int64
		err := o.ctx.Tx.Expand(engine.NodeID(src), leg.relTok, leg.dir, func(nb engine.Neighbor) error {
			o.ctx.countScan(1)
			if leg.allow != nil && !leg.allow[nb.Type] {
				return nil
			}
			deg++
			return nil
		})
		if err != nil {
			return 0, err
		}
		if deg == 0 {
			return 0, nil
		}
		product *= deg
	}
	return product, nil
}

func (o *productCountOp) close() error { return o.input.close() }

// intersectCountOp is the executor for a fused triangle count (doc 11 §5, §7; doc 12
// §5.2). It stands in for an Aggregate over a WCOJ Intersect: instead of feeding the
// intersection from a materialized mid expand and counting one apex row at a time, it
// drives the mid expand itself and merge-counts the apex matches per middle node, so
// it builds neither a mid row nor an apex row. For each anchor it reads the hub leg's
// apex candidates once (they depend only on the anchor), expands the anchor along the
// mid edge to enumerate the middle node, and for each middle node merge-intersects its
// mid-leg neighbors with the hub candidates on dense position, tallying every apex the
// two reach where the three triangle edges are pairwise distinct and the apex carries
// the required labels. The tally equals the row count the Intersect+Aggregate would
// produce, because it applies the same per-leg type filters, the same
// relationship-uniqueness, and the same apex labels the Intersect applied.
//
// Like a grouping-free aggregate it always emits exactly one row, zero included.
type intersectCountOp struct {
	spec      *plan.IntersectCount
	input     operator
	inputPlan plan.Op // the logical input, recompiled per worker on the parallel path
	ctx       *Ctx

	done bool

	midTok    engine.Token          // the mid expand's single type token, or 0 for all
	midAllow  map[engine.Token]bool // mid expand post-filter when more than one type
	midNoType bool                  // the mid expand's every named type is unknown
	midDir    engine.Direction

	hub resolvedLeg // the leg reaching the apex from the anchor
	mid resolvedLeg // the leg reaching the apex from the middle node

	midLabelsKnown bool // every middle-node label is known: an unknown one matches nothing

	adjx engine.Adjacency
	adjr engine.AdjacencyReader
}

// icScratch is one counting goroutine's reusable merge-intersection buffers: the hub
// candidates (read once per anchor) and the mid-leg neighbors (read once per middle
// node), each a distinct buffer so the hub list stays valid while the mid list is
// refilled (engine.Adjacency contract). Each worker on the parallel path holds its
// own, so the shared op carries no mutable scan state and the workers never race.
type icScratch struct {
	hub    []engine.PosNeighbor
	mid    []engine.PosNeighbor
	reader engine.NeighborReader // reusable per-worker neighbor reader, nil if the engine has none
}

func (o *intersectCountOp) open(ctx *Ctx) error {
	o.ctx, o.done = ctx, false
	o.midTok, o.midAllow, o.midNoType = resolveTypes(o.spec.MidTypes)
	htok, hallow, hnone := resolveTypes(o.spec.HubLeg.Types)
	o.hub = resolvedLeg{relTok: htok, allow: hallow, noType: hnone, dir: toEngineDir(o.spec.HubLeg.Dir)}
	mtok, mallow, mnone := resolveTypes(o.spec.MidLeg.Types)
	o.mid = resolvedLeg{relTok: mtok, allow: mallow, noType: mnone, dir: toEngineDir(o.spec.MidLeg.Dir)}
	o.midDir = toEngineDir(o.spec.MidDir)
	o.midLabelsKnown = true
	for _, l := range o.spec.MidLabels {
		if !l.Known {
			o.midLabelsKnown = false
		}
	}
	o.adjx, _ = ctx.Tx.(engine.Adjacency)
	o.adjr, _ = ctx.Tx.(engine.AdjacencyReader)
	return o.input.open(ctx)
}

func (o *intersectCountOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	var total int64
	var err error
	switch {
	case o.midNoType || o.hub.noType || o.mid.noType || !o.midLabelsKnown:
		// A type set that resolves to nothing or an unknown middle-node label matches
		// no edge or node, so the count is zero with no scan.
		total = 0
	default:
		if ns := o.parallelScan(); ns != nil {
			total, err = o.runParallel(ns)
		} else {
			total, err = o.runSerial()
		}
	}
	if err != nil {
		return nil, false, err
	}
	// One tally row stands in for total flat rows, so the factorization ratio is total:
	// the Intersect+Aggregate would have built total apex rows to count, this built one
	// (doc 20 §6.3).
	o.ctx.countFactorized()
	if total > 0 {
		o.ctx.countFactorizationRatio(float64(total))
	}
	return eval.Row{o.spec.Col: value.Int(total)}, true, nil
}

// runSerial drains the anchor rows on this goroutine, summing the triangles each
// closes with one set of reused merge buffers.
func (o *intersectCountOp) runSerial() (int64, error) {
	var s icScratch
	defer o.bindReader(&s)()
	if leaf, ok := o.input.(*nodeScanOp); ok {
		// The whole input is a bare node scan, so feed the count off its ids directly.
		return o.countScanDirect(&s, leaf)
	}
	var total int64
	for {
		in, ok, err := o.input.next()
		if err != nil {
			return 0, err
		}
		if !ok {
			return total, nil
		}
		n, err := o.countRow(&s, in)
		if err != nil {
			return 0, err
		}
		total += n
	}
}

// countScanDirect counts the triangles closed at every anchor a bare node scan
// emits, reading the anchor ids straight from the scan and applying its residual
// label filter in place. It never allocates the single-entry anchor row map the
// pull path builds per node, so on a million-anchor scan it removes a million short
// lived maps, the dominant query-path allocation and the source of the GC tail on
// triangle counting (doc 12 §10). It matches the pull path exactly: same ids, same
// label filter, same per-anchor merge count.
func (o *intersectCountOp) countScanDirect(s *icScratch, leaf *nodeScanOp) (int64, error) {
	var total int64
	for _, id := range leaf.scanIDs() {
		ok, err := leaf.hasAll(id)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		n, err := o.countMerge(s, engine.NodeID(id))
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// parallelScan returns the NodeScan to parallelize the count over, or nil to run
// serially. The count is a pure sum reduction over the anchor rows, so it is eligible
// exactly when the aggregate it replaced would have been: a read-only, row-independent
// chain rooted at a node scan, on a machine with more than one core. Triangle counting
// is the canonical many-core workload, so this is where the fused operator earns the
// cores Kuzu uses (doc 12 §10).
func (o *intersectCountOp) parallelScan() *plan.NodeScan {
	if o.ctx.Effects != nil || o.inputPlan == nil {
		return nil
	}
	if runtime.GOMAXPROCS(0) < 2 {
		return nil
	}
	return parallelLeaf(o.inputPlan)
}

// runParallel scans the anchor nodes once, fans the morsels across workers that each
// sum the triangles their morsels close, and adds the partials. It falls back to the
// serial path when the scan is too small to be worth splitting. The sum is associative
// and commutative, so the parallel total is identical to the serial one.
func (o *intersectCountOp) runParallel(ns *plan.NodeScan) (int64, error) {
	ids, err := primaryScan(o.ctx.Tx, ns)
	if err != nil {
		return 0, err
	}
	w := parallelWorkers(len(ids))
	if w < 2 {
		return o.runSerial()
	}
	// The morsel workers walk this already-scanned id slice rather than scanning again,
	// so the anchor scan work is counted once here (doc 20 §3.1) instead of per worker.
	o.ctx.countScan(int64(len(ids)))
	src := &morselSource{ids: ids, chunk: morselChunk}
	partials := make([]int64, w)
	errs := make([]error, w)
	var wg sync.WaitGroup
	for k := range w {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			partials[k], errs[k] = o.countWorker(ids, src)
		}(k)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return 0, e
		}
	}
	var total int64
	for _, n := range partials {
		total += n
	}
	return total, nil
}

// countWorker runs one worker: a fresh, private copy of the anchor pipeline pointed at
// the shared morsel cursor, summing the triangles closed by every anchor in the morsels
// it pulls. The copy, its scan window, and its merge buffers are unshared, and every
// read goes through the concurrency-safe read path, so workers run without coordination
// beyond the atomic morsel handoff.
func (o *intersectCountOp) countWorker(ids []engine.NodeID, src *morselSource) (int64, error) {
	op, err := compile(o.inputPlan)
	if err != nil {
		return 0, err
	}
	leaf := scanLeaf(op)
	if leaf == nil {
		return 0, errNoScanLeaf
	}
	leaf.windowed = true
	leaf.winIDs = ids
	// When the pipeline is just the scan, each morsel feeds the count off the window's
	// ids directly with no per-anchor row map (the bare-scan fast path); otherwise the
	// worker pulls rows through the intervening operators as before.
	bareScan := op == operator(leaf)
	var s icScratch
	defer o.bindReader(&s)()
	var total int64
	for {
		lo, hi, ok := src.take()
		if !ok {
			return total, nil
		}
		leaf.winLo, leaf.winHi = lo, hi
		if err := op.open(o.ctx); err != nil {
			return 0, err
		}
		if bareScan {
			n, err := o.countScanDirect(&s, leaf)
			if err != nil {
				_ = op.close()
				return 0, err
			}
			total += n
			if err := op.close(); err != nil {
				return 0, err
			}
			continue
		}
		for {
			in, ok, err := op.next()
			if err != nil {
				_ = op.close()
				return 0, err
			}
			if !ok {
				break
			}
			n, err := o.countRow(&s, in)
			if err != nil {
				_ = op.close()
				return 0, err
			}
			total += n
		}
		if err := op.close(); err != nil {
			return 0, err
		}
	}
}

// countRow counts the triangles closed at one anchor row, a null or absent anchor
// contributing nothing. It takes the merge-intersection path when the engine exposes
// position-sorted adjacency, falling back to a hash probe otherwise.
func (o *intersectCountOp) countRow(s *icScratch, in eval.Row) (int64, error) {
	a, ok := in[o.spec.Hub].AsNode()
	if !ok {
		return 0, nil
	}
	if o.adjx != nil {
		return o.countMerge(s, engine.NodeID(a))
	}
	return o.countHash(s, engine.NodeID(a))
}

// bindReader gives a worker its reusable neighbor reader when the engine exposes
// one, returning a release func the caller defers. The reader holds one liveness
// cursor and one visibility closure for the worker's whole run, so the hot per-edge
// NeighborsByPos calls in countMerge allocate neither per call. When the engine has
// no reader the buffer-only Adjacency path stands in and release is a no-op.
func (o *intersectCountOp) bindReader(s *icScratch) func() {
	if o.adjr == nil {
		return func() {}
	}
	s.reader = o.adjr.NewNeighborReader()
	return s.reader.Close
}

// neighbors fetches a node's position-sorted neighbors through the worker's reusable
// reader when one is bound, falling back to the per-call Adjacency path otherwise.
func (o *intersectCountOp) neighbors(s *icScratch, id engine.NodeID, relTok engine.Token, dir engine.Direction, buf []engine.PosNeighbor) ([]engine.PosNeighbor, error) {
	if s.reader != nil {
		return s.reader.NeighborsByPos(id, relTok, dir, buf)
	}
	return o.adjx.NeighborsByPos(id, relTok, dir, buf)
}

// countMerge reads the anchor's hub candidates once, drives the mid expand to
// enumerate the middle node, and for each middle node merge-intersects its mid-leg
// neighbors with the hub candidates on dense position. It never materializes an apex
// row: each apex match is tallied in place.
func (o *intersectCountOp) countMerge(s *icScratch, a engine.NodeID) (int64, error) {
	hub, err := o.neighbors(s, a, o.hub.relTok, o.hub.dir, s.hub)
	if err != nil {
		return 0, err
	}
	s.hub = hub
	if len(hub) == 0 {
		return 0, nil
	}
	o.ctx.countScan(int64(len(hub)))
	var total int64
	err = o.ctx.Tx.Expand(a, o.midTok, o.midDir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.midAllow != nil && !o.midAllow[nb.Type] {
			return nil
		}
		if ok, e := o.hasMidLabels(nb.Node); e != nil || !ok {
			return e
		}
		mid, e := o.neighbors(s, nb.Node, o.mid.relTok, o.mid.dir, s.mid)
		if e != nil {
			return e
		}
		s.mid = mid
		if len(mid) == 0 {
			return nil
		}
		o.ctx.countScan(int64(len(mid)))
		n, e := o.mergeCount(hub, mid, nb.Rel)
		if e != nil {
			return e
		}
		total += n
		return nil
	})
	return total, err
}

// mergeCount walks the hub candidates and a middle node's mid-leg neighbors, both
// sorted ascending by dense position, with two cursors and tallies every apex they
// both reach (doc 12 §5.2). Equal positions denote the same apex; a multigraph can
// have several edges to it on each side, so the matching runs are paired as a small
// Cartesian product, counting only pairs whose three edges (the mid expand edge rel1
// and the two leg edges) are pairwise distinct, for an apex carrying the labels.
func (o *intersectCountOp) mergeCount(hub, mid []engine.PosNeighbor, rel1 engine.RelID) (int64, error) {
	var total int64
	i, j := 0, 0
	for i < len(hub) && j < len(mid) {
		switch {
		case hub[i].Pos < mid[j].Pos:
			i++
		case hub[i].Pos > mid[j].Pos:
			j++
		default:
			p := hub[i].Pos
			i0, j0 := i, j
			for i < len(hub) && hub[i].Pos == p {
				i++
			}
			for j < len(mid) && mid[j].Pos == p {
				j++
			}
			ok, err := o.hasLabels(hub[i0].Node)
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			for _, x := range hub[i0:i] {
				if o.hub.allow != nil && !o.hub.allow[x.Type] {
					continue
				}
				if x.Rel == rel1 {
					continue
				}
				for _, y := range mid[j0:j] {
					if o.mid.allow != nil && !o.mid.allow[y.Type] {
						continue
					}
					if y.Rel == rel1 || y.Rel == x.Rel {
						continue
					}
					total++
				}
			}
		}
	}
	return total, nil
}

// countHash is the fallback when the engine exposes no position-sorted adjacency: it
// keys the anchor's hub candidates by apex node, then drives the mid expand and, per
// middle node, probes the hub map with its mid-leg neighbors. The set of triangles it
// tallies is the same the merge path tallies; only the access path differs.
func (o *intersectCountOp) countHash(_ *icScratch, a engine.NodeID) (int64, error) {
	hub := map[engine.NodeID][]engine.RelID{}
	err := o.ctx.Tx.Expand(a, o.hub.relTok, o.hub.dir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.hub.allow != nil && !o.hub.allow[nb.Type] {
			return nil
		}
		hub[nb.Node] = append(hub[nb.Node], nb.Rel)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(hub) == 0 {
		return 0, nil
	}
	var total int64
	err = o.ctx.Tx.Expand(a, o.midTok, o.midDir, func(nb engine.Neighbor) error {
		o.ctx.countScan(1)
		if o.midAllow != nil && !o.midAllow[nb.Type] {
			return nil
		}
		if ok, e := o.hasMidLabels(nb.Node); e != nil || !ok {
			return e
		}
		rel1 := nb.Rel
		return o.ctx.Tx.Expand(nb.Node, o.mid.relTok, o.mid.dir, func(mb engine.Neighbor) error {
			o.ctx.countScan(1)
			if o.mid.allow != nil && !o.mid.allow[mb.Type] {
				return nil
			}
			if mb.Rel == rel1 {
				return nil
			}
			hrels := hub[mb.Node]
			if len(hrels) == 0 {
				return nil
			}
			ok, err := o.hasLabels(mb.Node)
			if err != nil || !ok {
				return err
			}
			for _, hr := range hrels {
				if hr == rel1 || hr == mb.Rel {
					continue
				}
				total++
			}
			return nil
		})
	})
	return total, err
}

func (o *intersectCountOp) hasLabels(id engine.NodeID) (bool, error) {
	for _, l := range o.spec.Labels {
		if !l.Known {
			return false, nil
		}
		has, err := o.ctx.Tx.HasLabel(id, l.Token)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

// hasMidLabels reports whether the middle node carries every label the mid expand's
// target required. next() has already shown every middle-node label is known (an
// unknown one collapses the whole count to zero), so this only tests presence.
func (o *intersectCountOp) hasMidLabels(id engine.NodeID) (bool, error) {
	for _, l := range o.spec.MidLabels {
		has, err := o.ctx.Tx.HasLabel(id, l.Token)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

func (o *intersectCountOp) close() error { return o.input.close() }
