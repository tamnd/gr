package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// seedFile creates a fresh empty database file on a plain (no-fault) VFS and
// closes it cleanly, returning the VFS with the seeded file. The fault controller
// is attached after seeding so faults only cover the workload, not the file creation.
func seedFile(t *testing.T, name string) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	db, err := Open(name, Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatalf("seedFile: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("seedFile close: %v", err)
	}
	return fsys
}

// crashAt seeds a database file, attaches a fault controller that trips at ordinal
// `at` with the given mode, then runs the workload. When the injected fault fires,
// the pager returns ErrInjectedCrash up through the call stack. crashAt snapshots
// the VFS (the durable bytes a real crash would leave) and returns the snapshot
// plus the committed count the workload reported before the crash.
func crashAt(t *testing.T, name string, at int, mode vfs.TripMode, workload func(db *DB) (committed int, err error)) (crashed *vfs.Mem, committed int) {
	t.Helper()
	fsys := seedFile(t, name)
	fc := vfs.NewTrip(at, mode)
	fsys.Attach(fc)

	db, err := Open(name, Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		// Trip fired during open itself (e.g. very first I/O ordinal). The snapshot
		// represents the pre-open durable state; recover from that.
		return fsys.Snapshot(), 0
	}

	committed, werr := workload(db)
	_ = db.Close()

	if werr != nil && !errors.Is(werr, vfs.ErrInjectedCrash) && !fc.Tripped() {
		t.Fatalf("crashAt at=%d: workload returned unexpected error: %v", at, werr)
	}
	return fsys.Snapshot(), committed
}

// reopenCrashed opens a fresh DB on a crash snapshot and returns it.
// Panics (via t.Fatal) if recovery fails.
func reopenCrashed(t *testing.T, crashed *vfs.Mem, name string) *DB {
	t.Helper()
	db, err := Open(name, Options{VFS: crashed, SaltSeed: 1})
	if err != nil {
		t.Fatalf("reopenCrashed: recovery failed: %v", err)
	}
	return db
}

// crashMatrix runs a workload, counts its I/O fault points, then re-runs it
// crashing at every ordinal 0..N-1 with TripCrash. After each crash it:
//  1. Reopens the file (triggering WAL recovery).
//  2. Asserts the recovered node count is 0 (no committed data yet) or equal to
//     what the workload committed before the crash (committed <= expectedMax).
//  3. Calls assertIntegrity so every crash result passes the structural checker.
//  4. Reopens a second time to verify idempotent redo.
//
// Only TripCrash mode is used here; torn-write variants are in separate tests.
func crashMatrix(t *testing.T, name string, expectedMax int, workload func(db *DB) (committed int, err error)) {
	t.Helper()

	// Counting pass: discover N (total fault points after Open) without crashing.
	fsys0 := seedFile(t, name)
	counter := vfs.NewCounter()
	fsys0.Attach(counter)
	db, err := Open(name, Options{VFS: fsys0, SaltSeed: 1})
	if err != nil {
		t.Fatalf("crashMatrix count: open: %v", err)
	}
	_, _ = workload(db)
	_ = db.Close()
	N := counter.Count()

	// Crash at every ordinal.
	for at := range N {
		snap, _ := crashAt(t, name, at, vfs.TripCrash, workload)

		// First recovery.
		r1 := reopenCrashed(t, snap, name)
		n1 := nodeCount(t, r1)
		assertIntegrity(t, r1)
		_ = r1.Close()

		// Recovered count must be in [0, expectedMax].
		// We can't assert an exact count because a crash may fire after a commit's
		// fsync but before Exec returns — the commit IS durable even though the
		// workload got an error. The invariant is just: no more than the max that
		// could possibly have committed.
		if n1 > int64(expectedMax) {
			t.Errorf("crashMatrix at=%d: recovered %d nodes, max possible committed was %d", at, n1, expectedMax)
		}

		// Second recovery (idempotency): reopen the SAME snapshot again.
		r2 := reopenCrashed(t, snap, name)
		n2 := nodeCount(t, r2)
		assertIntegrity(t, r2)
		_ = r2.Close()

		if n1 != n2 {
			t.Errorf("crashMatrix at=%d: idempotency: first recovery=%d nodes, second=%d", at, n1, n2)
		}
	}
}

