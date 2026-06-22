package engine

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// nodePosFor resolves a node id to its dense position through the engine's id-map,
// the position the segmented column store is keyed by.
func nodePosFor(t *testing.T, e *DiskEngine, id NodeID) uint64 {
	t.Helper()
	pos, ok := e.ids.Pos(uint64(id))
	if !ok {
		t.Fatalf("node %d has no dense position", id)
	}
	return pos
}

// TestCheckpointPopulatesSegmentedBase is the W1 gate: after a checkpoint the
// segmented base holds every property the naive column does, with the same value
// at the same position, and an unset property reads absent. The read path still
// answers from the naive columns, so this inspects the segmented store directly.
func TestCheckpointPopulatesSegmentedBase(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "seg.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	age, _ := e.Intern(catalog.KindPropKey, "age")
	score, _ := e.Intern(catalog.KindPropKey, "score")

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	b, _ := tx.CreateNode(nil)
	c, _ := tx.CreateNode(nil)
	// a has all three properties, b only name, c none.
	tx.SetNodeProperty(a, name, value.String("alice"))
	tx.SetNodeProperty(a, age, value.Int(30))
	tx.SetNodeProperty(a, score, value.Float(9.5))
	tx.SetNodeProperty(b, name, value.String("bob"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	posA := nodePosFor(t, e, a)
	posB := nodePosFor(t, e, b)
	posC := nodePosFor(t, e, c)

	// Present values land in the segmented base unchanged.
	wantPresent(t, e, toCat(name), posA, value.String("alice"))
	wantPresent(t, e, toCat(age), posA, value.Int(30))
	wantPresent(t, e, toCat(score), posA, value.Float(9.5))
	wantPresent(t, e, toCat(name), posB, value.String("bob"))

	// b has no age or score, c has nothing: those read absent.
	wantAbsent(t, e, toCat(age), posB)
	wantAbsent(t, e, toCat(score), posB)
	wantAbsent(t, e, toCat(name), posC)
}

// TestCheckpointDrainsNaiveDelta checkpoints a graph with both node and
// relationship properties, then asserts the naive delta is drained (the folded
// values now live only in the segmented base) while the SPI still reads every
// value back. This is the W2 contract: the read path consults the segmented base
// with the naive store as a delta over it.
func TestCheckpointDrainsNaiveDelta(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "drain.gr")
	defer e.Close()

	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	tag, _ := e.Intern(catalog.KindPropKey, "tag")
	since, _ := e.Intern(catalog.KindPropKey, "since")

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	b, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, tag, value.String("x"))
	tx.SetNodeProperty(b, tag, value.String("y"))
	r, _ := tx.CreateRel(a, b, knows)
	tx.SetRelProperty(r, since, value.Int(2015))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// The delta is drained: no key carries a present value in the naive store.
	if got := len(e.ncols.Keys()); got != 0 {
		t.Fatalf("node delta not drained: %d keys remain", got)
	}
	if got := len(e.rcols.Keys()); got != 0 {
		t.Fatalf("rel delta not drained: %d keys remain", got)
	}

	// The values now read back through the segmented base via the SPI.
	rx, _ := e.Begin(false)
	defer rx.Abort()
	if v, _ := rx.NodeProperty(a, tag); mustStr(t, v) != "x" {
		t.Fatalf("a.tag = %v, want x", v)
	}
	if v, _ := rx.NodeProperty(b, tag); mustStr(t, v) != "y" {
		t.Fatalf("b.tag = %v, want y", v)
	}
	if v, _ := rx.RelProperty(r, since); mustInt(t, v) != 2015 {
		t.Fatalf("r.since = %v, want 2015", v)
	}
}

