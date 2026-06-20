package engine

import (
	"sync"
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// TestStatisticsMatchGraph is the M1 statistics-accuracy gate (doc 25 §4.4):
// after a build the engine's per-label and per-type counts match the actual
// graph, they track deletes and label changes, survive a reopen, and an aborted
// transaction leaves them unchanged.
func TestStatisticsMatchGraph(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "stats.gr")

	person, _ := e.Intern(catalog.KindLabel, "Person")
	city, _ := e.Intern(catalog.KindLabel, "City")
	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	livesin, _ := e.Intern(catalog.KindRelType, "LIVES_IN")

	wantLabel := func(when string, label Token, want uint64) {
		t.Helper()
		got, err := e.NodeCountByLabel(label)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: NodeCountByLabel = %d, want %d", when, got, want)
		}
	}
	wantType := func(when string, ty Token, want uint64) {
		t.Helper()
		got, err := e.RelCountByType(ty)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: RelCountByType = %d, want %d", when, got, want)
		}
	}

	// Build: 3 Person nodes, 2 City nodes, one node carrying both labels, plus
	// KNOWS and LIVES_IN edges.
	w, _ := e.Begin(true)
	p1, _ := w.CreateNode([]Token{person})
	p2, _ := w.CreateNode([]Token{person})
	p3, _ := w.CreateNode([]Token{person})
	c1, _ := w.CreateNode([]Token{city})
	c2, _ := w.CreateNode([]Token{city})
	r1, _ := w.CreateRel(p1, p2, knows)
	w.CreateRel(p2, p3, knows)
	w.CreateRel(p1, c1, livesin)
	w.CreateRel(p2, c2, livesin)
	w.Commit()

	wantLabel("after build", person, 3)
	wantLabel("after build", city, 2)
	wantType("after build", knows, 2)
	wantType("after build", livesin, 2)

	// A node gaining a second label bumps that label's count.
	w2, _ := e.Begin(true)
	w2.AddLabel(p3, city) // p3 is now Person and City
	w2.Commit()
	wantLabel("after add-label", person, 3)
	wantLabel("after add-label", city, 3)

	// Deleting a relationship and removing a label decrement their counts.
	w3, _ := e.Begin(true)
	w3.DeleteRel(r1)
	w3.RemoveLabel(p3, city)
	w3.Commit()
	wantType("after rel delete", knows, 1)
	wantLabel("after remove-label", city, 2)

	// An aborted transaction leaves the counts unchanged.
	w4, _ := e.Begin(true)
	w4.CreateNode([]Token{person})
	w4.CreateRel(p2, p3, knows)
	w4.Abort()
	wantLabel("after abort", person, 3)
	wantType("after abort", knows, 1)

	// The counts are durable across a reopen.
	e.Close()
	e2 := openDisk(t, fsys, "stats.gr")
	defer e2.Close()
	if got, _ := e2.NodeCountByLabel(person); got != 3 {
		t.Fatalf("after reopen: Person = %d, want 3", got)
	}
	if got, _ := e2.NodeCountByLabel(city); got != 2 {
		t.Fatalf("after reopen: City = %d, want 2", got)
	}
	if got, _ := e2.RelCountByType(knows); got != 1 {
		t.Fatalf("after reopen: KNOWS = %d, want 1", got)
	}
	if got, _ := e2.RelCountByType(livesin); got != 2 {
		t.Fatalf("after reopen: LIVES_IN = %d, want 2", got)
	}
}

