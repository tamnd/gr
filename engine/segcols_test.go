package engine

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/column"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
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

// TestSegmentedBaseMatchesNaive checkpoints a graph with both node and
// relationship properties and asserts the segmented base agrees with the naive
// column at every live position, the invariant the read-path flip will rely on.
func TestSegmentedBaseMatchesNaive(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "agree.gr")
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

	assertSegMatchesNaive(t, e.ncols, e.nseg, uint64(e.nodes.Count()))
	assertSegMatchesNaive(t, e.rcols, e.rseg, uint64(e.rels.Count()))
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

// assertSegMatchesNaive checks the segmented store agrees with the naive column at
// every position over [0, count) for every key the naive store knows.
func assertSegMatchesNaive(t *testing.T, naive *column.Columns, seg *colsegstore.Store, count uint64) {
	t.Helper()
	for _, key := range naive.Keys() {
		for pos := range count {
			nv, nok, err := naive.Get(key, pos)
			if err != nil {
				t.Fatal(err)
			}
			sv, sok, err := seg.Get(key, pos)
			if err != nil {
				t.Fatal(err)
			}
			if nok != sok {
				t.Fatalf("key %d pos %d presence: naive=%v seg=%v", key, pos, nok, sok)
			}
			if nok && !nv.Equal(sv) {
				t.Fatalf("key %d pos %d: naive=%v seg=%v", key, pos, nv, sv)
			}
		}
	}
}