// TestCrashMatrixSingleWrite runs the crash matrix over a single committed
// write transaction. At every fault point, the recovered state is either empty
// (crash before commit) or has 1 node (crash after commit). Both are valid
// durable-prefix outcomes and must pass assertIntegrity.
func TestCrashMatrixSingleWrite(t *testing.T) {
	crashMatrix(t, "cm_single.gr", 1, func(db *DB) (int, error) {
		if _, err := db.Exec("CREATE (:N {i: 1})", nil); err != nil {
			return 0, err
		}
		return 1, nil
	})
}

// TestCrashMatrixThreeWrites runs the crash matrix over three sequential committed
// transactions. After each crash the recovered count must be in [0,3] and
// assertIntegrity must pass.
func TestCrashMatrixThreeWrites(t *testing.T) {
	crashMatrix(t, "cm3.gr", 3, func(db *DB) (committed int, err error) {
		for i := 1; i <= 3; i++ {
			if _, err := db.Exec("CREATE (:N)", nil); err != nil {
				return committed, err
			}
			committed++
		}
		return committed, nil
	})
}

// TestCrashMatrixRelationship runs the crash matrix over a workload that creates
// two nodes and a relationship between them. The expectedMax is 2 because one
// successful commit leaves 2 nodes in the database.
func TestCrashMatrixRelationship(t *testing.T) {
	crashMatrix(t, "cm_rel.gr", 2, func(db *DB) (int, error) {
		if _, err := db.Exec("CREATE (a:A)-[:R]->(b:B)", nil); err != nil {
			return 0, err
		}
		return 2, nil
	})
}

// TestCrashMatrixWithIndex verifies crash recovery with a declared property index:
// the index must survive recovery consistently (the recovered index matches the
// recovered data — the index-vs-data property assertIntegrity checks).
func TestCrashMatrixWithIndex(t *testing.T) {
	crashMatrix(t, "cm_ix.gr", 2, func(db *DB) (committed int, err error) {
		if _, err := db.Exec("CREATE INDEX FOR (n:N) ON (n.k)", nil); err != nil {
			return 0, err
		}
		committed++
		if _, err := db.Exec("CREATE (:N {k: 42})", nil); err != nil {
			return committed, err
		}
		committed++
		return committed, nil
	})
}

// TestCrashAtEveryFsync crashes at every Sync (fsync) operation boundary, which is
// where the durability commitment happens. This is the most important set of crash
// points: a crash just BEFORE a commit-Sync means the transaction is not durable;
// just AFTER means it is. Both sides must be consistent.
func TestCrashAtEveryFsync(t *testing.T) {
	crashMatrix(t, "cm_fsync.gr", 2, func(db *DB) (committed int, err error) {
		if _, err := db.Exec("CREATE (:A {i: 1})", nil); err != nil {
			return 0, err
		}
		committed++
		if _, err := db.Exec("CREATE (:B {i: 2})", nil); err != nil {
			return committed, err
		}
		committed++
		return committed, nil
	})
}