// TestDenseNodeDegreeEngine drives the dense-node mechanism through the engine
// SPI (doc 04 §12; doc 25 deliverable 10): a supernode with high degree across
// two types reports per-type, per-direction degree in O(1) without scanning all
// its edges, a typed expand sees only that type's slice, and the count tracks
// writes, deletes, the writer's own uncommitted work, and a checkpoint.
func TestDenseNodeDegreeEngine(t *testing.T) {
	const degKnows, degLikes = 600, 150
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "dense.gr")
	defer e.Close()

	person, _ := e.Intern(catalog.KindLabel, "Person")
	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	likes, _ := e.Intern(catalog.KindRelType, "LIKES")

	w, _ := e.Begin(true)
	s, _ := w.CreateNode([]Token{person})
	nbr := make([]NodeID, 0, degKnows)
	for range degKnows {
		n, _ := w.CreateNode([]Token{person})
		nbr = append(nbr, n)
	}
	var firstKnows RelID
	for i, n := range nbr {
		r, _ := w.CreateRel(s, n, knows)
		if i == 0 {
			firstKnows = r
		}
	}
	for i := range degLikes {
		w.CreateRel(s, nbr[i], likes)
	}
	// Read-your-writes: the writer sees its own uncommitted degree.
	if d, _ := w.Degree(s, knows, Outgoing); d != degKnows {
		t.Fatalf("read-your-writes degree(KNOWS) = %d, want %d", d, degKnows)
	}
	w.Commit()

	// A committed reader sees the per-type, per-direction degrees and the wildcard sum.
	r, _ := e.Begin(false)
	defer r.Abort()
	if d, _ := r.Degree(s, knows, Outgoing); d != degKnows {
		t.Fatalf("degree(KNOWS,out) = %d, want %d", d, degKnows)
	}
	if d, _ := r.Degree(s, likes, Outgoing); d != degLikes {
		t.Fatalf("degree(LIKES,out) = %d, want %d", d, degLikes)
	}
	if d, _ := r.Degree(s, knows, Incoming); d != 0 {
		t.Fatalf("degree(KNOWS,in) = %d, want 0", d)
	}
	if d, _ := r.Degree(s, 0, Outgoing); d != degKnows+degLikes {
		t.Fatalf("degree(all types,out) = %d, want %d", d, degKnows+degLikes)
	}
	// Typed expand selectivity: a KNOWS expand yields only the KNOWS slice.
	var seen int
	r.Expand(s, knows, Outgoing, func(n Neighbor) error {
		if n.Type != knows {
			t.Fatalf("KNOWS expand yielded a %v edge", n.Type)
		}
		seen++
		return nil
	})
	if seen != degKnows {
		t.Fatalf("KNOWS expand saw %d edges, want %d", seen, degKnows)
	}

	// Deleting one KNOWS edge drops the KNOWS degree by one, LIKES untouched.
	w2, _ := e.Begin(true)
	w2.DeleteRel(firstKnows)
	if d, _ := w2.Degree(s, knows, Outgoing); d != degKnows-1 {
		t.Fatalf("degree after delete = %d, want %d", d, degKnows-1)
	}
	if d, _ := w2.Degree(s, likes, Outgoing); d != degLikes {
		t.Fatalf("LIKES degree after KNOWS delete = %d, want %d", d, degLikes)
	}
	w2.Commit()

	// A checkpoint folds the tail into the base; degree comes from the offset
	// arrays and stays correct.
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	r2, _ := e.Begin(false)
	defer r2.Abort()
	if d, _ := r2.Degree(s, knows, Outgoing); d != degKnows-1 {
		t.Fatalf("degree(KNOWS) after checkpoint = %d, want %d", d, degKnows-1)
	}
	if d, _ := r2.Degree(s, likes, Outgoing); d != degLikes {
		t.Fatalf("degree(LIKES) after checkpoint = %d, want %d", d, degLikes)
	}
}

// TestMVCCSnapshotIsolation is the M1 MVCC gate: a snapshot taken before a writer
// commits does not see the writer's change; a snapshot taken after does; a write
// transaction reads its own uncommitted writes.
func TestMVCCSnapshotIsolation(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "mvcc.gr")
	defer e.Close()

	person, _ := e.Intern(catalog.KindLabel, "Person")
	name, _ := e.Intern(catalog.KindPropKey, "name")

	// Commit a node with name = "v1".
	w1, _ := e.Begin(true)
	a, _ := w1.CreateNode([]Token{person})
	w1.SetNodeProperty(a, name, value.String("v1"))
	w1.Commit()

	// Open a read snapshot now, before any further write.
	before, _ := e.Begin(false)

	// A separate writer changes name to "v2" and commits, while `before` is live.
	w2, _ := e.Begin(true)
	if v, _ := w2.NodeProperty(a, name); mustStr(t, v) != "v1" {
		t.Fatalf("writer pre-change read = %v, want v1", v)
	}
	w2.SetNodeProperty(a, name, value.String("v2"))
	// Read-your-writes: the writer sees its own uncommitted change.
	if v, _ := w2.NodeProperty(a, name); mustStr(t, v) != "v2" {
		t.Fatalf("read-your-writes = %v, want v2", v)
	}
	w2.Commit()

	// A snapshot taken after the commit sees v2.
	after, _ := e.Begin(false)

	// The pre-commit snapshot still sees v1 (snapshot stability); the post-commit
	// snapshot sees v2.
	if v, _ := before.NodeProperty(a, name); mustStr(t, v) != "v1" {
		t.Fatalf("pre-commit snapshot = %v, want v1", v)
	}
	if v, _ := after.NodeProperty(a, name); mustStr(t, v) != "v2" {
		t.Fatalf("post-commit snapshot = %v, want v2", v)
	}
	before.Abort()
	after.Abort()
}

