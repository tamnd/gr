package exec

import (
	"runtime"
	"testing"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// personScanPlan is a bare build-side plan: scan every :Person node. It is the
// scan-rooted, row-independent shape the parallel build path accepts.
func personScanPlan() *plan.NodeScan {
	return &plan.NodeScan{Var: "b", Labels: []bind.NameRef{{Token: lblPerson, Known: true}}}
}

// drainBuildSerial builds the hash table the serial way, draining a single compiled
// copy of the plan into per-key buckets, so a test can compare it to the parallel
// build over the same plan and snapshot.
func drainBuildSerial(t *testing.T, ctx *Ctx, p plan.Op, on []string) map[string][]eval.Row {
	t.Helper()
	op, err := compile(p)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := op.open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer op.close()
	table := map[string][]eval.Row{}
	for {
		row, ok, err := op.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		k := rowKey(row, on)
		table[k] = append(table[k], row)
	}
	return table
}

// nodeIDSet renders a bucket's rows as a multiset of their "b" node ids. The build
// path preserves insertion order relative to a fixed id slice, but the MemEngine scans
// labels in Go-map order, so two independent scans differ in absolute order; the stable
// property to assert across them is the multiset of nodes in each bucket.
func nodeIDSet(rows []eval.Row) map[uint64]int {
	out := make(map[uint64]int, len(rows))
	for _, r := range rows {
		n, _ := r["b"].AsNode()
		out[n]++
	}
	return out
}

// TestParallelBuildTableMatchesSerial drives the build-side morsel parallelism: a
// build over a scan large enough to split into several morsels, with GOMAXPROCS forced
// above one so the parallel branch runs. The parallel table must hold the same bucket
// as the serial one, the same multiset of nodes, so a wrong morsel partition (a gap or
// an overlap) or a lost merge would show as a missing or doubled node. Every worker
// shares the one snapshot, so the race detector must stay clean.
func TestParallelBuildTableMatchesSerial(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	const n = 4096 // > 2*morselChunk, so several workers run
	e := bigPersonGraph(t, n)
	tx, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()
	ctx := &Ctx{Tx: tx}

	p := personScanPlan()
	want := drainBuildSerial(t, ctx, p, nil) // keyless: one bucket under ""

	got, ok, err := parallelBuildTable(ctx, p, nil)
	if err != nil {
		t.Fatalf("parallelBuildTable: %v", err)
	}
	if !ok {
		t.Fatal("expected the parallel build path to run for a large scan")
	}
	if len(want) != 1 || len(got) != 1 {
		t.Fatalf("buckets: serial %d, parallel %d, want 1 each", len(want), len(got))
	}
	ws, gs := nodeIDSet(want[""]), nodeIDSet(got[""])
	if len(ws) != n || len(gs) != n {
		t.Fatalf("distinct nodes: serial %d, parallel %d, want %d", len(ws), len(gs), n)
	}
	for id, c := range ws {
		if gs[id] != c {
			t.Fatalf("node %d count: serial %d, parallel %d", id, c, gs[id])
		}
	}
}

// TestParallelJoinMatchesSerial drives a full cartesian join whose build side is the
// large parallel-built table, against a handful of probe rows, once serial (GOMAXPROCS
// 1) and once parallel (GOMAXPROCS 4). The join output must be the same multiset both
// ways: build-side parallelism changes how the table is built, not the join's answer.
func TestParallelJoinMatchesSerial(t *testing.T) {
	const n = 4096
	e := bigPersonGraph(t, n)

	runJoinOverScan := func(procs int) map[string]int {
		old := runtime.GOMAXPROCS(procs)
		defer runtime.GOMAXPROCS(old)
		tx, err := e.Begin(false)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Abort()
		ctx := &Ctx{Tx: tx}
		left := &staticOp{rows: []eval.Row{
			{"a": value.Int(1)},
			{"a": value.Int(2)},
			{"a": value.Int(3)},
		}}
		p := personScanPlan()
		right, err := compile(p)
		if err != nil {
			t.Fatalf("compile right: %v", err)
		}
		o := &joinOp{on: nil, left: left, right: right, rightPlan: p}
		if err := o.open(ctx); err != nil {
			t.Fatalf("open: %v", err)
		}
		defer o.close()
		out := map[string]int{}
		for {
			row, ok, err := o.next()
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if !ok {
				break
			}
			out[rowCanon(row)]++
		}
		return out
	}

	serial := runJoinOverScan(1)
	parallel := runJoinOverScan(4)

	if len(serial) != 3*n || len(parallel) != 3*n {
		t.Fatalf("distinct rows: serial %d, parallel %d, want %d", len(serial), len(parallel), 3*n)
	}
	for k, c := range serial {
		if parallel[k] != c {
			t.Fatalf("row %q count: serial %d, parallel %d", k, c, parallel[k])
		}
	}
}
