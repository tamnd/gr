package gr

import (
	"sort"
	"testing"

	"github.com/tamnd/gr/vfs"
)

func TestSchemaListings(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (:Person {name: 'Ada', age: 36})-[:KNOWS {since: 2020}]->(:Person {name: 'Bob'})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE INDEX person_name FOR (p:Person) ON (p.name)", nil); err != nil {
		t.Fatal(err)
	}

	labels, err := db.Labels()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(labels, "Person") {
		t.Errorf("labels = %v, want Person", labels)
	}

	types, err := db.RelationshipTypes()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(types, "KNOWS") {
		t.Errorf("types = %v, want KNOWS", types)
	}

	keys, err := db.PropertyKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name", "age", "since"} {
		if !contains(keys, want) {
			t.Errorf("property keys = %v, want %q", keys, want)
		}
	}

	ixs, err := db.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(ixs) != 1 {
		t.Fatalf("indexes = %v, want one", ixs)
	}
	if ixs[0].Name != "person_name" || ixs[0].Label != "Person" {
		t.Errorf("index = %+v, want person_name on Person", ixs[0])
	}
	if len(ixs[0].Props) != 1 || ixs[0].Props[0] != "name" {
		t.Errorf("index props = %v, want [name]", ixs[0].Props)
	}
}

func TestSchemaListingsClosed(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := db.Labels(); err != ErrClosed {
		t.Errorf("Labels after close = %v, want ErrClosed", err)
	}
	if _, err := db.Indexes(); err != ErrClosed {
		t.Errorf("Indexes after close = %v, want ErrClosed", err)
	}
}

func contains(xs []string, want string) bool {
	sort.Strings(xs)
	i := sort.SearchStrings(xs, want)
	return i < len(xs) && xs[i] == want
}
