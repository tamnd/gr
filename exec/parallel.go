package exec

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/plan"
)

// Morsel-driven parallelism for a grouping-free aggregation over a scan-rooted
// read pipeline (spec 2060 doc 12 §10, doc 25 §7.2 deliverable 8, the §7.4 exit
// criterion "morsel parallelism scales"). The shape is the simplest one that is
// genuinely parallel: a read-only RETURN count(*) / min / max over MATCH (n:L)
// ... with no GROUP BY. The whole input pipeline is independent per anchor row,
// so the scanned node set is cut into fixed-size morsels handed out by an atomic
// cursor, each worker runs a private copy of the pipeline over the morsels it
// pulls into its own accumulators, and the partials are merged at the end.
//
// This is transparent: the plan is unchanged (still Aggregate over its input),
// and the executor decides at run time whether to parallelize. The aggregates it
// parallelizes (count, min, max) are exactly associative and commutative, so the
// merged answer is byte-identical to the serial one, which keeps the M2/M3 result
// oracle green (M4 changes performance, not answers). sum and avg (float
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

// drainWorker runs one worker: a fresh, private copy of the aggregate's input
// pipeline whose scan is pointed at the shared morsel source, folded into its own
// accumulators. The copy and its accumulators are unshared, and every read it
// makes goes through the concurrency-safe read path, so workers run without
// coordination beyond the atomic morsel handoff.
func (o *aggregateOp) drainWorker(src *morselSource) ([]accumulator, error) {
	op, err := compile(o.inputPlan)
	if err != nil {
		return nil, err
	}
	leaf := scanLeaf(op)
	if leaf == nil {
		return nil, errNoScanLeaf
	}
	leaf.src = src
	if err := op.open(o.ctx); err != nil {
		return nil, err
	}
	defer op.close()
	accs := o.newAccs()
	for {
		in, ok, err := op.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		env := o.ctx.env(in)
		for j, call := range o.calls {
			if err := o.feed(accs[j], call, env); err != nil {
				return nil, err
			}
		}
	}
	return accs, nil
}

// runParallel scans the anchor nodes once, fans the morsels across workers, and
// merges their partial accumulators into the single grouping-free output row. It
// falls back to the serial path when the scan is too small to be worth splitting.
func (o *aggregateOp) runParallel(ns *plan.NodeScan) error {
	ids, err := primaryScan(o.ctx.Tx, ns)
	if err != nil {
		return err
	}
	w := parallelWorkers(len(ids))
	if w < 2 {
		return o.runSerial()
	}
	src := &morselSource{ids: ids, chunk: morselChunk}
	partials := make([][]accumulator, w)
	errs := make([]error, w)
	var wg sync.WaitGroup
	for k := 0; k < w; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			partials[k], errs[k] = o.drainWorker(src)
		}(k)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	merged := o.newAccs()
	for _, accs := range partials {
		for j := range merged {
			merged[j].(mergeable).merge(accs[j])
		}
	}
	g := &group{rep: nil, accs: merged}
	return o.emit(map[string]*group{"": g}, []string{""})
}