// TestDeltaShadowsBaseAfterCheckpoint proves a write after a checkpoint shadows
// the folded base value, and a removal after a checkpoint hides it, both through
// the naive delta over the segmented base.
func TestDeltaShadowsBaseAfterCheckpoint(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "shadow.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	b, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("old-a"))
	tx.SetNodeProperty(b, name, value.String("keep-b"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// After the checkpoint: overwrite a, remove b. Both values are only in the base.
	tx2, _ := e.Begin(true)
	tx2.SetNodeProperty(a, name, value.String("new-a"))
	tx2.SetNodeProperty(b, name, value.Null) // removal -> tombstone over the base
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()
	if v, _ := rx.NodeProperty(a, name); mustStr(t, v) != "new-a" {
		t.Fatalf("a.name = %v, want new-a (delta shadows base)", v)
	}
	if v, err := rx.NodeProperty(b, name); err != nil || !v.IsNull() {
		t.Fatalf("b.name = %v err=%v, want null (tombstone hides base)", v, err)
	}
}

// TestMultiCheckpointAccumulates writes across two checkpoints and asserts both
// values survive: the second fold merges the new delta over the base the first
// fold produced, rather than rebuilding from the delta alone.
func TestMultiCheckpointAccumulates(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "accum.gr")
	defer e.Close()

	first, _ := e.Intern(catalog.KindPropKey, "first")
	second, _ := e.Intern(catalog.KindPropKey, "second")

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, first, value.Int(1))
	tx.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// A different key after the first fold; the second fold must keep the first.
	tx2, _ := e.Begin(true)
	tx2.SetNodeProperty(a, second, value.Int(2))
	tx2.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()
	if v, _ := rx.NodeProperty(a, first); mustInt(t, v) != 1 {
		t.Fatalf("a.first = %v, want 1 (survived second fold)", v)
	}
	if v, _ := rx.NodeProperty(a, second); mustInt(t, v) != 2 {
		t.Fatalf("a.second = %v, want 2", v)
	}
}

// TestSnapshotStableAcrossCheckpoint proves an open read snapshot keeps seeing its
// values even after a later transaction overwrites them and a checkpoint folds the
// new values into the base: the overlay still answers the old snapshot.
func TestSnapshotStableAcrossCheckpoint(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "snap.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("v1"))
	tx.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Open a reader, then overwrite and checkpoint underneath it.
	old, _ := e.Begin(false)
	defer old.Abort()

	tx2, _ := e.Begin(true)
	tx2.SetNodeProperty(a, name, value.String("v2"))
	tx2.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	if v, _ := old.NodeProperty(a, name); mustStr(t, v) != "v1" {
		t.Fatalf("old snapshot a.name = %v, want v1", v)
	}
	now, _ := e.Begin(false)
	defer now.Abort()
	if v, _ := now.NodeProperty(a, name); mustStr(t, v) != "v2" {
		t.Fatalf("fresh snapshot a.name = %v, want v2", v)
	}
}

// TestPropertyKeysAfterCheckpoint proves NodePropertyKeys finds a property folded
// into the base, not just one written since the last checkpoint, because the
// candidate keys union the delta and the base.
func TestPropertyKeysAfterCheckpoint(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "keys.gr")
	defer e.Close()

	folded, _ := e.Intern(catalog.KindPropKey, "folded")
	fresh, _ := e.Intern(catalog.KindPropKey, "fresh")

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, folded, value.Int(1))
	tx.Commit()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	tx2, _ := e.Begin(true)
	tx2.SetNodeProperty(a, fresh, value.Int(2))
	tx2.Commit()

	rx, _ := e.Begin(false)
	defer rx.Abort()
	keys, err := rx.NodePropertyKeys(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("property keys = %v, want both folded and fresh", keys)
	}
}

