package catalog

import (
	"testing"

	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
)

// TestIndexRoundTrip declares and drops indexes, commits, reopens, and checks the
// surviving set replayed identically from the one catalog Log, alongside the
// constraints that share the Log.
func TestIndexRoundTrip(t *testing.T) {
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
	person, _, _ := cat.Intern(KindLabel, "Person")
	movie, _, _ := cat.Intern(KindLabel, "Movie")
	email, _, _ := cat.Intern(KindPropKey, "email")
	title, _, _ := cat.Intern(KindPropKey, "title")

	if err := cat.AddIndex(Index{Name: "person_email", Label: person, Props: []uint32{email}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddIndex(Index{Name: "movie_title", Label: movie, Props: []uint32{title}}); err != nil {
		t.Fatal(err)
	}
	// A duplicate name is rejected.
	if err := cat.AddIndex(Index{Name: "person_email", Label: person, Props: []uint32{email}}); err != ErrIndexExists {
		t.Fatalf("re-add returned %v, want ErrIndexExists", err)
	}
	// Drop one, then a missing one.
	if err := cat.DropIndex("movie_title"); err != nil {
		t.Fatal(err)
	}
	if err := cat.DropIndex("movie_title"); err != ErrNoSuchIndex {
		t.Fatalf("re-drop returned %v, want ErrNoSuchIndex", err)
	}
	if got := len(cat.Indexes()); got != 1 {
		t.Fatalf("live indexes = %d, want 1", got)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the one surviving index replays with its tokens intact.
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
	ixs := cat2.Indexes()
	if len(ixs) != 1 {
		t.Fatalf("after reopen: %d indexes, want 1", len(ixs))
	}
	ix := ixs[0]
	if ix.Name != "person_email" || ix.Label != person {
		t.Fatalf("replayed index = %+v", ix)
	}
	if len(ix.Props) != 1 || ix.Props[0] != email {
		t.Fatalf("replayed props = %v", ix.Props)
	}
	if _, ok := cat2.IndexByName("movie_title"); ok {
		t.Fatal("dropped index reappeared after reopen")
	}
}

// TestIndexAndConstraintCoexist confirms an index and a constraint of the same
// name can both live in the catalog: they are separate namespaces in separate
// maps, sharing only the append-only Log.
func TestIndexAndConstraintCoexist(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()
	secs, _ := store.CreateSections(p)
	cat, err := Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}
	label, _, _ := cat.Intern(KindLabel, "Person")
	prop, _, _ := cat.Intern(KindPropKey, "email")
	if err := cat.AddConstraint(Constraint{Name: "person_email", Kind: UniqueNode, Label: label, Props: []uint32{prop}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddIndex(Index{Name: "person_email", Label: label, Props: []uint32{prop}}); err != nil {
		t.Fatalf("index with a constraint's name was rejected: %v", err)
	}
	if _, ok := cat.ConstraintByName("person_email"); !ok {
		t.Fatal("constraint disappeared")
	}
	if _, ok := cat.IndexByName("person_email"); !ok {
		t.Fatal("index disappeared")
	}
}

// TestIndexSchemaOpsMonotonic confirms an index add and drop each move the schema
// counter, so an index change invalidates cached plans just as a constraint does.
func TestIndexSchemaOpsMonotonic(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()
	secs, _ := store.CreateSections(p)
	cat, err := Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}
	label, _, _ := cat.Intern(KindLabel, "L")
	prop, _, _ := cat.Intern(KindPropKey, "k")
	base := cat.SchemaOps()
	if err := cat.AddIndex(Index{Name: "i", Label: label, Props: []uint32{prop}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.DropIndex("i"); err != nil {
		t.Fatal(err)
	}
	if cat.SchemaOps() != base+2 {
		t.Fatalf("SchemaOps = %d, want %d", cat.SchemaOps(), base+2)
	}
}
