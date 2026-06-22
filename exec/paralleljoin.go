package exec

import (
	"sort"
	"sync"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
)

// Build-side morsel parallelism for the hash join (spec 2060 doc 12 §5, §10, doc 25
// §7.2 deliverable 8: "the join operators ... morsel-driven parallelism across
// cores"). Only the construction of the build-side hash table fans across cores; the
// probe stays serial and pull-based, so the join's output rows and their order are
// exactly the serial path's. The two children of a Join share no variables (the
// planner emits a Join only for a disconnected pattern), so the build (right) side is
// an independent subplan a worker can run a private copy of over a morsel of the
// scanned anchor ids, the same shape and machinery the parallel aggregation uses.
//
// It applies when the build side is a scan-rooted, row-independent pipeline (the
// parallelLeaf shape) and no memory budget is set. A budgeted join keeps the serial
// build so it can spill (spilljoin.go); parallel build and spill do not compose, and
// the in-memory build is exactly the case a budget of zero already chose.

// buildWindow is one morsel's contribution to the hash table: its per-key row
// buckets, tagged with the morsel's window start. Merging the windows in ascending
// window order, each window's rows already in scan order, reproduces the serial
// insertion order per key exactly, so even an order-sensitive consumer above the
// join sees the serial result.
type buildWindow struct {
	lo    int
	table map[string][]eval.Row
}

// parallelBuildTable builds a hash join's build-side table across cores from a fresh
// copy of the build subplan per worker. It returns ok false (and a nil table) when
// the build side is not the parallelizable shape or is too small to be worth
// splitting, so the caller falls back to the serial build. The error is non-nil only
// on a real failure (a scan or a worker error).
func parallelBuildTable(ctx *Ctx, buildPlan plan.Op, on []string) (map[string][]eval.Row, bool, error) {
	ns := parallelLeaf(buildPlan)
	if ns == nil {
		return nil, false, nil
	}
	ids, err := primaryScan(ctx.Tx, ns)
	if err != nil {
		return nil, false, err
	}
	w := parallelWorkers(len(ids))
	if w < 2 {
		return nil, false, nil
	}
	src := &morselSource{ids: ids, chunk: morselChunk}
	partials := make([][]buildWindow, w)
	errs := make([]error, w)
	var wg sync.WaitGroup
	for k := 0; k < w; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			partials[k], errs[k] = buildWorker(ctx, buildPlan, on, ids, src)
		}(k)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, false, e
		}
	}
	var all []buildWindow
	for _, p := range partials {
		all = append(all, p...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].lo < all[j].lo })
	table := map[string][]eval.Row{}
	for _, bw := range all {
		for key, rows := range bw.table {
			table[key] = append(table[key], rows...)
		}
	}
	return table, true, nil
}

// buildWorker runs one worker: a fresh, private copy of the build subplan that
// processes one morsel at a time. For each morsel it takes from the shared cursor it
// points the scan at that window, re-opens the pipeline, and folds the rows into its
// own per-key buckets tagged with the window start. The copy and its buckets are
// unshared and every read goes through the concurrency-safe read path, so workers run
// without coordination beyond the atomic morsel handoff.
func buildWorker(ctx *Ctx, buildPlan plan.Op, on []string, ids []engine.NodeID, src *morselSource) ([]buildWindow, error) {
	op, err := compile(buildPlan)
	if err != nil {
		return nil, err
	}
	leaf := scanLeaf(op)
	if leaf == nil {
		return nil, errNoScanLeaf
	}
	leaf.windowed = true
	leaf.winIDs = ids
	var out []buildWindow
	for {
		lo, hi, ok := src.take()
		if !ok {
			return out, nil
		}
		leaf.winLo, leaf.winHi = lo, hi
		if err := op.open(ctx); err != nil {
			return nil, err
		}
		win := map[string][]eval.Row{}
		for {
			row, ok, err := op.next()
			if err != nil {
				_ = op.close()
				return nil, err
			}
			if !ok {
				break
			}
			k := rowKey(row, on)
			win[k] = append(win[k], row)
		}
		if err := op.close(); err != nil {
			return nil, err
		}
		if len(win) > 0 {
			out = append(out, buildWindow{lo: lo, table: win})
		}
	}
}
