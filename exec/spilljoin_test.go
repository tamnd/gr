package exec

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// staticOp is a test operator that yields a fixed slice of rows once, the leaf the
// spill-join test drives the join with (no graph or planner needed to exercise the
// join operator on its own).
type staticOp struct {
	rows []eval.Row
	pos  int
}

func (o *staticOp) open(ctx *Ctx) error { o.pos = 0; return nil }

func (o *staticOp) next() (eval.Row, bool, error) {
	if o.pos >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.pos]
	o.pos++
	return r, true, nil
}

func (o *staticOp) close() error { return nil }

// memTempFiles returns a TempFile factory backed by a fresh in-memory VFS, the
// spill area the test hands the join.
func memTempFiles() func() (vfs.File, func() error, error) {
	fs := vfs.NewMem()
	ctr := 0
	return func() (vfs.File, func() error, error) {
		ctr++
		name := fmt.Sprintf("spill-%d", ctr)
		f, err := fs.Open(name, true)
		if err != nil {
			return nil, nil, err
		}
		discard := func() error {
			_ = f.Close()
			return fs.Remove(name)
		}
		return f, discard, nil
	}
}

// runJoin drains a fresh join over the given rows under the given context and
// returns the output rows as a multiset, keyed by a canonical string.
func runJoin(t *testing.T, ctx *Ctx, left, right []eval.Row) map[string]int {
	t.Helper()
	o := &joinOp{
		on:    []string{"k"},
		left:  &staticOp{rows: left},
		right: &staticOp{rows: right},
	}
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

// rowCanon renders a row as a stable string, keys sorted, for multiset equality.
func rowCanon(row eval.Row) string {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s;", k, row[k].String())
	}
	return b.String()
}

// TestSpillJoinMatchesInMemory drives the same hash join twice over the same
// inputs, once entirely in memory (no budget) and once with a tiny budget and a
// temp area that forces spilling and several rounds of re-partitioning, and checks
// the two produce the same multiset of output rows. The grace path must change how
// the join spends memory, not its answer.
func TestSpillJoinMatchesInMemory(t *testing.T) {
	const (
		nRight  = 4000
		nLeft   = 2000
		nKeys   = 40
		nDup    = 3
		payload = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" // pad rows so a small budget overflows
	)
	var right []eval.Row
	for i := 0; i < nRight; i++ {
		right = append(right, eval.Row{
			"k": value.Int(int64(i % nKeys)),
			"r": value.Int(int64(i)),
			"p": value.String(payload),
		})
	}
	var left []eval.Row
	for i := 0; i < nLeft; i++ {
		// A few left rows per key so each probe matches many build rows.
		left = append(left, eval.Row{
			"k": value.Int(int64(i % (nKeys * nDup) % nKeys)),
			"l": value.Int(int64(i)),
		})
	}

	base := func() *Ctx { return &Ctx{} }

	want := runJoin(t, base(), left, right)

	spillCtx := base()
	spillCtx.MemBudget = 2048
	spillCtx.TempFile = memTempFiles()
	got := runJoin(t, spillCtx, left, right)

	if len(want) != len(got) {
		t.Fatalf("distinct output rows: in-memory %d, spilled %d", len(want), len(got))
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("row %q count: in-memory %d, spilled %d", k, n, got[k])
		}
	}

	total := 0
	for _, n := range want {
		total += n
	}
	if total == 0 {
		t.Fatal("expected the join to produce rows")
	}
}

// runCartesian drains a fresh keyless join (a cartesian product) under the given
// context and returns its output rows as a multiset.
func runCartesian(t *testing.T, ctx *Ctx, left, right []eval.Row) map[string]int {
	t.Helper()
	o := &joinOp{
		on:    nil,
		left:  &staticOp{rows: left},
		right: &staticOp{rows: right},
	}
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

// TestSpillCartesianMatchesInMemory checks a keyless join (a cartesian product),
// which has no key to partition on, spills its build side to one file and streams it
// per probe row (a block-nested-loop over disk) without changing its output. The
// spilled run uses a tiny budget that forces the build side out of memory.
func TestSpillCartesianMatchesInMemory(t *testing.T) {
	const (
		nLeft  = 200
		nRight = 300
	)
	var left []eval.Row
	for i := 0; i < nLeft; i++ {
		left = append(left, eval.Row{"l": value.Int(int64(i))})
	}
	var right []eval.Row
	for i := 0; i < nRight; i++ {
		right = append(right, eval.Row{
			"r": value.Int(int64(i)),
			"p": value.String("padding-padding-padding-padding"),
		})
	}

	want := runCartesian(t, &Ctx{}, left, right)

	spillCtx := &Ctx{MemBudget: 512, TempFile: memTempFiles()}
	got := runCartesian(t, spillCtx, left, right)

	if len(want) != len(got) || len(want) != nLeft*nRight {
		t.Fatalf("distinct rows: in-memory %d, spilled %d, want %d", len(want), len(got), nLeft*nRight)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("row %q count: in-memory %d, spilled %d", k, n, got[k])
		}
	}
}