// TestMVCCExistenceAndEdgeVisibility checks that node and edge existence are
// snapshot-scoped: a snapshot predating a create does not see the new node or
// edge, even via expand.
func TestMVCCExistenceAndEdgeVisibility(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "mvccvis.gr")
	defer e.Close()

	person, _ := e.Intern(catalog.KindLabel, "Person")
	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")

	w1, _ := e.Begin(true)
	a, _ := w1.CreateNode([]Token{person})
	b, _ := w1.CreateNode([]Token{person})
	w1.Commit()

	// Snapshot before the edge and before node c exist.
	before, _ := e.Begin(false)

	w2, _ := e.Begin(true)
	c, _ := w2.CreateNode([]Token{person})
	w2.CreateRel(a, b, knows)
	w2.Commit()

	after, _ := e.Begin(false)

	// New node invisible to the old snapshot, visible to the new one.
	if ok, _ := before.NodeExists(c); ok {
		t.Fatal("pre-create snapshot sees node c")
	}
	if ok, _ := after.NodeExists(c); !ok {
		t.Fatal("post-create snapshot misses node c")
	}

	// New edge invisible to the old snapshot's expand, visible to the new one.
	count := func(tx Tx) int {
		var n int
		tx.Expand(a, knows, Outgoing, func(Neighbor) error { n++; return nil })
		return n
	}
	if got := count(before); got != 0 {
		t.Fatalf("pre-create snapshot expand a = %d edges, want 0", got)
	}
	if got := count(after); got != 1 {
		t.Fatalf("post-create snapshot expand a = %d edges, want 1", got)
	}
	before.Abort()
	after.Abort()
}

// TestMVCCDeleteVisibility checks that a delete is snapshot-scoped: a snapshot
// predating the delete still sees the node, a later one does not.
func TestMVCCDeleteVisibility(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "mvccdel.gr")
	defer e.Close()
	person, _ := e.Intern(catalog.KindLabel, "Person")

	w1, _ := e.Begin(true)
	a, _ := w1.CreateNode([]Token{person})
	w1.Commit()

	before, _ := e.Begin(false)

	w2, _ := e.Begin(true)
	if err := w2.DeleteNode(a); err != nil {
		t.Fatal(err)
	}
	w2.Commit()

	after, _ := e.Begin(false)

	if ok, _ := before.NodeExists(a); !ok {
		t.Fatal("pre-delete snapshot should still see node a")
	}
	if ok, _ := after.NodeExists(a); ok {
		t.Fatal("post-delete snapshot should not see node a")
	}
	before.Abort()
	after.Abort()
}

// TestMVCCConcurrentReaderWriter runs a held read snapshot concurrently with a
// stream of writers and asserts the snapshot's view stays stable, exercising the
// overlay and base under the race detector.
func TestMVCCConcurrentReaderWriter(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "mvccrace.gr")
	defer e.Close()
	person, _ := e.Intern(catalog.KindLabel, "Person")
	name, _ := e.Intern(catalog.KindPropKey, "name")

	w, _ := e.Begin(true)
	a, _ := w.CreateNode([]Token{person})
	w.SetNodeProperty(a, name, value.String("frozen"))
	w.Commit()

	reader, _ := e.Begin(false)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			// The snapshot must always observe the value as of its begin.
			if v, _ := reader.NodeProperty(a, name); mustStr(t, v) != "frozen" {
				t.Errorf("snapshot drifted to %v", v)
				return
			}
		}
	}()

	for i := range 50 {
		wx, _ := e.Begin(true)
		_ = i
		wx.SetNodeProperty(a, name, value.String("moving"))
		wx.Commit()
	}
	wg.Wait()
	reader.Abort()
}

// TestMVCCVersionGC checks that the watermark bounds retention: pre-images held
// for a long reader are reclaimed once that reader releases and a checkpoint runs.
func TestMVCCVersionGC(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "mvccgc.gr")
	defer e.Close()
	person, _ := e.Intern(catalog.KindLabel, "Person")
	name, _ := e.Intern(catalog.KindPropKey, "name")

	w, _ := e.Begin(true)
	a, _ := w.CreateNode([]Token{person})
	w.SetNodeProperty(a, name, value.String("v0"))
	w.Commit()

	// A long reader pins the watermark.
	reader, _ := e.Begin(false)

	// Several writes accumulate pre-images the reader might need.
	for i := range 3 {
		wx, _ := e.Begin(true)
		_ = i
		wx.SetNodeProperty(a, name, value.String("v"))
		wx.Commit()
	}
	if e.ov.Len() == 0 {
		t.Fatal("expected retained pre-images while a long reader is live")
	}
	// A checkpoint with the reader still live cannot reclaim them.
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if e.ov.Len() == 0 {
		t.Fatal("pre-images dropped while reader still holds the watermark")
	}

	// Release the reader and checkpoint again: the watermark advances and the
	// overlay is reclaimed.
	reader.Abort()
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if e.ov.Len() != 0 {
		t.Fatalf("overlay should be empty after reader release + checkpoint, got %d", e.ov.Len())
	}
}
