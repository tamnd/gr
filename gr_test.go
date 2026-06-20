package gr

import (
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const testPath = "campaign.gr"

// The crash campaign is the durability guardrail: it drives the pager and WAL
// directly (not the graph engine on top), because what it proves — that a crash
// at any fault point recovers to a committed prefix — is a property of the
// substrate Open mounts. The graph read path is exercised by TestQuery below.

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

// buildClean creates a pager file with one data page committed at value 0 and
// returns the VFS holding it.
func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	p, err := pager.Open(fsys, testPath, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID() != 1 {
		t.Fatalf("expected the data page to be page 1, got %d", f.ID())
	}
	encodePage(f, 0)
	p.MarkDirty(f)
	p.Unpin(f)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	return fsys
}

// runWorkload reopens the pager on fsys and performs T commits, each rewriting
// page 1 with the next value. It returns the first error (an injected crash ends
// the workload early, which is expected during the campaign).
func runWorkload(fsys vfs.VFS, T int) (err error) {
	p, e := pager.Open(fsys, testPath, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = p.Close() }()
	for j := 1; j <= T; j++ {
		f, e := p.ReadPage(1)
		if e != nil {
			return e
		}
		encodePage(f, uint64(j))
		p.MarkDirty(f)
		p.Unpin(f)
		if e := p.Commit(); e != nil {
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
	p, err := pager.Open(crashed, testPath, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen after crash failed: %v", label, err)
	}
	defer func() { _ = p.Close() }()
	f, err := p.ReadPage(1)
	if err != nil {
		t.Fatalf("%s: ReadPage(1) after crash failed (torn page survived?): %v", label, err)
	}
	v, torn := decodePage(f)
	p.Unpin(f)
	if torn {
		t.Fatalf("%s: recovered page 1 is torn (value %d)", label, v)
	}
	if v > uint64(T) {
		t.Fatalf("%s: recovered value %d exceeds committed max %d", label, v, T)
	}
	if c := p.Header().ChangeCounter; c != v+1 {
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

// TestLifecycle is the open/close demo: Open on a fresh path creates a valid
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

// TestCustomPageSize exercises non-default page sizes through the lifecycle: the
// engine creates and commits its structure at the chosen page size, and a reopen
// reports the same size back.
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
		if err := db.Close(); err != nil {
			t.Fatalf("ps=%d: close: %v", ps, err)
		}
		db2, err := Open("p.gr", Options{VFS: fsys})
		if err != nil {
			t.Fatalf("ps=%d: reopen: %v", ps, err)
		}
		if db2.PageSize() != ps {
			t.Fatalf("ps=%d: reopened size = %d", ps, db2.PageSize())
		}
		_ = db2.Close()
	}
}

// TestQuery is the M2 read-path demo through the library surface: a parameterless
// RETURN, returning a literal computed with no graph access, streamed by column.
func TestQuery(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("q.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	res, err := db.Query("RETURN 1 + 2 AS n", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = res.Close() }()

	if cols := res.Columns(); len(cols) != 1 || cols[0] != "n" {
		t.Fatalf("columns = %v, want [n]", cols)
	}
	row, ok, err := res.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !ok {
		t.Fatal("expected one row")
	}
	if n, ok := row[0].AsInt(); !ok || n != 3 {
		t.Fatalf("row[0] = %v, want 3", row[0])
	}
	if _, ok, _ := res.Next(); ok {
		t.Fatal("expected exactly one row")
	}
}

// TestQueryParams threads a parameter through the read path and reads it back.
func TestQueryParams(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("qp.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	params := map[string]value.Value{"x": value.Int(41)}
	res, err := db.Query("RETURN $x + 1 AS y", params)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = res.Close() }()

	row, ok, err := res.Next()
	if err != nil || !ok {
		t.Fatalf("next: ok=%v err=%v", ok, err)
	}
	if y, ok := row[0].AsInt(); !ok || y != 42 {
		t.Fatalf("row[0] = %v, want 42", row[0])
	}
}

// TestQueryPlanCache asserts a repeated query shape compiles once: the second run
// of the same text hits the plan cache, so the cache holds a single entry, and a
// different text adds a second.
func TestQueryPlanCache(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("pc.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	run := func(q string) {
		res, err := db.Query(q, nil)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		for {
			if _, ok, err := res.Next(); err != nil {
				t.Fatal(err)
			} else if !ok {
				break
			}
		}
		_ = res.Close()
	}

	run("RETURN 1 AS n")
	run("RETURN 1 AS n") // same shape: a cache hit, no new entry
	if got := db.cache.Len(); got != 1 {
		t.Fatalf("after two runs of one shape, cache len = %d, want 1", got)
	}
	run("  RETURN 1 AS n  ") // only outer whitespace differs: still the same shape
	if got := db.cache.Len(); got != 1 {
		t.Fatalf("outer whitespace should normalize to the same key, len = %d, want 1", got)
	}
	run("RETURN 2 AS n") // a different shape: a new entry
	if got := db.cache.Len(); got != 2 {
		t.Fatalf("after a second distinct shape, cache len = %d, want 2", got)
	}
}

// TestQueryOnClosed reports the closed-database error rather than panicking.
func TestQueryOnClosed(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("c.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query("RETURN 1", nil); err != ErrClosed {
		t.Fatalf("query on closed db = %v, want ErrClosed", err)
	}
}

// collectRows runs a read query and returns its rows as name-keyed maps, a small
// helper for the write tests that read back what a CREATE produced.
// openMem opens a fresh in-memory database for a test, failing the test on a
// setup error so each test body can assume a usable handle.
func openMem(t *testing.T, name string) *DB {
	t.Helper()
	db, err := Open(name, Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func collectRows(t *testing.T, db *DB, q string, params map[string]value.Value) []map[string]value.Value {
	t.Helper()
	res, err := db.Query(q, params)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	cols := res.Columns()
	var out []map[string]value.Value
	for {
		row, ok, err := res.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		m := map[string]value.Value{}
		for i, c := range cols {
			m[c] = row[i]
		}
		out = append(out, m)
	}
	return out
}

// TestExecCreateNode creates a labeled node with a property and reads it back,
// checking both the side-effect summary and the persisted value.
func TestExecCreateNode(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("create.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	sum, err := db.Exec("CREATE (a:Person {name: 'Ada', age: 36})", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.NodesCreated != 1 || sum.LabelsAdded != 1 || sum.PropertiesSet != 2 {
		t.Fatalf("summary = %+v, want 1 node, 1 label, 2 props", sum)
	}

	rows := collectRows(t, db, "MATCH (p:Person) RETURN p.name AS name, p.age AS age", nil)
	if len(rows) != 1 {
		t.Fatalf("read back %d rows, want 1", len(rows))
	}
	if name, _ := rows[0]["name"].AsString(); name != "Ada" {
		t.Fatalf("name = %q, want Ada", name)
	}
	if age, _ := rows[0]["age"].AsInt(); age != 36 {
		t.Fatalf("age = %d, want 36", age)
	}
}

// TestExecCreateNullProperty confirms a property whose value is null is left
// unset and not counted in the summary.
func TestExecCreateNullProperty(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("nullprop.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	sum, err := db.Exec("CREATE (a:Person {name: 'x', nick: null})", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 1 {
		t.Fatalf("PropertiesSet = %d, want 1 (null is not set)", sum.PropertiesSet)
	}
}

// TestExecCreateRelationship creates two nodes and a typed relationship with a
// property, then reads the pattern back to confirm the edge and its direction.
func TestExecCreateRelationship(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("rel.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	sum, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS {since: 2020}]->(b:Person {name: 'B'})", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.NodesCreated != 2 || sum.RelationshipsCreated != 1 || sum.PropertiesSet != 3 {
		t.Fatalf("summary = %+v, want 2 nodes, 1 rel, 3 props", sum)
	}

	rows := collectRows(t, db,
		"MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a.name AS a, b.name AS b, r.since AS since", nil)
	if len(rows) != 1 {
		t.Fatalf("read back %d rows, want 1", len(rows))
	}
	a, _ := rows[0]["a"].AsString()
	b, _ := rows[0]["b"].AsString()
	since, _ := rows[0]["since"].AsInt()
	if a != "A" || b != "B" || since != 2020 {
		t.Fatalf("row = a:%q b:%q since:%d, want A B 2020", a, b, since)
	}
}

// TestExecCreatePerMatchedRow confirms a CREATE after a MATCH runs once per
// matched row, giving each matched node a new neighbor.
func TestExecCreatePerMatchedRow(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("permatch.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (:Person {name: 'A'}), (:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (p:Person) CREATE (p)-[:HAS]->(:Pet)", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.NodesCreated != 2 || sum.RelationshipsCreated != 2 {
		t.Fatalf("summary = %+v, want 2 nodes and 2 rels (one per matched person)", sum)
	}
	rows := collectRows(t, db, "MATCH (:Person)-[:HAS]->(pet:Pet) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 2 {
		t.Fatalf("pet edges = %d, want 2", n)
	}
}

// TestExecSetProperty sets a new property and overwrites an existing one, then
// reads both back. Each non-null assignment counts a property set.
func TestExecSetProperty(t *testing.T) {
	db := openMem(t, "setprop.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) SET a.name = 'Grace', a.age = 45", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 2 {
		t.Fatalf("PropertiesSet = %d, want 2", sum.PropertiesSet)
	}
	rows := collectRows(t, db, "MATCH (a:Person) RETURN a.name AS name, a.age AS age", nil)
	if name, _ := rows[0]["name"].AsString(); name != "Grace" {
		t.Fatalf("name = %q, want Grace", name)
	}
	if age, _ := rows[0]["age"].AsInt(); age != 45 {
		t.Fatalf("age = %d, want 45", age)
	}
}

// TestExecSetSameValue confirms SET to the unchanged value still counts a
// property set, the openCypher rule (doc 13 §6.22).
func TestExecSetSameValue(t *testing.T) {
	db := openMem(t, "setsame.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {age: 30})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) SET a.age = 30", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 1 {
		t.Fatalf("PropertiesSet = %d, want 1 (same value still counts)", sum.PropertiesSet)
	}
}

// TestExecSetNullRemoves confirms SET of a null value removes the property and
// counts it only when the property was present (doc 13 §6.22, §7.11).
func TestExecSetNullRemoves(t *testing.T) {
	db := openMem(t, "setnull.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'Ada', age: 36})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// age is present, so setting it null removes and counts it.
	sum, err := db.Exec("MATCH (a:Person) SET a.age = null", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 1 {
		t.Fatalf("PropertiesSet = %d, want 1", sum.PropertiesSet)
	}
	// age is now absent, so a second null set counts nothing.
	sum, err = db.Exec("MATCH (a:Person) SET a.age = null", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 0 {
		t.Fatalf("PropertiesSet = %d, want 0 (already absent)", sum.PropertiesSet)
	}
	rows := collectRows(t, db, "MATCH (a:Person) RETURN a.age AS age", nil)
	if !rows[0]["age"].IsNull() {
		t.Fatalf("age = %v, want null after removal", rows[0]["age"])
	}
}

// TestExecSetLabel adds a label, counting only the net addition: re-adding an
// existing label counts nothing (doc 13 §6.7).
func TestExecSetLabel(t *testing.T) {
	db := openMem(t, "setlabel.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) SET a:Admin", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.LabelsAdded != 1 {
		t.Fatalf("LabelsAdded = %d, want 1", sum.LabelsAdded)
	}
	// Re-adding Admin and Person, both present, counts nothing.
	sum, err = db.Exec("MATCH (a:Admin) SET a:Admin:Person", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.LabelsAdded != 0 {
		t.Fatalf("LabelsAdded = %d, want 0 (labels already present)", sum.LabelsAdded)
	}
	rows := collectRows(t, db, "MATCH (a:Admin) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("Admin nodes = %d, want 1", n)
	}
}

// TestExecRemoveProperty removes a present property (counted) then an absent one
// (not counted), folding both under PropertiesSet (doc 13 §7.11).
func TestExecRemoveProperty(t *testing.T) {
	db := openMem(t, "rmprop.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'Ada', age: 36})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) REMOVE a.age", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 1 {
		t.Fatalf("PropertiesSet = %d, want 1", sum.PropertiesSet)
	}
	// age is gone, so removing it again counts nothing.
	sum, err = db.Exec("MATCH (a:Person) REMOVE a.age", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 0 {
		t.Fatalf("PropertiesSet = %d, want 0 (already absent)", sum.PropertiesSet)
	}
	rows := collectRows(t, db, "MATCH (a:Person) RETURN a.age AS age", nil)
	if !rows[0]["age"].IsNull() {
		t.Fatalf("age = %v, want null after removal", rows[0]["age"])
	}
}

// TestExecRemoveUnknownProperty confirms removing a property the catalog never
// interned is a no-op that counts nothing and does not create the key.
func TestExecRemoveUnknownProperty(t *testing.T) {
	db := openMem(t, "rmunknown.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) REMOVE a.nope", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 0 {
		t.Fatalf("PropertiesSet = %d, want 0", sum.PropertiesSet)
	}
}

// TestExecRemoveLabel removes a present label (counted) then an absent one (not
// counted), and confirms the node no longer matches the removed label.
func TestExecRemoveLabel(t *testing.T) {
	db := openMem(t, "rmlabel.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person:Admin {name: 'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Admin) REMOVE a:Admin", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.LabelsRemoved != 1 {
		t.Fatalf("LabelsRemoved = %d, want 1", sum.LabelsRemoved)
	}
	// Admin is gone, so removing it again counts nothing.
	sum, err = db.Exec("MATCH (a:Person) REMOVE a:Admin", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.LabelsRemoved != 0 {
		t.Fatalf("LabelsRemoved = %d, want 0 (already absent)", sum.LabelsRemoved)
	}
	rows := collectRows(t, db, "MATCH (a:Admin) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("Admin nodes = %d, want 0 after removal", n)
	}
}

// TestExecSetRelProperty sets a property on a relationship and reads it back.
func TestExecSetRelProperty(t *testing.T) {
	db := openMem(t, "setrel.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (:Person)-[r:KNOWS]->(:Person) SET r.since = 2020", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.PropertiesSet != 1 {
		t.Fatalf("PropertiesSet = %d, want 1", sum.PropertiesSet)
	}
	rows := collectRows(t, db, "MATCH (:Person)-[r:KNOWS]->(:Person) RETURN r.since AS since", nil)
	if since, _ := rows[0]["since"].AsInt(); since != 2020 {
		t.Fatalf("since = %d, want 2020", since)
	}
}

// TestExecSetMapDeferred confirms the map forms of SET are rejected for now,
// pointing at the later M3 milestone rather than silently misbehaving.
func TestExecSetMapDeferred(t *testing.T) {
	db := openMem(t, "setmap.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person)", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec("MATCH (a:Person) SET a += {age: 1}", nil); err == nil {
		t.Fatal("map-merge SET should be rejected for now")
	}
	if _, err := db.Exec("MATCH (a:Person) SET a = {age: 1}", nil); err == nil {
		t.Fatal("map-replace SET should be rejected for now")
	}
}

// TestExecDeleteNode deletes an unattached node and confirms the count and that
// it is gone.
func TestExecDeleteNode(t *testing.T) {
	db := openMem(t, "delnode.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) DELETE a", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.NodesDeleted != 1 {
		t.Fatalf("NodesDeleted = %d, want 1", sum.NodesDeleted)
	}
	rows := collectRows(t, db, "MATCH (a:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("Person nodes = %d, want 0 after delete", n)
	}
}

// TestExecDeleteRelationship deletes a relationship and confirms both endpoints
// survive while the edge is gone.
func TestExecDeleteRelationship(t *testing.T) {
	db := openMem(t, "delrel.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (:Person)-[r:KNOWS]->(:Person) DELETE r", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.RelationshipsDeleted != 1 {
		t.Fatalf("RelationshipsDeleted = %d, want 1", sum.RelationshipsDeleted)
	}
	rows := collectRows(t, db, "MATCH (:Person)-[r:KNOWS]->(:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("KNOWS edges = %d, want 0 after delete", n)
	}
	rows = collectRows(t, db, "MATCH (p:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 2 {
		t.Fatalf("Person nodes = %d, want 2 (endpoints survive)", n)
	}
}

// TestExecDeleteAttachedNodeFails confirms a plain DELETE of a node that still
// has relationships fails the no-dangling check and leaves the graph untouched
// (doc 13 §9.4).
func TestExecDeleteAttachedNodeFails(t *testing.T) {
	db := openMem(t, "delattached.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec("MATCH (a:Person)-[:KNOWS]->(:Person) DELETE a", nil); err == nil {
		t.Fatal("plain DELETE of an attached node should fail")
	}
	// The aborted write must leave both nodes and the edge in place.
	rows := collectRows(t, db, "MATCH (:Person)-[r:KNOWS]->(:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("KNOWS edges = %d, want 1 (write aborted)", n)
	}
}

// TestExecDetachDelete confirms DETACH DELETE removes a node and its incident
// relationship, leaving the neighbor intact but disconnected (doc 13 §9.5).
func TestExecDetachDelete(t *testing.T) {
	db := openMem(t, "detach.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person {name: 'A'}) DETACH DELETE a", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.NodesDeleted != 1 || sum.RelationshipsDeleted != 1 {
		t.Fatalf("summary = %+v, want 1 node and 1 rel deleted", sum)
	}
	rows := collectRows(t, db, "MATCH (p:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("Person nodes = %d, want 1 (neighbor survives)", n)
	}
	rows = collectRows(t, db, "MATCH ()-[r]->() RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("edges = %d, want 0 after detach delete", n)
	}
}

// TestExecDeleteRelThenNode confirms a comma DELETE listing a relationship and
// its endpoints deletes the relationship before the nodes, so no-dangling holds
// (doc 13 §9.12).
func TestExecDeleteRelThenNode(t *testing.T) {
	db := openMem(t, "delrelnode.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person)-[r:KNOWS]->(b:Person) DELETE r, a, b", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.NodesDeleted != 2 || sum.RelationshipsDeleted != 1 {
		t.Fatalf("summary = %+v, want 2 nodes and 1 rel deleted", sum)
	}
	rows := collectRows(t, db, "MATCH (p:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("Person nodes = %d, want 0 after delete", n)
	}
}

// TestExecDeleteIdempotent deletes one edge reached from both sides by an
// undirected match; the second visit finds it already gone and counts nothing
// (doc 13 §9.6).
func TestExecDeleteIdempotent(t *testing.T) {
	db := openMem(t, "delidem.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person)-[r:KNOWS]-(b:Person) DELETE r", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.RelationshipsDeleted != 1 {
		t.Fatalf("RelationshipsDeleted = %d, want 1 (idempotent)", sum.RelationshipsDeleted)
	}
}

// TestExecDeleteNullNoop confirms deleting a null target deletes nothing and
// raises no error.
func TestExecDeleteNullNoop(t *testing.T) {
	db := openMem(t, "delnull.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a:Person {name: 'A'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum, err := db.Exec("MATCH (a:Person) OPTIONAL MATCH (a)-[r:KNOWS]->() DELETE r", nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sum.RelationshipsDeleted != 0 {
		t.Fatalf("RelationshipsDeleted = %d, want 0 (null target)", sum.RelationshipsDeleted)
	}
	rows := collectRows(t, db, "MATCH (p:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("Person nodes = %d, want 1 (node untouched)", n)
	}
}

// TestQueryRejectsWrites confirms Query refuses a write statement, directing the
// caller to Exec, and that nothing is mutated.
func TestQueryRejectsWrites(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("reject.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Query("CREATE (a:Person)", nil); err != ErrReadQuery {
		t.Fatalf("Query on a write = %v, want ErrReadQuery", err)
	}
	rows := collectRows(t, db, "MATCH (p:Person) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("a rejected write must not mutate, found %d nodes", n)
	}
}

// TestExecCreatePersistsAcrossReopen confirms a committed CREATE survives closing
// and reopening the database.
func TestExecCreatePersistsAcrossReopen(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("persist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE (a:Person {name: 'Grace'})", nil); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open("persist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	rows := collectRows(t, db2, "MATCH (p:Person) RETURN p.name AS name", nil)
	if len(rows) != 1 {
		t.Fatalf("after reopen, %d nodes, want 1", len(rows))
	}
	if name, _ := rows[0]["name"].AsString(); name != "Grace" {
		t.Fatalf("name = %q, want Grace", name)
	}
}

// TestExecOnClosed reports the closed-database error rather than panicking.
func TestExecOnClosed(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("ec.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE (a)", nil); err != ErrClosed {
		t.Fatalf("exec on closed db = %v, want ErrClosed", err)
	}
}
