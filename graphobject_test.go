package gr

import (
	"context"
	"testing"
)

// createGraph seeds a small two-person KNOWS graph for the graph-object tests and
// returns the database.
func createGraph(t *testing.T) *DB {
	t.Helper()
	db := openMem(t, "graphobj.gr")
	if _, err := db.Run(context.Background(),
		"CREATE (:Person {name: 'Ada', age: 36})-[:KNOWS {since: 2019}]->(:Person {name: 'Lin'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// firstNode runs a single-node query and returns the node from its one row.
func firstNode(t *testing.T, db *DB, cypher string, opts ...RunOption) (Node, *Result) {
	t.Helper()
	res, err := db.Run(context.Background(), cypher, nil, opts...)
	if err != nil {
		t.Fatalf("run %q: %v", cypher, err)
	}
	if !res.Next() {
		_ = res.Close()
		t.Fatalf("run %q: no row", cypher)
	}
	n, err := res.Record().GetNode("n")
	if err != nil {
		_ = res.Close()
		t.Fatalf("get node: %v", err)
	}
	return n, res
}

// TestNodeAccessors confirms a returned node exposes its labels and properties
// through the accessors, with eager materialization (the default) so the values
// survive the result's close (doc 16 §10.2, §10.6).
func TestNodeAccessors(t *testing.T) {
	db := createGraph(t)
	n, res := firstNode(t, db, "MATCH (n:Person {name: 'Ada'}) RETURN n")
	// Close the result up front: eager properties must outlive it.
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if n.ElementId() == "" {
		t.Error("empty element id")
	}
	if labels := n.Labels(); len(labels) != 1 || labels[0] != "Person" {
		t.Errorf("labels = %v, want [Person]", labels)
	}
	if !n.HasLabel("Person") || n.HasLabel("City") {
		t.Error("HasLabel wrong")
	}
	name, ok := n.Get("name")
	if !ok || name != "Ada" {
		t.Errorf("Get(name) = %v, %v", name, ok)
	}
	age, ok := n.Get("age")
	if !ok || age != int64(36) {
		t.Errorf("Get(age) = %v, %v", age, ok)
	}
	if _, ok := n.Get("missing"); ok {
		t.Error("Get(missing) reported present")
	}
	props := n.Props()
	if props["name"] != "Ada" || props["age"] != int64(36) {
		t.Errorf("Props = %v", props)
	}
	if keys := n.Keys(); len(keys) != 2 || keys[0] != "age" || keys[1] != "name" {
		t.Errorf("Keys = %v, want [age name]", keys)
	}
}

// TestRelationshipAccessors confirms a returned relationship exposes its type,
// endpoints, and properties (doc 16 §10.3).
func TestRelationshipAccessors(t *testing.T) {
	db := createGraph(t)
	res, err := db.Run(context.Background(), "MATCH ()-[r:KNOWS]->() RETURN r", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Next() {
		t.Fatal("no row")
	}
	r, err := res.Record().GetRelationship("r")
	if err != nil {
		t.Fatalf("get rel: %v", err)
	}
	_ = res.Close()

	if r.Type() != "KNOWS" {
		t.Errorf("Type = %q", r.Type())
	}
	if r.StartElementId() == "" || r.EndElementId() == "" {
		t.Errorf("endpoints = %q / %q", r.StartElementId(), r.EndElementId())
	}
	if r.StartElementId() == r.EndElementId() {
		t.Error("start and end ids equal for a non-loop")
	}
	since, ok := r.Get("since")
	if !ok || since != int64(2019) {
		t.Errorf("Get(since) = %v, %v", since, ok)
	}
}

// TestEntityInterface confirms both a node and a relationship satisfy Entity, so
// generic code reads either through one interface (doc 16 §10.5).
func TestEntityInterface(t *testing.T) {
	db := createGraph(t)
	res, err := db.Run(context.Background(), "MATCH (a:Person {name:'Ada'})-[r:KNOWS]->(b) RETURN a, r", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Next() {
		t.Fatal("no row")
	}
	n, _ := res.Record().GetNode("a")
	r, _ := res.Record().GetRelationship("r")
	_ = res.Close()

	entities := []Entity{n, r}
	for _, e := range entities {
		if e.ElementId() == "" {
			t.Errorf("entity %T has empty element id", e)
		}
		if e.Props() == nil {
			t.Errorf("entity %T has nil props", e)
		}
	}
}

// TestPathAccessors confirms a path returned from a variable-length pattern carries
// its nodes and relationships in order (doc 16 §10.4).
func TestPathAccessors(t *testing.T) {
	db := createGraph(t)
	res, err := db.Run(context.Background(), "MATCH p = (:Person {name:'Ada'})-[:KNOWS]->(:Person) RETURN p", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Next() {
		t.Fatal("no row")
	}
	p, err := res.Record().GetPath("p")
	if err != nil {
		t.Fatalf("get path: %v", err)
	}
	_ = res.Close()

	if p.Length() != 1 {
		t.Errorf("Length = %d, want 1", p.Length())
	}
	if len(p.Nodes()) != 2 {
		t.Errorf("Nodes = %d, want 2", len(p.Nodes()))
	}
	if len(p.Relationships()) != 1 {
		t.Errorf("Relationships = %d, want 1", len(p.Relationships()))
	}
	start, ok := p.Start().Get("name")
	if !ok || start != "Ada" {
		t.Errorf("Start name = %v", start)
	}
	end, ok := p.End().Get("name")
	if !ok || end != "Lin" {
		t.Errorf("End name = %v", end)
	}
}

// TestLazyProperties confirms lazy materialization defers property reads to the
// transaction's snapshot: inside the transaction the reads succeed (doc 16 §10.6).
func TestLazyProperties(t *testing.T) {
	db := createGraph(t)
	err := db.View(func(tx *Tx) error {
		res, err := tx.Run(context.Background(), "MATCH (n:Person {name:'Ada'}) RETURN n", nil, WithLazyProperties(true))
		if err != nil {
			return err
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatal("no row")
		}
		n, err := res.Record().GetNode("n")
		if err != nil {
			return err
		}
		// Properties not loaded eagerly under lazy mode; first Get fetches them.
		name, ok := n.Get("name")
		if !ok || name != "Ada" {
			t.Errorf("lazy Get(name) = %v, %v", name, ok)
		}
		if labels := n.Labels(); len(labels) != 1 || labels[0] != "Person" {
			t.Errorf("lazy labels = %v", labels)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestNodeByElementId confirms a node's element id round-trips back to the node
// through the transaction, and that a bad or absent id is ErrNotFound (doc 16 §10.7).
func TestNodeByElementId(t *testing.T) {
	db := createGraph(t)
	var id string
	err := db.View(func(tx *Tx) error {
		res, err := tx.Run(context.Background(), "MATCH (n:Person {name:'Ada'}) RETURN n", nil)
		if err != nil {
			return err
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatal("no row")
		}
		n, err := res.Record().GetNode("n")
		if err != nil {
			return err
		}
		id = n.ElementId()

		got, err := tx.NodeByElementId(id)
		if err != nil {
			t.Fatalf("NodeByElementId: %v", err)
		}
		if !got.Equal(n) {
			t.Errorf("round-trip node %q != %q", got.ElementId(), n.ElementId())
		}
		name, ok := got.Get("name")
		if !ok || name != "Ada" {
			t.Errorf("fetched node name = %v", name)
		}

		if _, err := tx.NodeByElementId("garbage"); err != ErrNotFound {
			t.Errorf("bad id error = %v, want ErrNotFound", err)
		}
		// A relationship id is not a node id, so it must not resolve as a node.
		if _, err := tx.NodeByElementId("r0"); err != ErrNotFound {
			t.Errorf("rel id as node error = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestRelationshipByElementId confirms the relationship round-trip and its kind
// guard (doc 16 §10.7).
func TestRelationshipByElementId(t *testing.T) {
	db := createGraph(t)
	err := db.View(func(tx *Tx) error {
		res, err := tx.Run(context.Background(), "MATCH ()-[r:KNOWS]->() RETURN r", nil)
		if err != nil {
			return err
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatal("no row")
		}
		r, err := res.Record().GetRelationship("r")
		if err != nil {
			return err
		}
		got, err := tx.RelationshipByElementId(r.ElementId())
		if err != nil {
			t.Fatalf("RelationshipByElementId: %v", err)
		}
		if !got.Equal(r) || got.Type() != "KNOWS" {
			t.Errorf("round-trip rel = %q/%s", got.ElementId(), got.Type())
		}
		// A node id is not a relationship id.
		if _, err := tx.RelationshipByElementId("n0"); err != ErrNotFound {
			t.Errorf("node id as rel error = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestWriteReturnMaterializes confirms a node returned from an auto-commit write is
// fully materialized: runAutoWrite commits before returning, so the result must open
// a fresh read snapshot to resolve the returned node's labels and properties (doc 16
// §10.6). The values must also survive the result's close, since eager properties
// outlive the snapshot.
func TestWriteReturnMaterializes(t *testing.T) {
	db := openMem(t, "writereturn.gr")
	res, err := db.Run(context.Background(),
		"CREATE (n:City {name: 'Hue', founded: 1802}) RETURN n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Next() {
		t.Fatal("no row")
	}
	n, err := res.Record().GetNode("n")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if labels := n.Labels(); len(labels) != 1 || labels[0] != "City" {
		t.Errorf("labels = %v", labels)
	}
	name, ok := n.Get("name")
	if !ok || name != "Hue" {
		t.Errorf("Get(name) = %v, %v", name, ok)
	}
	if founded, ok := n.Get("founded"); !ok || founded != int64(1802) {
		t.Errorf("Get(founded) = %v, %v", founded, ok)
	}
}

// TestElementIdKindsDistinct confirms a node id and a relationship id never collide,
// since the element id encodes the kind (doc 02 §5.2).
func TestElementIdKindsDistinct(t *testing.T) {
	if encodeElementID(elemNode, 5) == encodeElementID(elemRel, 5) {
		t.Error("node and relationship element ids collided")
	}
	kind, raw, err := decodeElementID(encodeElementID(elemRel, 42))
	if err != nil || kind != elemRel || raw != 42 {
		t.Errorf("decode round-trip = %c/%d/%v", kind, raw, err)
	}
	if _, _, err := decodeElementID("x9"); err != ErrNotFound {
		t.Errorf("bad kind decode = %v, want ErrNotFound", err)
	}
}