// TestSegmentedBaseRecoversAfterCrash proves the checkpoint's section swap is
// crash safe: after a checkpoint commits, snapshotting the media without a clean
// close and reopening recovers the folded base from the WAL, with the values
// intact and the naive delta drained.
func TestSegmentedBaseRecoversAfterCrash(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "crash.gr")

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("durable"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Crash: snapshot the media with no clean close, reopen on the copy.
	crashed := fsys.Snapshot()
	e2, err := Open(crashed, "crash.gr", pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer e2.Close()

	if got := len(e2.ncols.Keys()); got != 0 {
		t.Fatalf("recovered delta not drained: %d keys", got)
	}
	name2, _ := e2.Lookup(catalog.KindPropKey, "name")
	rx, _ := e2.Begin(false)
	defer rx.Abort()
	if v, _ := rx.NodeProperty(a, name2); mustStr(t, v) != "durable" {
		t.Fatalf("recovered a.name = %v, want durable", v)
	}
}

// TestSegmentedBaseSurvivesReopen proves the folded segments are durable: after a
// checkpoint, close, and reopen, the segmented base reopens from its section and
// still holds the values.
func TestSegmentedBaseSurvivesReopen(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "reopen.gr")

	name, _ := e.Intern(catalog.KindPropKey, "name")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode(nil)
	tx.SetNodeProperty(a, name, value.String("ada"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	posA := nodePosFor(t, e, a)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openDisk(t, fsys, "reopen.gr")
	defer e2.Close()
	name2, _ := e2.Lookup(catalog.KindPropKey, "name")
	wantPresent(t, e2, toCat(name2), posA, value.String("ada"))
}

// TestSegmentedBaseCrossesSegmentBoundary folds more positions than fit in one
// segment, so the store must split the column into several contiguous segments and
// still read every value back, exercising the per-column binary search.
func TestSegmentedBaseCrossesSegmentBoundary(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "boundary.gr")
	defer e.Close()

	val, _ := e.Intern(catalog.KindPropKey, "val")
	const n = segPositions + segPositions/2 // crosses two boundaries

	tx, _ := e.Begin(true)
	ids := make([]NodeID, n)
	for i := range ids {
		id, _ := tx.CreateNode(nil)
		ids[i] = id
		tx.SetNodeProperty(id, val, value.Int(int64(i)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// The column spans more than one segment now, and every value reads back
	// through the per-column binary search.
	for i, id := range ids {
		wantPresent(t, e, toCat(val), nodePosFor(t, e, id), value.Int(int64(i)))
	}
}

// TestCheckpointReclaimsOldBase is the W4c gate: a checkpoint frees the old
// segmented base and the old naive delta it replaces, so a steady stream of
// checkpoints over an unchanging graph reuses those pages instead of growing the
// file. Without the frees each checkpoint leaks a fresh base plus fresh delta and
// the page count climbs without bound; with them it holds flat.
func TestCheckpointReclaimsOldBase(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "reclaim.gr")
	defer e.Close()

	name, _ := e.Intern(catalog.KindPropKey, "name")
	age, _ := e.Intern(catalog.KindPropKey, "age")

	// A small fixed graph: enough columns and positions that a fold allocates real
	// base and delta pages, so a leak would be visible in the page count.
	tx, _ := e.Begin(true)
	var first NodeID
	for i := range 64 {
		n, _ := tx.CreateNode(nil)
		if i == 0 {
			first = n
		}
		tx.SetNodeProperty(n, name, value.String("node"))
		tx.SetNodeProperty(n, age, value.Int(int64(i)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Warm up to steady state: the first checkpoints fill the free list and the
	// fresh stores start drawing from it, so the page count settles.
	for range 3 {
		if err := e.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	before := e.p.Header().PageCount

	// Many more checkpoints over the same graph must not grow the file: every fresh
	// base and delta reuses the pages the prior checkpoint freed.
	for range 20 {
		if err := e.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	after := e.p.Header().PageCount
	if after != before {
		t.Fatalf("page count grew across checkpoints: %d -> %d (old base or delta not reclaimed)", before, after)
	}

	// The data still reads back correctly after all the page reuse.
	pos := nodePosFor(t, e, first)
	wantPresent(t, e, toCat(name), pos, value.String("node"))
	wantPresent(t, e, toCat(age), pos, value.Int(0))
}

// wantPresent asserts the segmented base for the node element kind holds value v
// at (key, pos).
func wantPresent(t *testing.T, e *DiskEngine, key uint32, pos uint64, want value.Value) {
	t.Helper()
	v, ok, err := e.nseg.Get(key, pos)
	if err != nil {
		t.Fatalf("seg get key %d pos %d: %v", key, pos, err)
	}
	if !ok {
		t.Fatalf("seg get key %d pos %d: absent, want %v", key, pos, want)
	}
	if !v.Equal(want) {
		t.Fatalf("seg get key %d pos %d = %v, want %v", key, pos, v, want)
	}
}

// wantAbsent asserts the segmented base holds no value at (key, pos).
func wantAbsent(t *testing.T, e *DiskEngine, key uint32, pos uint64) {
	t.Helper()
	if _, ok, err := e.nseg.Get(key, pos); err != nil || ok {
		t.Fatalf("seg get key %d pos %d: ok=%v err=%v, want absent", key, pos, ok, err)
	}
}

