package parse

import (
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/value"
)

// TestParsePragmaQuery parses the query form PRAGMA name into a query-form command with no
// value.
func TestParsePragmaQuery(t *testing.T) {
	q, err := Parse("PRAGMA synchronous")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.First != nil {
		t.Fatalf("query form set First, want nil")
	}
	if q.Pragma == nil {
		t.Fatal("Pragma is nil")
	}
	if q.Pragma.Name != "synchronous" {
		t.Errorf("name = %q, want synchronous", q.Pragma.Name)
	}
	if q.Pragma.Set {
		t.Error("query form marked as Set")
	}
}

// TestParsePragmaNameLowered confirms the pragma name is folded to its canonical
// lower-case spelling, so PRAGMA SYNCHRONOUS and PRAGMA synchronous are the same knob (doc
// 24 §3.2).
func TestParsePragmaNameLowered(t *testing.T) {
	q, err := Parse("PRAGMA Page_Size")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Pragma.Name != "page_size" {
		t.Errorf("name = %q, want page_size", q.Pragma.Name)
	}
}

// TestParsePragmaSetBool parses the set form with a boolean keyword value.
func TestParsePragmaSetBool(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"PRAGMA lazy_properties = true", true},
		{"PRAGMA lazy_properties = on", true},
		{"PRAGMA lazy_properties = false", false},
		{"PRAGMA lazy_properties = off", false},
	} {
		q, err := Parse(tc.src)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.src, err)
		}
		if !q.Pragma.Set {
			t.Fatalf("%q: not marked Set", tc.src)
		}
		b, ok := q.Pragma.Value.AsBool()
		if !ok {
			t.Fatalf("%q: value is not a bool (%v)", tc.src, q.Pragma.Value.Type())
		}
		if b != tc.want {
			t.Errorf("%q: value = %v, want %v", tc.src, b, tc.want)
		}
	}
}

// TestParsePragmaSetInt parses the set form with an integer value, including a negative.
func TestParsePragmaSetInt(t *testing.T) {
	q, err := Parse("PRAGMA mem_budget = 1048576")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	n, ok := q.Pragma.Value.AsInt()
	if !ok || n != 1048576 {
		t.Errorf("value = %v ok=%v, want 1048576", q.Pragma.Value, ok)
	}
	q, err = Parse("PRAGMA mem_budget = -5")
	if err != nil {
		t.Fatalf("parse negative: %v", err)
	}
	if n, _ := q.Pragma.Value.AsInt(); n != -5 {
		t.Errorf("negative value = %v, want -5", n)
	}
}

// TestParsePragmaSetFloat parses the set form with a float value.
func TestParsePragmaSetFloat(t *testing.T) {
	q, err := Parse("PRAGMA replan_drift_factor = 2.5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Pragma.Value.Type() != value.TypeFloat {
		t.Fatalf("value type = %v, want float", q.Pragma.Value.Type())
	}
	if f, _ := q.Pragma.Value.AsFloat(); f != 2.5 {
		t.Errorf("value = %v, want 2.5", f)
	}
}

// TestParsePragmaSetEnumWord parses the set form with a bare enum word, kept as a string
// for the configuration subsystem to validate against the knob (doc 24 §24.4).
func TestParsePragmaSetEnumWord(t *testing.T) {
	q, err := Parse("PRAGMA synchronous = NORMAL")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, ok := q.Pragma.Value.AsString()
	if !ok || s != "NORMAL" {
		t.Errorf("value = %v ok=%v, want NORMAL", q.Pragma.Value, ok)
	}
}

// TestParsePragmaSetString parses the set form with a quoted string value.
func TestParsePragmaSetString(t *testing.T) {
	q, err := Parse("PRAGMA some_name = 'a value'")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s, _ := q.Pragma.Value.AsString(); s != "a value" {
		t.Errorf("value = %q, want 'a value'", s)
	}
}

// TestParsePragmaTemp records the TEMP modifier that forces a persistent knob's set to
// apply session-only (doc 24 §3.5).
func TestParsePragmaTemp(t *testing.T) {
	q, err := Parse("PRAGMA synchronous = NORMAL TEMP")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !q.Pragma.Temp {
		t.Error("Temp not set")
	}
	q, err = Parse("PRAGMA synchronous = NORMAL")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Pragma.Temp {
		t.Error("Temp set without the modifier")
	}
}

// TestParsePragmaWordStillIdentifier confirms PRAGMA and TEMP are soft keywords: a query
// can still use them as ordinary names.
func TestParsePragmaWordStillIdentifier(t *testing.T) {
	for _, src := range []string{
		"MATCH (pragma) RETURN pragma",
		"MATCH (n) RETURN n.temp AS pragma",
		"WITH 1 AS temp RETURN temp",
	} {
		if _, err := Parse(src); err != nil {
			t.Errorf("parse %q: %v", src, err)
		}
	}
}

// TestParsePragmaMissingName rejects a PRAGMA with no name.
func TestParsePragmaMissingName(t *testing.T) {
	if _, err := Parse("PRAGMA"); err == nil {
		t.Fatal("expected an error for PRAGMA with no name")
	}
}

// TestParsePragmaMissingValue rejects a set form with no value after the equals sign.
func TestParsePragmaMissingValue(t *testing.T) {
	if _, err := Parse("PRAGMA mem_budget ="); err == nil {
		t.Fatal("expected an error for a set form with no value")
	}
}

// TestParsePragmaExplainRejectedLater confirms EXPLAIN PRAGMA parses (the prefix attaches
// to whatever follows); the database layer rejects running it, not the parser.
func TestParsePragmaExplainParses(t *testing.T) {
	q, err := Parse("EXPLAIN PRAGMA page_count")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !q.Explain || q.Pragma == nil {
		t.Errorf("Explain=%v Pragma=%v, want both set", q.Explain, q.Pragma != nil)
	}
}

var _ = ast.PragmaCommand{}
