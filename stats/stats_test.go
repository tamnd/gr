package stats_test

import (
	"testing"

	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/stats"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "stats.gr"

func opts() pager.Options { return pager.Options{Sync: wal.SyncFull, SaltSeed: 3} }

func labelCount(t *testing.T, st *stats.Stats, tok uint32) uint64 {
	t.Helper()
	c, err := st.LabelCount(tok)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func relCount(t *testing.T, st *stats.Stats, tok uint32) uint64 {
	t.Helper()
	c, err := st.RelTypeCount(tok)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestStatsCounts checks the count store: bumps accumulate, an untouched token
// reads zero, decrements clamp at zero, a sparse token grows the backing vector,
// and everything survives a commit and reopen.
func TestStatsCounts(t *testing.T) {
	fsys := vfs.NewMem()
	pg, err := pager.Open(fsys, path, opts())
	if err != nil {
		t.Fatal(err)
	}
	secs, err := store.CreateSections(pg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := stats.Create(pg, secs)
	if err != nil {
		t.Fatal(err)
	}

	mustAdd := func(f func() error) {
		t.Helper()
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(func() error { return st.AddLabel(0, 3) })
	mustAdd(func() error { return st.AddLabel(2, 1) })  // sparse: grows past token 1
	mustAdd(func() error { return st.AddLabel(50, 7) }) // sparser still
	mustAdd(func() error { return st.AddRelType(1, 5) })

	if c := labelCount(t, st, 0); c != 3 {
		t.Fatalf("label 0 = %d, want 3", c)
	}
	if c := labelCount(t, st, 2); c != 1 {
		t.Fatalf("label 2 = %d, want 1", c)
	}
	if c := labelCount(t, st, 50); c != 7 {
		t.Fatalf("label 50 = %d, want 7", c)
	}
	if c := labelCount(t, st, 1); c != 0 {
		t.Fatalf("untouched label 1 = %d, want 0", c)
	}
	if c := labelCount(t, st, 999); c != 0 {
		t.Fatalf("out-of-range label 999 = %d, want 0", c)
	}
	if c := relCount(t, st, 1); c != 5 {
		t.Fatalf("type 1 = %d, want 5", c)
	}

	// Decrements, including a clamp below zero.
	mustAdd(func() error { return st.AddLabel(0, -1) })
	if c := labelCount(t, st, 0); c != 2 {
		t.Fatalf("label 0 after -1 = %d, want 2", c)
	}
	mustAdd(func() error { return st.AddLabel(0, -10) })
	if c := labelCount(t, st, 0); c != 0 {
		t.Fatalf("label 0 after clamp = %d, want 0", c)
	}

	// Commit and reopen: the counts are durable.
	if err := pg.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	pg, err = pager.Open(fsys, path, opts())
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	secs, err = store.OpenSections(pg)
	if err != nil {
		t.Fatal(err)
	}
	st, err = stats.Open(pg, secs)
	if err != nil {
		t.Fatal(err)
	}
	if c := labelCount(t, st, 2); c != 1 {
		t.Fatalf("reopened label 2 = %d, want 1", c)
	}
	if c := labelCount(t, st, 50); c != 7 {
		t.Fatalf("reopened label 50 = %d, want 7", c)
	}
	if c := relCount(t, st, 1); c != 5 {
		t.Fatalf("reopened type 1 = %d, want 5", c)
	}
}
