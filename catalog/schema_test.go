package catalog

import (
	"testing"

	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
)

// TestConstraintRoundTrip declares and drops constraints, commits, reopens, and
// checks the surviving set replayed identically from the one catalog Log.
func TestConstraintRoundTrip(t *testing.T) {
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
	// Intern the names the constraints reference so their tokens are real.
	person, _, _ := cat.Intern(KindLabel, "Person")
	movie, _, _ := cat.Intern(KindLabel, "Movie")
	email, _, _ := cat.Intern(KindPropKey, "email")
	title, _, _ := cat.Intern(KindPropKey, "title")

	if err := cat.AddConstraint(Constraint{Name: "person_email", Kind: UniqueNode, Label: person, Props: []uint32{email}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddConstraint(Constraint{Name: "movie_title", Kind: UniqueNode, Label: movie, Props: []uint32{title}}); err != nil {
		t.Fatal(err)
	}
	// A duplicate name is rejected.
	if err := cat.AddConstraint(Constraint{Name: "person_email", Label: person, Props: []uint32{email}}); err != ErrConstraintExists {
		t.Fatalf("re-add returned %v, want ErrConstraintExists", err)
	}
	// Drop one, then a missing one.
	if err := cat.DropConstraint("movie_title"); err != nil {
		t.Fatal(err)
	}
	if err := cat.DropConstraint("movie_title"); err != ErrNoSuchConstraint {
		t.Fatalf("re-drop returned %v, want ErrNoSuchConstraint", err)
	}
	if got := len(cat.Constraints()); got != 1 {
		t.Fatalf("live constraints = %d, want 1", got)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the one surviving constraint replays with its tokens intact.
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
	cons := cat2.Constraints()
	if len(cons) != 1 {
		t.Fatalf("after reopen: %d constraints, want 1", len(cons))
	}
	c := cons[0]
	if c.Name != "person_email" || c.Kind != UniqueNode || c.Label != person {
		t.Fatalf("replayed constraint = %+v", c)
	}
	if len(c.Props) != 1 || c.Props[0] != email {
		t.Fatalf("replayed props = %v", c.Props)
	}
	if _, ok := cat2.ConstraintByName("movie_title"); ok {
		t.Fatal("dropped constraint reappeared after reopen")
	}
}

// TestTypeConstraintRoundTrip confirms a property-type constraint carries its
// ValueType through the encode/decode round trip and replays it after a reopen.
func TestTypeConstraintRoundTrip(t *testing.T) {
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
	age, _, _ := cat.Intern(KindPropKey, "age")

	// ValueType 2 stands for the integer value type (value.TypeInt); the catalog
	// stores the tag opaquely and does not depend on the value package.
	if err := cat.AddConstraint(Constraint{Name: "person_age_int", Kind: TypedNode, Label: person, Props: []uint32{age}, ValueType: 2}); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

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
	c, ok := cat2.ConstraintByName("person_age_int")
	if !ok {
		t.Fatal("type constraint vanished after reopen")
	}
	if c.Kind != TypedNode || c.ValueType != 2 {
		t.Fatalf("replayed constraint = %+v, want TypedNode with ValueType 2", c)
	}
}

// TestSchemaOpsMonotonic confirms the schema-op counter only ever grows, so the
// engine can fold it into a monotonic catalog version even across drops.
func TestSchemaOpsMonotonic(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, _ := store.CreateSections(p)
	cat, err := Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}
	label, _, _ := cat.Intern(KindLabel, "L")
	prop, _, _ := cat.Intern(KindPropKey, "k")
	base := cat.SchemaOps()
	if err := cat.AddConstraint(Constraint{Name: "c", Label: label, Props: []uint32{prop}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.DropConstraint("c"); err != nil {
		t.Fatal(err)
	}
	if cat.SchemaOps() != base+2 {
		t.Fatalf("SchemaOps = %d, want %d", cat.SchemaOps(), base+2)
	}
}
