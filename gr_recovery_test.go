package gr

import (
	"testing"

	"github.com/tamnd/gr/vfs"
)

// openOn opens a database over a caller-held VFS so a test can snapshot the media
// afterward. It mirrors openMem but keeps the VFS in the test's hands, which is what
// a crash-recovery test needs: it writes through the database, then snapshots the
// same VFS at the crash point.
func openOn(t *testing.T, fsys vfs.VFS, name string) *DB {
	t.Helper()
	db, err := Open(name, Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// crashReopen snapshots the media at the current point (no clean Close, so the WAL
// is whatever the committed writes left) and reopens a fresh database on the copy.
// This is the crash model the durability campaign uses (gr_test.go): the snapshot is
// exactly the post-crash media, and Open must recover the committed writes from the
// WAL since no checkpoint ran.
func crashReopen(t *testing.T, fsys *vfs.Mem, name string) *DB {
	t.Helper()
	crashed := fsys.Snapshot()
	db, err := Open(name, Options{VFS: crashed, SaltSeed: 1})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	return db
}

// TestExecWriteRecoversAfterCrash confirms a sequence of committed Cypher writes
// survives a crash with no clean close: each Exec commits its own engine
// transaction to the WAL, and reopening replays the WAL to the last committed state
// (doc 05 §10, doc 13 §16).
func TestExecWriteRecoversAfterCrash(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "recover.gr")

	mustExec(t, db, "CREATE (:Person {name: 'A'})", nil)
	mustExec(t, db, "CREATE (:Person {name: 'B'})", nil)
	mustExec(t, db, "CREATE (:Person {name: 'C'})", nil)
	// A later clause mutates an earlier node: the recovered state must reflect the
	// mutation, not just the creates.
	mustExec(t, db, "MATCH (p:Person {name: 'B'}) SET p.name = 'B2'", nil)
	mustExec(t, db, "MATCH (p:Person {name: 'C'}) DELETE p", nil)

	// Crash: snapshot the media without closing the database, then reopen.
	db2 := crashReopen(t, fsys, "recover.gr")
	defer func() { _ = db2.Close() }()

	if n := nodeCount(t, db2); n != 2 {
		t.Fatalf("recovered node count = %d, want 2 (C was deleted)", n)
	}
	rows := collectRows(t, db2, "MATCH (p:Person) RETURN p.name AS name ORDER BY name", nil)
	if len(rows) != 2 {
		t.Fatalf("recovered %d persons, want 2", len(rows))
	}
	a, _ := rows[0]["name"].AsString()
	b, _ := rows[1]["name"].AsString()
	if a != "A" || b != "B2" {
		t.Fatalf("recovered names = %q,%q, want A,B2 (the SET must have survived)", a, b)
	}
}

// TestExecWriteRecoversCommittedPrefix confirms the recovered state is exactly the
// committed prefix: a snapshot taken after the third of several commits recovers
// three nodes, and a later snapshot recovers the later commits too.
func TestExecWriteRecoversCommittedPrefix(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "prefix.gr")

	mustExec(t, db, "CREATE (:N {i: 1})", nil)
	mustExec(t, db, "CREATE (:N {i: 2})", nil)
	mustExec(t, db, "CREATE (:N {i: 3})", nil)

	// Crash after three commits: the recovery sees exactly three.
	early := fsys.Snapshot()
	dbEarly, err := Open("prefix.gr", Options{VFS: early, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n := nodeCount(t, dbEarly); n != 3 {
		t.Fatalf("early recovery node count = %d, want 3", n)
	}
	_ = dbEarly.Close()

	// Two more commits on the live database, then a later crash sees all five.
	mustExec(t, db, "CREATE (:N {i: 4})", nil)
	mustExec(t, db, "CREATE (:N {i: 5})", nil)

	db2 := crashReopen(t, fsys, "prefix.gr")
	defer func() { _ = db2.Close() }()
	if n := nodeCount(t, db2); n != 5 {
		t.Fatalf("late recovery node count = %d, want 5", n)
	}
}

// TestExecAbortedWriteLeavesNoTraceAfterCrash confirms a write that aborted on a
// constraint violation leaves nothing behind even across a crash: the rolled-back
// transaction never reached the WAL, so recovery cannot resurrect it, and the
// constraint itself survives (doc 13 §12, §16).
func TestExecAbortedWriteLeavesNoTraceAfterCrash(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "aborted.gr")

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	// A duplicate violates uniqueness and aborts at commit.
	if _, err := db.Exec("CREATE (:Person {email: 'a@x'})", nil); err == nil {
		t.Fatal("duplicate insert was accepted")
	}

	db2 := crashReopen(t, fsys, "aborted.gr")
	defer func() { _ = db2.Close() }()

	if n := nodeCount(t, db2); n != 1 {
		t.Fatalf("recovered node count = %d, want 1 (the aborted insert must leave no trace)", n)
	}
	// The constraint also survived: another duplicate is still rejected.
	if _, err := db2.Exec("CREATE (:Person {email: 'a@x'})", nil); err == nil {
		t.Fatal("duplicate accepted after recovery: the constraint did not survive")
	}
}

// TestExecRecoveredGraphAcceptsMoreWrites confirms a recovered database is fully
// writable, not stuck read-only: after a crash and reopen, a new write commits and
// is itself durable across a second crash. This exercises that recovery leaves the
// WAL in a state the engine can keep appending to.
func TestExecRecoveredGraphAcceptsMoreWrites(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "more.gr")
	mustExec(t, db, "CREATE (:N {i: 1})", nil)

	crashed := fsys.Snapshot()
	db2, err := Open("more.gr", Options{VFS: crashed, SaltSeed: 1})
	if err != nil {
		t.Fatalf("first reopen: %v", err)
	}
	if n := nodeCount(t, db2); n != 1 {
		t.Fatalf("after first recovery node count = %d, want 1", n)
	}
	// Write more against the recovered database.
	mustExec(t, db2, "CREATE (:N {i: 2})", nil)
	if n := nodeCount(t, db2); n != 2 {
		t.Fatalf("after post-recovery write node count = %d, want 2", n)
	}

	// A second crash on the recovered VFS recovers both the replayed and the new
	// write.
	db3 := crashReopen(t, crashed, "more.gr")
	defer func() { _ = db3.Close() }()
	if n := nodeCount(t, db3); n != 2 {
		t.Fatalf("after second recovery node count = %d, want 2", n)
	}
}

