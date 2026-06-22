package exec

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
)

// Morsel-driven parallelism for an aggregation over a scan-rooted read pipeline
// (spec 2060 doc 12 §10, doc 25 §7.2 deliverable 8, the §7.4 exit criterion
// "morsel parallelism scales"). It covers a read-only RETURN count(*) / min / max
// over MATCH (n:L) ..., with or without GROUP BY. The whole input pipeline is
// independent per anchor row, so the scanned node set is cut into fixed-size
// morsels handed out by an atomic cursor, each worker runs a private copy of the
// pipeline over the morsels it pulls, and the per-morsel group buckets are merged
// at the end.
//
// This is transparent: the plan is unchanged (still Aggregate over its input),
// and the executor decides at run time whether to parallelize. The aggregates it
// parallelizes (count, min, max) are exactly associative and commutative, so the
// merged answer is byte-identical to the serial one, which keeps the M2/M3 result
// oracle green (M4 changes performance, not answers). For grouped aggregation the
// output row order matches the serial path too: morsels are disjoint id windows,
// so replaying them in ascending window order, each window in its own first-seen
// order, reproduces the serial first-seen group order exactly. sum and avg (float
// re-association would change the last bits), collect (order), and the DISTINCT
// variants (cross-row set state) stay on the serial path; widening the set is a
// later slice. The read path the workers share was made concurrency-safe first
// (impl docs 74-76): one snapshot, many reader goroutines, no shared mutable
// state below the engine's shared read lock except the now-locked caches.

// morselChunk is how many scanned node ids one morsel hands a worker. It trades
// scheduling overhead (smaller = more atomic handoffs) against load balance
// (larger = more skew at the tail). The fixed value is a starting point the
// benchmark tunes later.
const morselChunk = 1024

// morselSource hands out disjoint [lo,hi) index windows over a fixed node-id
// slice to worker goroutines. The slice is built once (a single label scan) and
// is read-only afterward; pos is the only shared mutable state and it advances
// atomically, so a worker takes a morsel with one CAS-free fetch-and-add.
type morselSource struct {
	ids   []engine.NodeID
	pos   int64
	chunk int
}

// take returns the next morsel window, or ok false when the source is drained.
func (m *morselSource) take() (lo, hi int, ok bool) {
	start := int(atomic.AddInt64(&m.pos, int64(m.chunk))) - m.chunk
	if start >= len(m.ids) {
		return 0, 0, false
	}
	end := start + m.chunk
	if end > len(m.ids) {
		end = len(m.ids)
	}
	return start, end, true
}

// parallelLeaf returns the NodeScan a plan is rooted at when every operator
// between the aggregate and the scan is read-only and row-independent (so cutting
// the scan into morsels changes no answer), or nil when the plan is not of that
// shape. A DISTINCT projection is a cross-row pipeline breaker and stops it; so
// does any operator that buffers or correlates across rows (Sort, Skip, Limit,
// Unwind, Union, Optional, Join, a nested Aggregate) or writes.
func parallelLeaf(p plan.Op) *plan.NodeScan {
	for {
		switch x := p.(type) {
		case *plan.NodeScan:
			return x
		case *plan.Filter:
			p = x.Input
		case *plan.Expand:
			p = x.Input
		case *plan.Intersect:
			p = x.Input
		case *plan.ShortestPath:
			p = x.Input
		case *plan.BindPath:
			p = x.Input
		case *plan.Project:
			if x.Distinct {
				return nil
			}
			p = x.Input
		default:
			return nil
		}
	}
}

// scanLeaf walks a compiled operator pipeline down its single input chain to the
// nodeScanOp at its root, mirroring parallelLeaf over the executor operators so a
// worker can point its private copy's scan at the shared morsel source. It returns
// nil if the chain does not bottom out in a nodeScanOp (which parallelLeaf already
// ruled out for an eligible plan, so this is a guard, not a path taken).
func scanLeaf(op operator) *nodeScanOp {
	for {
		switch x := op.(type) {
		case *nodeScanOp:
			return x
		case *filterOp:
			op = x.input
		case *expandOp:
			op = x.input
		case *varExpandOp:
			op = x.input
		case *intersectOp:
			op = x.input
		case *shortestPathOp:
			op = x.input
		case *bindPathOp:
			op = x.input
		case *projectOp:
			op = x.input
		default:
			return nil
		}
	}
}

// parallelSafeAgg reports whether an aggregate function's accumulator merges to a
// byte-identical result regardless of the order rows are folded in. count, min,
// and max do (integer addition and the min/max comparison are associative and
// commutative); sum and avg can re-associate float addition and collect depends
// on order, so they stay serial.
func parallelSafeAgg(name string) bool {
	switch strings.ToLower(name) {
	case "count", "min", "max":
		return true
	}
	return false
}

// parallelWorkers picks how many workers to run for a scan of n nodes: one per
// available core, but never more than there are full morsels to go around, and
// never parallel at all below a couple of morsels (the fan-out overhead would not
// pay for itself on a tiny scan). A return of 1 means run the serial path.
func parallelWorkers(n int) int {
	if n < 2*morselChunk {
		return 1
	}
	w := runtime.GOMAXPROCS(0)
	if max := n / morselChunk; w > max {
		w = max
	}
	if w < 1 {
		w = 1
	}
	return w
}

