package engine

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// seekIDs runs an index seek and collects the yielded node ids, asserting no
// error and returning whether an index served the seek.
func seekIDs(t *testing.T, tx Tx, label, key Token, v value.Value) ([]NodeID, bool) {
	t.Helper()
	var got []NodeID
	used, err := tx.IndexSeek(label, key, v, func(id NodeID) error {
		got = append(got, id)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, used
}

func hasID(ids []NodeID, want NodeID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// indexTokens declares a Person.email index and returns the SPI tokens to seek by.
func indexTokens(t *testing.T, e *DiskEngine) (label, key Token) {
	t.Helper()
	if _, err := e.CreateIndex("", "Person", "email", false); err != nil {
		t.Fatal(err)
	}
	label, _ = e.Lookup(catalog.KindLabel, "Person")
	key, _ = e.Lookup(catalog.KindPropKey, "email")
	return label, key
}

// TestIndexSeekBasic confirms a declared index serves an equality seek and that a
// seek on an undeclared (label, key) reports no index.
func TestIndexSeekBasic(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "idx.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{label})
	b, _ := tx.CreateNode([]Token{label})
	c, _ := tx.CreateNode([]Token{label})
	tx.SetNodeProperty(a, key, value.String("a@x"))
	tx.SetNodeProperty(b, key, value.String("b@x"))
	tx.SetNodeProperty(c, key, value.String("a@x"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()
	got, used := seekIDs(t, rx, label, key, value.String("a@x"))
	if !used {
		t.Fatal("seek on a declared index reported no index")
	}
	if len(got) != 2 || !hasID(got, a) || !hasID(got, c) {
		t.Fatalf("seek a@x = %v, want {%d, %d}", got, a, c)
	}
	got, _ = seekIDs(t, rx, label, key, value.String("b@x"))
	if len(got) != 1 || got[0] != b {
		t.Fatalf("seek b@x = %v, want {%d}", got, b)
	}
	// A value no node holds yields nothing.
	if got, _ := seekIDs(t, rx, label, key, value.String("none@x")); len(got) != 0 {
		t.Fatalf("seek none@x = %v, want empty", got)
	}
}

// TestIndexSeekNoIndex confirms a seek with no declared index reports used=false
// so the caller falls back to a scan.
func TestIndexSeekNoIndex(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "noidx.gr")
	defer e.Close()
	label, _ := e.Intern(catalog.KindLabel, "Person")
	key, _ := e.Intern(catalog.KindPropKey, "email")

	rx, _ := e.Begin(false)
	defer rx.Abort()
	if _, used := seekIDs(t, rx, label, key, value.String("x")); used {
		t.Fatal("seek without a declared index reported an index")
	}
}

// TestIndexSeekLabelScoped confirms the index yields only nodes carrying the
// indexed label, even when another label's node shares the property value.
func TestIndexSeekLabelScoped(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "scope.gr")
	defer e.Close()
	label, key := indexTokens(t, e)
	other, _ := e.Intern(catalog.KindLabel, "Company")

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{label})
	co, _ := tx.CreateNode([]Token{other})
	tx.SetNodeProperty(a, key, value.String("x"))
	tx.SetNodeProperty(co, key, value.String("x"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()
	got, _ := seekIDs(t, rx, label, key, value.String("x"))
	if len(got) != 1 || got[0] != a {
		t.Fatalf("seek = %v, want only the Person node %d", got, a)
	}
}

// TestIndexSeekNullMatchesNothing confirms a seek for null yields nothing, since
// indexes do not store nulls.
func TestIndexSeekNullMatchesNothing(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "null.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{label})
	tx.SetNodeProperty(a, key, value.String("x"))
	tx.Commit()

	rx, _ := e.Begin(false)
	defer rx.Abort()
	got, used := seekIDs(t, rx, label, key, value.Null)
	if !used {
		t.Fatal("null seek on a declared index reported no index")
	}
	if len(got) != 0 {
		t.Fatalf("null seek = %v, want empty", got)
	}
}

// TestIndexSeekReadsOwnWrites confirms a write transaction's index seek sees the
// nodes it created in that transaction, before commit, through its pending writes
// (the base index is rebuilt only at commit).
func TestIndexSeekReadsOwnWrites(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "ownwrites.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{label})
	tx.SetNodeProperty(a, key, value.String("x"))
	got, used := seekIDs(t, tx, label, key, value.String("x"))
	if !used {
		t.Fatal("write-tx seek reported no index")
	}
	if len(got) != 1 || got[0] != a {
		t.Fatalf("write-tx seek = %v, want own uncommitted node %d", got, a)
	}
	tx.Commit()
}

// TestIndexSeekSnapshotValueChange confirms a reader keeps seeing a node under the
// value it held at the reader's snapshot, even after a later committed write
// changes that value, and does not see it under the new value.
func TestIndexSeekSnapshotValueChange(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "snapchange.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	tx1, _ := e.Begin(true)
	a, _ := tx1.CreateNode([]Token{label})
	tx1.SetNodeProperty(a, key, value.String("old"))
	tx1.Commit()

	// A reader takes its snapshot, then a writer changes the value.
	rx, _ := e.Begin(false)
	defer rx.Abort()

	tx2, _ := e.Begin(true)
	tx2.SetNodeProperty(a, key, value.String("new"))
	tx2.Commit()

	// The reader still resolves the old value, and not the new one.
	if got, _ := seekIDs(t, rx, label, key, value.String("old")); len(got) != 1 || got[0] != a {
		t.Fatalf("reader seek old = %v, want {%d}", got, a)
	}
	if got, _ := seekIDs(t, rx, label, key, value.String("new")); len(got) != 0 {
		t.Fatalf("reader seek new = %v, want empty at the old snapshot", got)
	}

	// A reader taking its snapshot after the change sees the mirror image.
	rx2, _ := e.Begin(false)
	defer rx2.Abort()
	if got, _ := seekIDs(t, rx2, label, key, value.String("new")); len(got) != 1 || got[0] != a {
		t.Fatalf("fresh reader seek new = %v, want {%d}", got, a)
	}
	if got, _ := seekIDs(t, rx2, label, key, value.String("old")); len(got) != 0 {
		t.Fatalf("fresh reader seek old = %v, want empty", got)
	}
}

// TestIndexSeekSnapshotCreate confirms a reader does not see a node created after
// its snapshot, even though the latest base index lists it.
func TestIndexSeekSnapshotCreate(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "snapcreate.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	tx1, _ := e.Begin(true)
	a, _ := tx1.CreateNode([]Token{label})
	tx1.SetNodeProperty(a, key, value.String("x"))
	tx1.Commit()

	rx, _ := e.Begin(false)
	defer rx.Abort()

	tx2, _ := e.Begin(true)
	b, _ := tx2.CreateNode([]Token{label})
	tx2.SetNodeProperty(b, key, value.String("x"))
	tx2.Commit()

	got, _ := seekIDs(t, rx, label, key, value.String("x"))
	if len(got) != 1 || got[0] != a {
		t.Fatalf("reader seek = %v, want only the pre-snapshot node %d", got, a)
	}
}

// TestIndexSeekSnapshotDelete confirms a reader still sees a node deleted after
// its snapshot, even though the latest base index has dropped it.
func TestIndexSeekSnapshotDelete(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "snapdelete.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	tx1, _ := e.Begin(true)
	a, _ := tx1.CreateNode([]Token{label})
	b, _ := tx1.CreateNode([]Token{label})
	tx1.SetNodeProperty(a, key, value.String("x"))
	tx1.SetNodeProperty(b, key, value.String("x"))
	tx1.Commit()

	rx, _ := e.Begin(false)
	defer rx.Abort()

	tx2, _ := e.Begin(true)
	if err := tx2.DeleteNode(a); err != nil {
		t.Fatal(err)
	}
	tx2.Commit()

	got, _ := seekIDs(t, rx, label, key, value.String("x"))
	if len(got) != 2 || !hasID(got, a) || !hasID(got, b) {
		t.Fatalf("reader seek = %v, want both pre-snapshot nodes %d and %d", got, a, b)
	}

	// A fresh reader sees only the survivor.
	rx2, _ := e.Begin(false)
	defer rx2.Abort()
	if got, _ := seekIDs(t, rx2, label, key, value.String("x")); len(got) != 1 || got[0] != b {
		t.Fatalf("fresh reader seek = %v, want only %d", got, b)
	}
}

// TestIndexMaintainedAcrossLabelChange confirms adding and removing the indexed
// label moves a node in and out of the index.
func TestIndexMaintainedAcrossLabelChange(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "labelchange.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	// A node with the value but not the label is not indexed.
	tx1, _ := e.Begin(true)
	a, _ := tx1.CreateNode(nil)
	tx1.SetNodeProperty(a, key, value.String("x"))
	tx1.Commit()

	rx, _ := e.Begin(false)
	if got, _ := seekIDs(t, rx, label, key, value.String("x")); len(got) != 0 {
		t.Fatalf("seek before label = %v, want empty", got)
	}
	rx.Abort()

	// Add the label: now it is indexed.
	tx2, _ := e.Begin(true)
	tx2.AddLabel(a, label)
	tx2.Commit()

	rx2, _ := e.Begin(false)
	if got, _ := seekIDs(t, rx2, label, key, value.String("x")); len(got) != 1 || got[0] != a {
		t.Fatalf("seek after add label = %v, want {%d}", got, a)
	}
	rx2.Abort()

	// Remove the label: it leaves the index.
	tx3, _ := e.Begin(true)
	tx3.RemoveLabel(a, label)
	tx3.Commit()

	rx3, _ := e.Begin(false)
	defer rx3.Abort()
	if got, _ := seekIDs(t, rx3, label, key, value.String("x")); len(got) != 0 {
		t.Fatalf("seek after remove label = %v, want empty", got)
	}
}

// TestIndexPersistsAcrossReopen confirms a declared index is rebuilt from the base
// on reopen and serves seeks again.
func TestIndexPersistsAcrossReopen(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "persist.gr")
	label, key := indexTokens(t, e)

	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{label})
	tx.SetNodeProperty(a, key, value.String("x"))
	tx.Commit()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openDisk(t, fsys, "persist.gr")
	defer e2.Close()
	label2, _ := e2.Lookup(catalog.KindLabel, "Person")
	key2, _ := e2.Lookup(catalog.KindPropKey, "email")
	rx, _ := e2.Begin(false)
	defer rx.Abort()
	got, used := seekIDs(t, rx, label2, key2, value.String("x"))
	if !used {
		t.Fatal("index did not survive reopen")
	}
	if len(got) != 1 {
		t.Fatalf("seek after reopen = %v, want one node", got)
	}
}

// TestCreateIndexOverExistingData confirms an index created over a populated graph
// is built from the live data: the seek serves nodes that existed before the index
// was declared, not just ones written afterward. CreateIndex rebuilds the index
// from the live base, so the pre-existing rows are picked up.
func TestCreateIndexOverExistingData(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "buildover.gr")
	defer e.Close()

	// Populate Person nodes with no index declared yet.
	label, _ := e.Intern(catalog.KindLabel, "Person")
	key, _ := e.Intern(catalog.KindPropKey, "email")
	tx, _ := e.Begin(true)
	a, _ := tx.CreateNode([]Token{label})
	b, _ := tx.CreateNode([]Token{label})
	c, _ := tx.CreateNode([]Token{label})
	tx.SetNodeProperty(a, key, value.String("a@x"))
	tx.SetNodeProperty(b, key, value.String("b@x"))
	tx.SetNodeProperty(c, key, value.String("a@x"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// No index served the seek before the index existed.
	rx0, _ := e.Begin(false)
	if _, used := seekIDs(t, rx0, label, key, value.String("a@x")); used {
		t.Fatal("a seek reported an index before one was declared")
	}
	rx0.Abort()

	// Declare the index over the now-populated graph.
	if _, err := e.CreateIndex("", "Person", "email", false); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()
	got, used := seekIDs(t, rx, label, key, value.String("a@x"))
	if !used {
		t.Fatal("seek reported no index after CreateIndex")
	}
	if len(got) != 2 || !hasID(got, a) || !hasID(got, c) {
		t.Fatalf("seek over pre-existing data = %v, want the two a@x nodes %d and %d", got, a, c)
	}
	// A later write is maintained alongside the back-filled rows.
	wx, _ := e.Begin(true)
	d, _ := wx.CreateNode([]Token{label})
	wx.SetNodeProperty(d, key, value.String("a@x"))
	if err := wx.Commit(); err != nil {
		t.Fatal(err)
	}
	rx2, _ := e.Begin(false)
	defer rx2.Abort()
	if got, _ := seekIDs(t, rx2, label, key, value.String("a@x")); len(got) != 3 {
		t.Fatalf("seek after a later write = %v, want three nodes", got)
	}
}

// TestIndexDropStopsServing confirms a dropped index no longer serves seeks, so
// the caller falls back to a scan.
func TestIndexDropStopsServing(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "drop.gr")
	defer e.Close()
	label, key := indexTokens(t, e)

	if _, err := e.DropIndex(autoIndexName("Person", "email"), false); err != nil {
		t.Fatal(err)
	}
	rx, _ := e.Begin(false)
	defer rx.Abort()
	if _, used := seekIDs(t, rx, label, key, value.String("x")); used {
		t.Fatal("dropped index still served the seek")
	}
}

// TestIndexCreateIfNotExists confirms a duplicate create is an error without the
// guard and a no-op with it.
func TestIndexCreateIfNotExists(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "ifnotexists.gr")
	defer e.Close()

	if added, err := e.CreateIndex("ix", "Person", "email", false); err != nil || !added {
		t.Fatalf("first create: added=%v err=%v", added, err)
	}
	if _, err := e.CreateIndex("ix", "Person", "email", false); err != catalog.ErrIndexExists {
		t.Fatalf("plain re-create returned %v, want ErrIndexExists", err)
	}
	if added, err := e.CreateIndex("ix", "Person", "email", true); err != nil || added {
		t.Fatalf("guarded re-create: added=%v err=%v, want added=false err=nil", added, err)
	}
}
