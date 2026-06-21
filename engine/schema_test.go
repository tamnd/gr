package engine

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// makePerson creates a Person node carrying email=addr through a write tx and
// commits it, returning whether the commit succeeded.
func makePerson(t *testing.T, e *DiskEngine, person, email Token, addr string) error {
	t.Helper()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.CreateNode([]Token{person})
	if err != nil {
		_ = tx.Abort()
		t.Fatal(err)
	}
	if err := tx.SetNodeProperty(id, email, value.String(addr)); err != nil {
		_ = tx.Abort()
		t.Fatal(err)
	}
	return tx.Commit()
}

func TestUniqueConstraintEnforcedAtCommit(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "uc.gr")
	defer e.Close()

	person, _ := e.Intern(catalog.KindLabel, "Person")
	email, _ := e.Intern(catalog.KindPropKey, "email")
	if _, err := e.CreateUniqueConstraint("", "Person", "email", false); err != nil {
		t.Fatal(err)
	}
	if err := makePerson(t, e, person, email, "a@x"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := makePerson(t, e, person, email, "a@x")
	var ce *ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("duplicate insert error = %v, want ConstraintError", err)
	}
	if err := makePerson(t, e, person, email, "b@x"); err != nil {
		t.Fatalf("distinct insert: %v", err)
	}
}

func TestCreateUniqueConstraintRejectsExistingDuplicate(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "ucexist.gr")
	defer e.Close()

	person, _ := e.Intern(catalog.KindLabel, "Person")
	email, _ := e.Intern(catalog.KindPropKey, "email")
	if err := makePerson(t, e, person, email, "a@x"); err != nil {
		t.Fatal(err)
	}
	if err := makePerson(t, e, person, email, "a@x"); err != nil {
		t.Fatal(err)
	}
	_, err := e.CreateUniqueConstraint("", "Person", "email", false)
	var ce *ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("create-over-duplicate error = %v, want ConstraintError", err)
	}
	if got := len(e.cat.Constraints()); got != 0 {
		t.Fatalf("a failed creation left %d constraints", got)
	}
}

func TestCreateAndDropConstraintIdempotency(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "ucidem.gr")
	defer e.Close()

	added, err := e.CreateUniqueConstraint("c", "Person", "email", false)
	if err != nil || !added {
		t.Fatalf("create = %v,%v", added, err)
	}
	if _, err := e.CreateUniqueConstraint("c", "Person", "email", false); !errors.Is(err, catalog.ErrConstraintExists) {
		t.Fatalf("plain re-create = %v, want ErrConstraintExists", err)
	}
	added, err = e.CreateUniqueConstraint("c", "Person", "email", true)
	if err != nil || added {
		t.Fatalf("IF NOT EXISTS re-create = %v,%v", added, err)
	}
	removed, err := e.DropConstraint("c", false)
	if err != nil || !removed {
		t.Fatalf("drop = %v,%v", removed, err)
	}
	if _, err := e.DropConstraint("c", false); !errors.Is(err, catalog.ErrNoSuchConstraint) {
		t.Fatalf("plain re-drop = %v, want ErrNoSuchConstraint", err)
	}
	removed, err = e.DropConstraint("c", true)
	if err != nil || removed {
		t.Fatalf("IF EXISTS re-drop = %v,%v", removed, err)
	}
}

func TestConstraintCatalogVersionMoves(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "ucver.gr")
	defer e.Close()

	v0 := e.CatalogVersion()
	if _, err := e.CreateUniqueConstraint("c", "Person", "email", false); err != nil {
		t.Fatal(err)
	}
	v1 := e.CatalogVersion()
	if v1 <= v0 {
		t.Fatalf("catalog version did not move on create: %d -> %d", v0, v1)
	}
	if _, err := e.DropConstraint("c", false); err != nil {
		t.Fatal(err)
	}
	if v2 := e.CatalogVersion(); v2 <= v1 {
		t.Fatalf("catalog version did not move on drop: %d -> %d", v1, v2)
	}
}