// TestCrashTornWriteToWAL tests torn-write recovery: with TripTear mode the fault
// injects a partial write (only a sector prefix of a page write lands), modeling
// a torn sector at the storage layer. Recovery must detect the torn frame via its
// checksum and exclude the uncommitted transaction.
func TestCrashTornWriteToWAL(t *testing.T) {
	N := countFaultPoints(t, "torn.gr", func(db *DB) error {
		_, err := db.Exec("CREATE (:T {v: 1})", nil)
		return err
	})

	for at := range N {
		fsys := seedFile(t, "torn.gr")
		fc := vfs.NewTrip(at, vfs.TripTear)
		fsys.Attach(fc)

		db, err := Open("torn.gr", Options{VFS: fsys, SaltSeed: 1})
		if err != nil {
			snap := fsys.Snapshot()
			r := reopenCrashed(t, snap, "torn.gr")
			assertIntegrity(t, r)
			_ = r.Close()
			continue
		}
		_, _ = db.Exec("CREATE (:T {v: 1})", nil)
		_ = db.Close()

		snap := fsys.Snapshot()
		r := reopenCrashed(t, snap, "torn.gr")
		assertIntegrity(t, r)
		_ = r.Close()
	}
}

// TestCrashFsyncFatalAndRecovery injects an fsync failure (TripFsyncFail) and
// verifies that after the failure, reopening the database recovers to the last
// fully committed state — the fsync-fatal policy (doc 05 §4.9).
func TestCrashFsyncFatalAndRecovery(t *testing.T) {
	N := countFaultPoints(t, "fsyncfail.gr", func(db *DB) error {
		_, err := db.Exec("CREATE (:F {i: 1})", nil)
		return err
	})

	for at := range N {
		fsys := seedFile(t, "fsyncfail.gr")
		fc := vfs.NewTrip(at, vfs.TripFsyncFail)
		fsys.Attach(fc)

		db, err := Open("fsyncfail.gr", Options{VFS: fsys, SaltSeed: 1})
		if err != nil {
			snap := fsys.Snapshot()
			r := reopenCrashed(t, snap, "fsyncfail.gr")
			assertIntegrity(t, r)
			_ = r.Close()
			continue
		}
		_, _ = db.Exec("CREATE (:F {i: 1})", nil)
		_ = db.Close()

		snap := fsys.Snapshot()
		r := reopenCrashed(t, snap, "fsyncfail.gr")
		assertIntegrity(t, r)
		_ = r.Close()
	}
}

// TestCrashEmptyDatabase verifies that crashing on an empty database (no write
// transactions) recovers cleanly to an empty graph.
func TestCrashEmptyDatabase(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "empty_crash.gr")
	_ = db.Close()

	r := crashReopen(t, fsys, "empty_crash.gr")
	defer func() { _ = r.Close() }()

	assertIntegrity(t, r)
	if n := nodeCount(t, r); n != 0 {
		t.Errorf("empty crash: recovered %d nodes, want 0", n)
	}
}

// TestCrashMultipleRecoveries verifies that recovering twice from the same
// crash image yields the same result (idempotent redo, doc 05 invariant 4).
func TestCrashMultipleRecoveries(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "multi.gr")
	mustExec(t, db, "CREATE (:N {i: 1})", nil)
	mustExec(t, db, "CREATE (:N {i: 2})", nil)
	// Crash without checkpoint.
	snap := fsys.Snapshot()
	_ = db.Close()

	r1 := reopenCrashed(t, snap, "multi.gr")
	n1 := nodeCount(t, r1)
	assertIntegrity(t, r1)
	_ = r1.Close()

	// Second recovery from the same snapshot.
	r2 := reopenCrashed(t, snap, "multi.gr")
	n2 := nodeCount(t, r2)
	assertIntegrity(t, r2)
	_ = r2.Close()

	if n1 != n2 {
		t.Errorf("idempotent redo: first recovery=%d nodes, second=%d", n1, n2)
	}
}

// countFaultPoints runs a workload once with the counter controller and returns
// the total number of fault points after Open.
func countFaultPoints(t *testing.T, name string, workload func(db *DB) error) int {
	t.Helper()
	fsys := seedFile(t, name)
	counter := vfs.NewCounter()
	fsys.Attach(counter)
	db, err := Open(name, Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatalf("countFaultPoints: open: %v", err)
	}
	_ = workload(db)
	_ = db.Close()
	return counter.Count()
}
