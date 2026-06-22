package gr

import (
	"testing"

	"github.com/tamnd/gr/vfs"
)

// TestDBInfo confirms db.Info reports the structural facts and the live element,
// catalog, and schema-object counts behind .info / gr info (doc 17 §6.15).
func TestDBInfo(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	stmts := []string{
		"CREATE CONSTRAINT u_name FOR (p:Person) REQUIRE p.name IS UNIQUE",
		"CREATE INDEX p_age FOR (p:Person) ON (p.age)",
		"CREATE (a:Person {name:'Ada', age:36})-[:KNOWS {since:2019}]->(b:Person {name:'Lin'}), (a)-[:LIKES]->(:Genre {name:'Jazz'})",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s, nil); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}

	info, err := db.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.FormatVersion != 1 {
		t.Errorf("FormatVersion = %d, want 1", info.FormatVersion)
	}
	if info.PageSize == 0 || info.PageCount == 0 {
		t.Errorf("geometry = page %d x %d, want non-zero", info.PageSize, info.PageCount)
	}
	if info.SizeBytes != int64(info.PageCount)*int64(info.PageSize) {
		t.Errorf("SizeBytes = %d, want pageCount*pageSize", info.SizeBytes)
	}
	if info.Labels != 2 || info.RelTypes != 2 || info.PropertyKeys != 3 {
		t.Errorf("catalog counts = %d labels, %d types, %d keys; want 2, 2, 3", info.Labels, info.RelTypes, info.PropertyKeys)
	}
	if info.Nodes != 3 || info.Relationships != 2 {
		t.Errorf("element counts = %d nodes, %d rels; want 3, 2", info.Nodes, info.Relationships)
	}
	if info.Indexes != 1 {
		t.Errorf("Indexes = %d, want 1", info.Indexes)
	}
	if info.Constraints != 1 || info.UniqueCons != 1 {
		t.Errorf("constraints = %d (%d unique), want 1 (1 unique)", info.Constraints, info.UniqueCons)
	}
}

// TestDBInfoLiveCounts confirms the element counts drop after a delete: they come
// from a snapshot count, not a record high-water mark.
func TestDBInfoLiveCounts(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (a {x:1})-[:R]->(b {x:2})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("MATCH (n) DETACH DELETE n", nil); err != nil {
		t.Fatal(err)
	}
	info, err := db.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Nodes != 0 || info.Relationships != 0 {
		t.Errorf("after delete: %d nodes, %d rels; want 0, 0", info.Nodes, info.Relationships)
	}
}

// TestDBInfoClosed confirms Info on a closed database reports ErrClosed.
func TestDBInfoClosed(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := db.Info(); err != ErrClosed {
		t.Errorf("Info after close = %v, want ErrClosed", err)
	}
}
