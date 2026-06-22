package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// boltDB opens a fresh in-memory database for the Bolt adapter tests.
func boltDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// runBolt runs one statement through a fresh adapter transaction, drains the
// cursor into rows, and commits, the way an auto-commit Bolt RUN does.
func runBolt(t *testing.T, h bolt.Handler, query string, params map[string]value.Value) ([][]value.Value, bolt.Summary, bolt.Tx) {
	t.Helper()
	tx, err := h.Begin(map[string]any{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cur, err := tx.Run(query, params)
	if err != nil {
		t.Fatalf("run %q: %v", query, err)
	}
	var rows [][]value.Value
	for {
		row, ok, err := cur.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		rows = append(rows, row)
	}
	summary := cur.Summary()
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return rows, summary, tx
}

// TestBoltAdapterWriteThenRead runs a write and a read through the adapter and
// confirms the write commits and the read sees it (doc 18 §5.6).
func TestBoltAdapterWriteThenRead(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()

	_, summary, tx := runBolt(t, h, "CREATE (n:Person {name: 'Ada', age: 36}) RETURN n", nil)
	if summary.Type != "w" {
		t.Errorf("create query type %q, want w", summary.Type)
	}
	if summary.Stats["nodes-created"] != int64(1) {
		t.Errorf("nodes-created %v, want 1", summary.Stats["nodes-created"])
	}
	bm, err := tx.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if bm == "" {
		t.Error("commit returned an empty bookmark")
	}

	rows, summary, tx := runBolt(t, h, "MATCH (n:Person) RETURN n.name AS name, n.age AS age", nil)
	if summary.Type != "r" {
		t.Errorf("match query type %q, want r", summary.Type)
	}
	if len(rows) != 1 {
		t.Fatalf("matched %d rows, want 1", len(rows))
	}
	name, _ := rows[0][0].AsString()
	age, _ := rows[0][1].AsInt()
	if name != "Ada" || age != 36 {
		t.Errorf("row = (%q, %d), want (Ada, 36)", name, age)
	}
	tx.Commit()
}

// TestBoltAdapterMaterializeNode confirms a returned node handle materializes to
// its labels, properties, and element id (doc 18 §6.10).
func TestBoltAdapterMaterializeNode(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, _, tx := runBolt(t, h, "CREATE (:City {name: 'Oslo'})", nil)
	tx.Commit()

	tx, err := h.Begin(map[string]any{"mode": "r"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	cur, err := tx.Run("MATCH (n:City) RETURN n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("next: ok=%v err=%v", ok, err)
	}
	if row[0].Type() != value.TypeNode {
		t.Fatalf("column type %s, want a node handle", row[0].Type())
	}
	id, _ := row[0].AsNode()
	node, err := tx.Materializer().MaterializeNode(id)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(node.Labels) != 1 || node.Labels[0] != "City" {
		t.Errorf("labels %v, want [City]", node.Labels)
	}
	if node.Props["name"] != "Oslo" {
		t.Errorf("props %v, want name=Oslo", node.Props)
	}
	if node.ElementID == "" {
		t.Error("node has no element id")
	}
	cur.Close()
}

// TestBoltAdapterMaterializeRel confirms a returned relationship handle
// materializes to its type, endpoints, and element ids (doc 18 §6.10).
func TestBoltAdapterMaterializeRel(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, _, tx := runBolt(t, h, "CREATE (a:P {n:'a'})-[:KNOWS {since: 2020}]->(b:P {n:'b'})", nil)
	tx.Commit()

	tx, err := h.Begin(map[string]any{"mode": "r"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	cur, err := tx.Run("MATCH ()-[r:KNOWS]->() RETURN r", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("next: ok=%v err=%v", ok, err)
	}
	if row[0].Type() != value.TypeRel {
		t.Fatalf("column type %s, want a relationship handle", row[0].Type())
	}
	id, _ := row[0].AsRel()
	rel, err := tx.Materializer().MaterializeRel(id)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if rel.Type != "KNOWS" {
		t.Errorf("type %q, want KNOWS", rel.Type)
	}
	if rel.Props["since"] != int64(2020) {
		t.Errorf("props %v, want since=2020", rel.Props)
	}
	if rel.StartElementID == "" || rel.EndElementID == "" || rel.StartElementID == rel.EndElementID {
		t.Errorf("endpoint element ids start=%q end=%q", rel.StartElementID, rel.EndElementID)
	}
	cur.Close()
}

// TestBoltAdapterParams confirms a parameter binds into a query (doc 18 §6.9).
func TestBoltAdapterParams(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, _, tx := runBolt(t, h, "CREATE (:N {v: 7})", nil)
	tx.Commit()

	rows, _, tx := runBolt(t, h, "MATCH (n:N) WHERE n.v = $want RETURN n.v",
		map[string]value.Value{"want": value.Int(7)})
	if len(rows) != 1 {
		t.Fatalf("matched %d rows with the parameter, want 1", len(rows))
	}
	got, _ := rows[0][0].AsInt()
	if got != 7 {
		t.Errorf("n.v = %d, want 7", got)
	}
	tx.Commit()
}

// TestBoltAdapterRollback confirms a rolled-back write is not visible (doc 18
// §5.10).
func TestBoltAdapterRollback(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	tx, err := h.Begin(map[string]any{"mode": "w"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cur, err := tx.Run("CREATE (:Temp)", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	cur.Close()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	rows, _, tx2 := runBolt(t, h, "MATCH (n:Temp) RETURN n", nil)
	if len(rows) != 0 {
		t.Errorf("rolled-back node is visible: %d rows", len(rows))
	}
	tx2.Commit()
}

// TestBoltAdapterSchema confirms a schema statement runs and auto-commits through
// the adapter, reporting an "s" summary type (doc 18 §5.6).
func TestBoltAdapterSchema(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, summary, tx := runBolt(t, h, "CREATE INDEX FOR (n:Person) ON (n.name)", nil)
	if summary.Type != "s" {
		t.Errorf("schema query type %q, want s", summary.Type)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	idx, err := db.Indexes()
	if err != nil {
		t.Fatalf("indexes: %v", err)
	}
	if len(idx) == 0 {
		t.Error("schema statement created no index")
	}
}

// TestBoltAdapterSyntaxError confirms a bad query surfaces as a client status
// error (doc 18 §12).
func TestBoltAdapterSyntaxError(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	tx, err := h.Begin(map[string]any{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	_, err = tx.Run("THIS IS NOT CYPHER", nil)
	if err == nil {
		t.Fatal("a bad query returned no error")
	}
	var se *bolt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a bolt.StatusError", err)
	}
	if se.Code == "" || se.Code[:3] != "Neo" {
		t.Errorf("status code %q, want a Neo client code", se.Code)
	}
}

// TestBoltAdapterAuth confirms authentication enforcement (doc 18 §10).
func TestBoltAdapterAuth(t *testing.T) {
	db := boltDB(t)
	if err := db.CreateUser("alice", "secret", RoleReader); err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := db.BoltHandler(WithBoltAuth())

	if err := h.Authenticate("basic", "alice", "secret"); err != nil {
		t.Errorf("valid credentials rejected: %v", err)
	}
	if err := h.Authenticate("basic", "alice", "wrong"); err == nil {
		t.Error("wrong password accepted")
	}
	if err := h.Authenticate("none", "", ""); err == nil {
		t.Error("none scheme accepted while auth is required")
	}

	// With auth off the same none scheme is accepted.
	if err := db.BoltHandler().Authenticate("none", "", ""); err != nil {
		t.Errorf("none scheme rejected while auth is off: %v", err)
	}
}
