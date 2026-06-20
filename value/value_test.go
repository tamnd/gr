package value

import (
	"math"
	"testing"
)

func TestTypeAndAccessors(t *testing.T) {
	if !Null.IsNull() || Null.Type() != TypeNull {
		t.Fatal("null")
	}
	if b, ok := Bool(true).AsBool(); !ok || !b {
		t.Fatal("bool")
	}
	if i, ok := Int(42).AsInt(); !ok || i != 42 {
		t.Fatal("int")
	}
	if f, ok := Float(1.5).AsFloat(); !ok || f != 1.5 {
		t.Fatal("float")
	}
	if s, ok := String("x").AsString(); !ok || s != "x" {
		t.Fatal("string")
	}
}

func TestIntWidensToFloat(t *testing.T) {
	if got, ok := Int(7).AsFloat(); !ok || got != 7.0 {
		t.Fatalf("int->float widening: got %v ok=%v", got, ok)
	}
}

func TestEqual(t *testing.T) {
	if !Int(1).Equal(Int(1)) {
		t.Fatal("int equal")
	}
	if Int(1).Equal(Int(2)) {
		t.Fatal("int unequal")
	}
	if !List(Int(1), String("a")).Equal(List(Int(1), String("a"))) {
		t.Fatal("list equal")
	}
	if !Map(map[string]Value{"k": Int(1)}).Equal(Map(map[string]Value{"k": Int(1)})) {
		t.Fatal("map equal")
	}
	// NaN == NaN is true under structural equality (doc value semantics).
	if !Float(math.NaN()).Equal(Float(math.NaN())) {
		t.Fatal("NaN structural equal")
	}
}

func TestStringDeterministic(t *testing.T) {
	m := Map(map[string]Value{"b": Int(2), "a": Int(1)})
	// Map keys are sorted, so the rendering is stable across runs.
	if got := m.String(); got != `{"a": 1, "b": 2}` {
		t.Fatalf("map string = %q", got)
	}
}
