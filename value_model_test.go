package gr

import (
	"errors"
	"reflect"
	"testing"

	"github.com/tamnd/gr/value"
)

// TestParamConversionRoundTrip converts a Go parameter map to internal values and
// back, checking each Go type maps to its Cypher type and reads back as the same Go
// value, including a nested list and map.
func TestParamConversionRoundTrip(t *testing.T) {
	in := Params{
		"i":    int(7),
		"i64":  int64(9),
		"f":    3.5,
		"s":    "Ada",
		"b":    true,
		"by":   []byte("xy"),
		"list": []any{int64(1), "two"},
		"map":  map[string]any{"k": int64(3)},
		"nul":  nil,
	}
	vals, err := toValues(in)
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	got := map[string]Value{}
	for k, v := range vals {
		got[k] = fromValue(v)
	}
	want := map[string]Value{
		"i":    int64(7),
		"i64":  int64(9),
		"f":    3.5,
		"s":    "Ada",
		"b":    true,
		"by":   []byte("xy"),
		"list": []Value{int64(1), "two"},
		"map":  map[string]Value{"k": int64(3)},
		"nul":  nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", got, want)
	}
}

// TestParamUnrepresentable confirms a Go value the model cannot map fails before
// execution with ErrParam, naming the offending parameter.
func TestParamUnrepresentable(t *testing.T) {
	_, err := toValues(Params{"ch": make(chan int)})
	if !errors.Is(err, ErrParam) {
		t.Fatalf("err = %v, want ErrParam", err)
	}
}

// TestRecordAccessors drives the streaming Next/Record idiom and the record's typed
// accessors over a small result, including a by-index read, the name-keyed map, and
// the absent-column ok=false case.
func TestRecordAccessors(t *testing.T) {
	db := openMem(t, "vm1.gr")
	if _, err := db.Exec("CREATE (:Person {name:'Ada', age:36})", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := db.Query("MATCH (p:Person) RETURN p.name AS name, p.age AS age", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = res.Close() }()

	if keys := res.Keys(); !reflect.DeepEqual(keys, []string{"name", "age"}) {
		t.Fatalf("keys = %v, want [name age]", keys)
	}
	if !res.Next() {
		t.Fatalf("expected a row, err=%v", res.Err())
	}
	rec := res.Record()

	name, err := rec.GetString("name")
	if err != nil || name != "Ada" {
		t.Fatalf("GetString name = %q, %v", name, err)
	}
	age, err := rec.GetInt("age")
	if err != nil || age != 36 {
		t.Fatalf("GetInt age = %d, %v", age, err)
	}
	if v := rec.GetByIndex(0); v != "Ada" {
		t.Fatalf("GetByIndex 0 = %v, want Ada", v)
	}
	if _, ok := rec.Get("missing"); ok {
		t.Fatal("Get of an absent column returned ok=true")
	}
	if _, err := rec.GetInt("name"); !errors.Is(err, ErrType) {
		t.Fatalf("GetInt of a string column err = %v, want ErrType", err)
	}
	if m := rec.AsMap(); m["name"] != "Ada" || m["age"] != int64(36) {
		t.Fatalf("AsMap = %v", m)
	}

	if res.Next() {
		t.Fatal("expected exactly one row")
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err after loop = %v, want nil", err)
	}
}

// TestSingle checks the one-row helper: a single-row result yields its record, an
// empty result and a multi-row result each return an error.
func TestSingle(t *testing.T) {
	db := openMem(t, "vm2.gr")
	if _, err := db.Exec("CREATE (:Person {name:'Ada'}), (:Person {name:'Lin'})", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	one, err := db.Query("MATCH (p:Person) RETURN count(p) AS c", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rec, err := Single(one)
	if err != nil {
		t.Fatalf("Single one row: %v", err)
	}
	if c, _ := rec.GetInt("c"); c != 2 {
		t.Fatalf("count = %d, want 2", c)
	}

	empty, err := db.Query("MATCH (p:Person {name:'Nobody'}) RETURN p.name AS n", nil)
	if err != nil {
		t.Fatalf("query empty: %v", err)
	}
	if _, err := Single(empty); err == nil {
		t.Fatal("Single of an empty result returned no error")
	}

	many, err := db.Query("MATCH (p:Person) RETURN p.name AS n", nil)
	if err != nil {
		t.Fatalf("query many: %v", err)
	}
	if _, err := Single(many); err == nil {
		t.Fatal("Single of a two-row result returned no error")
	}
}

// TestRecordBeforeFirstNext confirms Record is nil before the first Next, the
// streaming contract that guards against reading a record that does not exist yet.
func TestRecordBeforeFirstNext(t *testing.T) {
	db := openMem(t, "vm3.gr")
	res, err := db.Query("MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = res.Close() }()
	if res.Record() != nil {
		t.Fatal("Record before first Next was not nil")
	}
}

// TestFromValueGraphTypes confirms the graph value types map to their Go shapes.
func TestFromValueGraphTypes(t *testing.T) {
	if n, ok := fromValue(value.Node(5)).(Node); !ok || n.ID != 5 {
		t.Fatalf("node maps to %#v", fromValue(value.Node(5)))
	}
	if r, ok := fromValue(value.Rel(6)).(Relationship); !ok || r.ID != 6 {
		t.Fatalf("rel maps to %#v", fromValue(value.Rel(6)))
	}
	p, ok := fromValue(value.Path(value.Node(1), value.Rel(2), value.Node(3))).(Path)
	if !ok || len(p.Elements) != 3 {
		t.Fatalf("path maps to %#v", fromValue(value.Path(value.Node(1), value.Rel(2), value.Node(3))))
	}
}