// TestExecIndexRecoversAfterCrash confirms a declared index and the data it covers
// both survive a crash, and the recovered index still serves seeks (it is rebuilt
// from the recovered base on reopen, doc 07 §9).
func TestExecIndexRecoversAfterCrash(t *testing.T) {
	fsys := vfs.NewMem()
	db := openOn(t, fsys, "ixrecover.gr")

	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)

	db2 := crashReopen(t, fsys, "ixrecover.gr")
	defer func() { _ = db2.Close() }()

	rows := collectRows(t, db2, "MATCH (p:Person) WHERE p.email = 'a@x' RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 2 {
		t.Fatalf("index-backed count after recovery = %d, want 2", n)
	}
	// The index survived too: a duplicate create of it errors.
	if _, err := db2.Exec("CREATE INDEX FOR (p:Person) ON (p.email)", nil); err == nil {
		t.Fatal("index did not survive recovery")
	}
}

// TestExecEmptyWALReopen confirms reopening a database that committed nothing since
// creation recovers cleanly to an empty graph (the no-write recovery path).
func TestExecEmptyWALReopen(t *testing.T) {
	fsys := vfs.NewMem()
	// openOn creates the file, then we crash without touching it.
	_ = openOn(t, fsys, "empty.gr")
	db2 := crashReopen(t, fsys, "empty.gr")
	defer func() { _ = db2.Close() }()
	if n := nodeCount(t, db2); n != 0 {
		t.Fatalf("empty recovery node count = %d, want 0", n)
	}
	// And it is writable.
	mustExec(t, db2, "CREATE (:N)", nil)
	if n := nodeCount(t, db2); n != 1 {
		t.Fatalf("node count after write = %d, want 1", n)
	}
}
