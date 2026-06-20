package engine

import (
	"testing"

	"github.com/tamnd/gr/value"
)

func TestStubNodeAndRelLifecycle(t *testing.T) {
	e := NewMemEngine()
	defer e.Close()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}

	const labelPerson Token = 1
	const propName Token = 2
	const typeKnows Token = 3

	a, err := tx.CreateNode([]Token{labelPerson})
	if err != nil {
		t.Fatal(err)
	}
	b, err := tx.CreateNode([]Token{labelPerson})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.SetNodeProperty(a, propName, value.String("alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.CreateRel(a, b, typeKnows); err != nil {
		t.Fatal(err)
	}

	// Reads under the same tx.
	if ok, _ := tx.HasLabel(a, labelPerson); !ok {
		t.Fatal("a should have Person label")
	}
	v, _ := tx.NodeProperty(a, propName)
	if s, ok := v.AsString(); !ok || s != "alice" {
		t.Fatalf("name = %v", v)
	}
	deg, _ := tx.Degree(a, typeKnows, Outgoing)
	if deg != 1 {
		t.Fatalf("out-degree = %d", deg)
	}

	var reached NodeID
	tx.Expand(a, typeKnows, Outgoing, func(n Neighbor) error {
		reached = n.Node
		return nil
	})
	if reached != b {
		t.Fatalf("expand reached %d, want %d", reached, b)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStubReadOnlyRejectsWrites(t *testing.T) {
	e := NewMemEngine()
	defer e.Close()
	tx, _ := e.Begin(false)
	if _, err := tx.CreateNode(nil); err != ErrReadOnlyTx {
		t.Fatalf("want ErrReadOnlyTx, got %v", err)
	}
	_ = tx.Abort()
}

func TestStubScanLabel(t *testing.T) {
	e := NewMemEngine()
	defer e.Close()
	tx, _ := e.Begin(true)
	const l Token = 1
	tx.CreateNode([]Token{l})
	tx.CreateNode([]Token{l})
	tx.CreateNode(nil)
	var count int
	tx.ScanLabel(l, func(NodeID) error { count++; return nil })
	if count != 2 {
		t.Fatalf("label scan found %d, want 2", count)
	}
	var all int
	tx.ScanLabel(0, func(NodeID) error { all++; return nil })
	if all != 3 {
		t.Fatalf("scan-all found %d, want 3", all)
	}
	tx.Commit()
}
