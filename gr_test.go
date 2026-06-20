package gr

import (
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const testPath = "campaign.gr"

// encodePage writes commit number j into a data page: a u64 prefix plus a
// byte(j) fill, so a torn page (a mix of two commits' bytes) is detectable and a
// recovered page can be checked for internal consistency.
func encodePage(f *pager.Frame, j uint64) {
	format.WriteHeader(f.Data, format.PageHeader{Type: format.PageTypeData})
	off := format.PayloadOffset()
	format.PutU64(f.Data[off:], j)
	for i := off + 8; i < len(f.Data)-format.ChecksumSize; i++ {
		f.Data[i] = byte(j)
	}
}

// decodePage reads back the committed value and verifies internal consistency
// (every fill byte equals the low byte of the u64 prefix). It returns the value
// and whether the page is torn.
func decodePage(f *pager.Frame) (uint64, bool) {
	off := format.PayloadOffset()
	v := format.U64(f.Data[off:])
	for i := off + 8; i < len(f.Data)-format.ChecksumSize; i++ {
		if f.Data[i] != byte(v) {
			return v, true // torn: fill disagrees with the prefix
		}
	}
	return v, false
}

// buildClean creates a database with one data page committed at value 0 and
// returns the VFS holding it.
func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	db, err := Open(testPath, Options{VFS: fsys, Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	f, err := db.pager.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID() != 1 {
		t.Fatalf("expected the data page to be page 1, got %d", f.ID())
	}
	encodePage(f, 0)
	db.pager.MarkDirty(f)
	db.pager.Unpin(f)
	if err := db.pager.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return fsys
}

// runWorkload reopens the database on fsys and performs T commits, each
// rewriting page 1 with the next value. It returns the first error (an injected
// crash ends the workload early, which is expected during the campaign).
func runWorkload(fsys vfs.VFS, T int) (err error) {
	db, e := Open(testPath, Options{VFS: fsys, Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = db.Close() }()
	for j := 1; j <= T; j++ {
		f, e := db.pager.ReadPage(1)
		if e != nil {
			return e
		}
		encodePage(f, uint64(j))
		db.pager.MarkDirty(f)
		db.pager.Unpin(f)
		if e := db.pager.Commit(); e != nil {
			return e
		}
	}
	return nil
}

// verifyDurablePrefix reopens a crashed snapshot (no faults) and asserts the
// durable-prefix property: recovery succeeds, page 1 is not torn, its value v is
// a committed prefix in [0,T], and the header's change counter advanced in lock
// step with the data page (c == v+1), proving the header and the data page
// committed atomically (doc 05 §10 invariants 5, 6, 7).
func verifyDurablePrefix(t *testing.T, crashed *vfs.Mem, T int, label string) uint64 {
	t.Helper()
	db, err := Open(testPath, Options{VFS: crashed})
	if err != nil {
		t.Fatalf("%s: reopen after crash failed: %v", label, err)
	}
	defer db.Close()
	f, err := db.pager.ReadPage(1)
	if err != nil {
		t.Fatalf("%s: ReadPage(1) after crash failed (torn page survived?): %v", label, err)
	}
	v, torn := decodePage(f)
	db.pager.Unpin(f)
	if torn {
		t.Fatalf("%s: recovered page 1 is torn (value %d)", label, v)
	}
	if v > uint64(T) {
		t.Fatalf("%s: recovered value %d exceeds committed max %d", label, v, T)
	}
	if c := db.pager.Header().ChangeCounter; c != v+1 {
		t.Fatalf("%s: header/data disagree: ChangeCounter=%d, page value=%d (want c==v+1)", label, c, v)
	}
	return v
}

// crashCampaign runs the full crash campaign for one trip mode: count the fault
// points of the workload, then trip at each ordinal and verify the recovered
// state honors the durable-prefix property.
func crashCampaign(t *testing.T, mode vfs.TripMode, label string) {
	const T = 6
	clean := buildClean(t)

	// Count phase: run the workload once, tripping nothing, to count fault points.
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

	for trip := 0; trip < n; trip++ {
		fc := vfs.NewTrip(trip, mode)
		fs := clean.Snapshot()
		fs.Attach(fc)
		// The workload should crash at this ordinal (or commit fewer than T).
		_ = runWorkload(fs, T)
		crashed := fs.Snapshot() // copy media at the crash point, drop faults
		verifyDurablePrefix(t, crashed, T, label)
	}
}

func TestCrashCampaignCrash(t *testing.T) {
	crashCampaign(t, vfs.TripCrash, "crash")
}

func TestCrashCampaignTorn(t *testing.T) {
	crashCampaign(t, vfs.TripTear, "torn")
}

func TestCrashCampaignFsyncFail(t *testing.T) {
	crashCampaign(t, vfs.TripFsyncFail, "fsync-fail")
}

// TestFsyncFatal checks that a failed fsync surfaces as a commit error (the
// database stops rather than pretending the commit was durable) and that the
// next open recovers to a valid committed prefix (doc 05 §10 invariant 8).
func TestFsyncFatal(t *testing.T) {
	clean := buildClean(t)
	// Find a sync fault point and trip it as an fsync failure.
	counter := vfs.NewCounter()
	cfs := clean.Snapshot()
	cfs.Attach(counter)
	_ = runWorkload(cfs, 3)
	n := counter.Count()

	sawError := false
	for trip := 0; trip < n; trip++ {
		fc := vfs.NewTrip(trip, vfs.TripFsyncFail)
		fs := clean.Snapshot()
		fs.Attach(fc)
		if err := runWorkload(fs, 3); err != nil {
			sawError = true
		}
		crashed := fs.Snapshot()
		verifyDurablePrefix(t, crashed, 3, "fsync-fatal")
	}
	if !sawError {
		t.Fatal("expected at least one fsync failure to surface as an error")
	}
}

// TestDeterminismReplay proves the determinism hooks: the same workload tripped
// at the same ordinal produces the same recovered state every time.
func TestDeterminismReplay(t *testing.T) {
	clean := buildClean(t)
	const T = 5
	counter := vfs.NewCounter()
	cfs := clean.Snapshot()
	cfs.Attach(counter)
	_ = runWorkload(cfs, T)
	n := counter.Count()

	for trip := 0; trip < n; trip++ {
		var first uint64
		for rep := 0; rep < 3; rep++ {
			fs := clean.Snapshot()
			fs.Attach(vfs.NewTrip(trip, vfs.TripCrash))
			_ = runWorkload(fs, T)
			crashed := fs.Snapshot()
			v := verifyDurablePrefix(t, crashed, T, "determinism")
			if rep == 0 {
				first = v
			} else if v != first {
				t.Fatalf("non-deterministic recovery at trip %d: rep0=%d rep%d=%d", trip, first, rep, v)
			}
		}
	}
}

// TestLifecycle is the M0 open/close demo: Open on a fresh path creates a valid
// file, Open again reads it, Close is clean and idempotent.
func TestLifecycle(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("life.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	if db.PageSize() != format.DefaultPageSize {
		t.Fatalf("page size = %d, want %d", db.PageSize(), format.DefaultPageSize)
	}
	if db.Path() != "life.gr" {
		t.Fatalf("path = %q", db.Path())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("double close should be a no-op, got %v", err)
	}

	db2, err := Open("life.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCustomPageSize exercises non-default page sizes through the lifecycle.
func TestCustomPageSize(t *testing.T) {
	for _, ps := range []uint32{512, 1024, 8192, 16384} {
		fsys := vfs.NewMem()
		db, err := Open("p.gr", Options{VFS: fsys, PageSize: ps})
		if err != nil {
			t.Fatalf("ps=%d: %v", ps, err)
		}
		if db.PageSize() != ps {
			t.Fatalf("ps=%d: got %d", ps, db.PageSize())
		}
		f, err := db.pager.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		db.pager.MarkDirty(f)
		db.pager.Unpin(f)
		if err := db.pager.Commit(); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}
}
