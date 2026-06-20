package catalog

import (
	"fmt"
	"testing"

	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "cat.gr"

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInternRoundTrip interns names across all three dictionaries, commits,
// reopens, and checks that every token and name survived with stable ids.
func TestInternRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, err := store.CreateSections(p)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}

	labels := []string{"Person", "Movie", "Person", "Genre", "Movie"}
	wantLabelTokens := []uint32{0, 1, 0, 2, 1}
	for i, name := range labels {
		tok, _, err := cat.Intern(KindLabel, name)
		if err != nil {
			t.Fatal(err)
		}
		if tok != wantLabelTokens[i] {
			t.Fatalf("label %q token = %d, want %d", name, tok, wantLabelTokens[i])
		}
	}
	cat.Intern(KindRelType, "ACTED_IN")
	cat.Intern(KindRelType, "DIRECTED")
	cat.Intern(KindPropKey, "name")
	cat.Intern(KindPropKey, "year")
	cat.Intern(KindPropKey, "name") // dup

	if cat.Count(KindLabel) != 3 || cat.Count(KindRelType) != 2 || cat.Count(KindPropKey) != 2 {
		t.Fatalf("counts = %d/%d/%d", cat.Count(KindLabel), cat.Count(KindRelType), cat.Count(KindPropKey))
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and verify the dictionaries replayed identically.
	p2 := openPager(t, fsys)
	defer p2.Close()
	secs2, err := store.OpenSections(p2)
	if err != nil {
		t.Fatal(err)
	}
	cat2, err := Open(p2, secs2)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := cat2.Name(KindLabel, 1); !ok || n != "Movie" {
		t.Fatalf("label token 1 = %q,%v after reopen", n, ok)
	}
	if tok, ok := cat2.Lookup(KindPropKey, "year"); !ok || tok != 1 {
		t.Fatalf("propkey year = %d,%v after reopen", tok, ok)
	}
	// Interning a known name after reopen returns the original token, no growth.
	tok, added, err := cat2.Intern(KindLabel, "Genre")
	if err != nil || added || tok != 2 {
		t.Fatalf("re-intern Genre = %d added=%v err=%v", tok, added, err)
	}
}

// buildClean creates a database with an empty catalog committed and returns the
// VFS holding it, so the crash campaign can inject faults only over the interning
// workload that follows.
func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, err := store.CreateSections(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(p, secs); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	return fsys
}

// internName is the name interned at step j (1-based), so a recovered catalog's
// label count tells us exactly how many commits were durable.
func internName(j int) string { return fmt.Sprintf("L%04d", j) }

// runWorkload reopens on fsys and interns T labels, one per commit. An injected
// crash ends it early, which is expected during the campaign.
func runWorkload(fsys vfs.VFS, T int) (err error) {
	p, e := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = p.Close() }()
	secs, e := store.OpenSections(p)
	if e != nil {
		return e
	}
	cat, e := Open(p, secs)
	if e != nil {
		return e
	}
	for j := 1; j <= T; j++ {
		if _, _, e := cat.Intern(KindLabel, internName(j)); e != nil {
			return e
		}
		if e := p.Commit(); e != nil {
			return e
		}
	}
	return nil
}

// verifyDurablePrefix reopens a crashed snapshot and asserts the catalog reflects
// a committed prefix: its labels are exactly L0001..L000k for some k in [0,T],
// densely numbered and individually addressable. A torn or partial Log would
// show a gap, a wrong name, or fail to replay.
func verifyDurablePrefix(t *testing.T, crashed *vfs.Mem, T int, label string) int {
	t.Helper()
	p, err := pager.Open(crashed, path, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen after crash failed: %v", label, err)
	}
	defer p.Close()
	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("%s: reopen sections failed: %v", label, err)
	}
	cat, err := Open(p, secs)
	if err != nil {
		t.Fatalf("%s: catalog replay after crash failed: %v", label, err)
	}
	k := cat.Count(KindLabel)
	if k > T {
		t.Fatalf("%s: recovered %d labels exceeds committed max %d", label, k, T)
	}
	for j := 1; j <= k; j++ {
		name, ok := cat.Name(KindLabel, uint32(j-1))
		if !ok || name != internName(j) {
			t.Fatalf("%s: label token %d = %q,%v, want %q", label, j-1, name, ok, internName(j))
		}
		if tok, ok := cat.Lookup(KindLabel, internName(j)); !ok || tok != uint32(j-1) {
			t.Fatalf("%s: lookup %q = %d,%v, want %d", label, internName(j), tok, ok, j-1)
		}
	}
	return k
}

// crashCampaign counts the fault points of the interning workload, then trips at
// each ordinal and verifies the recovered catalog honors the durable-prefix
// property (doc 23, doc 25 §4 deliverable 12 — the catalog rides the substrate's
// durability).
func crashCampaign(t *testing.T, mode vfs.TripMode, label string) {
	const T = 6
	clean := buildClean(t)

	counter := vfs.NewCounter()
	cfs := clean.Snapshot()
	cfs.Attach(counter)
	if err := runWorkload(cfs, T); err != nil {
		t.Fatalf("%s: counting run errored: %v", label, err)
	}
	n := counter.Count()
	if n == 0 {
		t.Fatalf("%s: workload had no fault points", label)
	}

	for trip := range n {
		fs := clean.Snapshot()
		fs.Attach(vfs.NewTrip(trip, mode))
		_ = runWorkload(fs, T)
		crashed := fs.Snapshot()
		verifyDurablePrefix(t, crashed, T, label)
	}
}

func TestCatalogCrashCampaignCrash(t *testing.T) { crashCampaign(t, vfs.TripCrash, "crash") }
func TestCatalogCrashCampaignTorn(t *testing.T)  { crashCampaign(t, vfs.TripTear, "torn") }
func TestCatalogCrashCampaignFsync(t *testing.T) { crashCampaign(t, vfs.TripFsyncFail, "fsync") }

// TestCatalogDeterminism proves the same workload tripped at the same ordinal
// recovers the same number of durable labels every time.
func TestCatalogDeterminism(t *testing.T) {
	const T = 5
	clean := buildClean(t)
	counter := vfs.NewCounter()
	cfs := clean.Snapshot()
	cfs.Attach(counter)
	_ = runWorkload(cfs, T)
	n := counter.Count()

	for trip := range n {
		first := -1
		for rep := range 3 {
			fs := clean.Snapshot()
			fs.Attach(vfs.NewTrip(trip, vfs.TripCrash))
			_ = runWorkload(fs, T)
			k := verifyDurablePrefix(t, fs.Snapshot(), T, "determinism")
			if rep == 0 {
				first = k
			} else if k != first {
				t.Fatalf("non-deterministic recovery at trip %d: rep0=%d rep%d=%d", trip, first, rep, k)
			}
		}
	}
}