// primaryScan builds the node-id slice the morsel source hands out: the nodes
// carrying the scan's first known label (or all nodes when the scan is
// unlabeled), the same set nodeScanOp buffers on its serial path before the
// residual labels are checked per row. An unknown label anywhere in the set means
// the scan matches nothing, so it returns an empty slice.
func primaryScan(tx engine.Tx, ns *plan.NodeScan) ([]engine.NodeID, error) {
	scanTok := engine.Token(0)
	for i, l := range ns.Labels {
		if !l.Known {
			return nil, nil
		}
		if i == 0 {
			scanTok = l.Token
		}
	}
	var ids []engine.NodeID
	err := tx.ScanLabel(scanTok, func(id engine.NodeID) error {
		ids = append(ids, id)
		return nil
	})
	return ids, err
}

// windowResult is one morsel's group buckets, tagged with the morsel's window
// start so the merge can replay morsels in scan order. order is the keys in the
// order this morsel first saw them, which (within one disjoint window) matches the
// order the serial scan would have seen them.
type windowResult struct {
	lo     int
	groups map[string]*group
	order  []string
}

// windowWorker runs one worker: a fresh, private copy of the aggregate's input
// pipeline that processes one morsel at a time. For each morsel it takes from the
// shared cursor it points the scan at that window, re-opens the pipeline, and drains
// it into its own group buckets, tagging the result with the window start. The copy
// and its accumulators are unshared, and every read goes through the concurrency-safe
// read path, so workers run without coordination beyond the atomic morsel handoff.
func (o *aggregateOp) windowWorker(ids []engine.NodeID, src *morselSource) ([]windowResult, error) {
	op, err := compile(o.inputPlan)
	if err != nil {
		return nil, err
	}
	leaf := scanLeaf(op)
	if leaf == nil {
		return nil, errNoScanLeaf
	}
	leaf.windowed = true
	leaf.winIDs = ids
	var out []windowResult
	for {
		lo, hi, ok := src.take()
		if !ok {
			return out, nil
		}
		leaf.winLo, leaf.winHi = lo, hi
		if err := op.open(o.ctx); err != nil {
			return nil, err
		}
		groups, order, err := o.drainGroups(op)
		if err != nil {
			_ = op.close()
			return nil, err
		}
		if err := op.close(); err != nil {
			return nil, err
		}
		if len(order) > 0 {
			out = append(out, windowResult{lo: lo, groups: groups, order: order})
		}
	}
}

// runParallel scans the anchor nodes once, fans the morsels across workers, and
// merges their per-morsel group buckets into the output rows. It falls back to the
// serial path when the scan is too small to be worth splitting.
func (o *aggregateOp) runParallel(ns *plan.NodeScan) error {
	ids, err := primaryScan(o.ctx.Tx, ns)
	if err != nil {
		return err
	}
	w := parallelWorkers(len(ids))
	if w < 2 {
		// The serial fallback re-scans through the nodeScan operator, which counts its
		// own work, so the scan is counted there, not here.
		return o.runSerial()
	}
	// The morsel workers walk this already-scanned id slice rather than scanning again,
	// so the scan work is counted once here (doc 20 §3.1) instead of per worker.
	o.ctx.countScan(int64(len(ids)))
	src := &morselSource{ids: ids, chunk: morselChunk}
	partials := make([][]windowResult, w)
	errs := make([]error, w)
	var wg sync.WaitGroup
	for k := 0; k < w; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			partials[k], errs[k] = o.windowWorker(ids, src)
		}(k)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return o.mergeWindows(partials)
}

// mergeWindows folds every worker's per-morsel group buckets into one global set in
// the order the serial scan would have first seen each group, then emits. Morsels
// are disjoint id windows, so sorting them by window start and replaying each in its
// own first-seen order reproduces the serial first-seen group order exactly; the
// accumulators merge associatively, so the grouped answer is byte-identical to the
// serial one. A grouping-free aggregation with no surviving rows still yields its
// one empty group (count 0), the same rule the serial path applies.
func (o *aggregateOp) mergeWindows(partials [][]windowResult) error {
	var all []windowResult
	for _, p := range partials {
		all = append(all, p...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].lo < all[j].lo })
	global := map[string]*group{}
	var order []string
	for _, wr := range all {
		for _, key := range wr.order {
			gw := wr.groups[key]
			g := global[key]
			if g == nil {
				g = &group{rep: gw.rep, accs: o.newAccs()}
				global[key] = g
				order = append(order, key)
			}
			for j := range g.accs {
				g.accs[j].(mergeable).merge(gw.accs[j])
			}
		}
	}
	if len(order) == 0 && len(o.spec.GroupKeys) == 0 {
		g := &group{rep: eval.Row{}, accs: o.newAccs()}
		global[""] = g
		order = append(order, "")
	}
	return o.emit(global, order)
}
