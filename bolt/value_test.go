package bolt

import (
	"reflect"
	"testing"

	"github.com/tamnd/gr/pack"
	"github.com/tamnd/gr/value"
)

// fakeMat is a Materializer backed by in-memory maps, standing in for the engine
// in the codec tests.
type fakeMat struct {
	nodes map[uint64]Node
	rels  map[uint64]Rel
}

func (m fakeMat) MaterializeNode(id uint64) (Node, error) { return m.nodes[id], nil }
func (m fakeMat) MaterializeRel(id uint64) (Rel, error)   { return m.rels[id], nil }

// TestEncodeScalars confirms the scalar/list/map kinds convert to the codec's
// plain types (doc 18 §6.5, §6.10).
func TestEncodeScalars(t *testing.T) {
	cases := []struct {
		name string
		in   value.Value
		want any
	}{
		{"null", value.Null, nil},
		{"bool", value.Bool(true), true},
		{"int", value.Int(42), int64(42)},
		{"float", value.Float(1.5), 1.5},
		{"string", value.String("hi"), "hi"},
		{"bytes", value.Bytes([]byte{1, 2}), []byte{1, 2}},
		{"list", value.List(value.Int(1), value.String("a")), []any{int64(1), "a"}},
		{"map", value.Map(map[string]value.Value{"x": value.Int(1)}), map[string]any{"x": int64(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeValue(tc.in, Version{5, 4}, fakeMat{})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("encoded %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildNodeVersionArity confirms a node serializes with 4 fields (element id)
// on 5.0+ and 3 fields on 4.x (doc 18 §6.1, §6.11).
func TestBuildNodeVersionArity(t *testing.T) {
	n := Node{ID: 1, Labels: []string{"Person"}, Props: map[string]any{"name": "Ada"}, ElementID: "4:db:1"}
	s5 := BuildNode(n, Version{5, 0})
	if s5.Tag != SigNode || len(s5.Fields) != 4 || s5.Fields[3] != "4:db:1" {
		t.Errorf("5.0 node %#v", s5)
	}
	if labels := s5.Fields[1].([]any); len(labels) != 1 || labels[0] != "Person" {
		t.Errorf("labels %#v", s5.Fields[1])
	}
	s4 := BuildNode(n, Version{4, 4})
	if len(s4.Fields) != 3 {
		t.Errorf("4.4 node should have 3 fields, got %d", len(s4.Fields))
	}
}

// TestBuildRelVersionArity confirms a relationship serializes with 8 fields on
// 5.0+ and 5 on 4.x (doc 18 §6.2, §6.11).
func TestBuildRelVersionArity(t *testing.T) {
	r := Rel{ID: 7, StartID: 1, EndID: 2, Type: "KNOWS", ElementID: "5:db:7", StartElementID: "4:db:1", EndElementID: "4:db:2"}
	if s := BuildRel(r, Version{5, 4}); len(s.Fields) != 8 || s.Fields[3] != "KNOWS" || s.Fields[5] != "5:db:7" {
		t.Errorf("5.4 rel %#v", s)
	}
	if s := BuildRel(r, Version{4, 4}); len(s.Fields) != 5 {
		t.Errorf("4.4 rel should have 5 fields, got %d", len(s.Fields))
	}
}

// TestBuildUnboundRelArity confirms an unbound relationship serializes with 4
// fields on 5.0+ and 3 on 4.x (doc 18 §6.3).
func TestBuildUnboundRelArity(t *testing.T) {
	r := Rel{ID: 7, Type: "KNOWS", ElementID: "5:db:7"}
	if s := BuildUnboundRel(r, Version{5, 4}); s.Tag != SigUnboundRel || len(s.Fields) != 4 {
		t.Errorf("5.4 unbound rel %#v", s)
	}
	if s := BuildUnboundRel(r, Version{4, 4}); len(s.Fields) != 3 {
		t.Errorf("4.4 unbound rel should have 3 fields, got %d", len(s.Fields))
	}
}

// TestEncodeNodeThroughValue confirms a node handle in a result is materialized
// and built (doc 18 §6.10).
func TestEncodeNodeThroughValue(t *testing.T) {
	mat := fakeMat{nodes: map[uint64]Node{5: {ID: 5, Labels: []string{"X"}, ElementID: "4:db:5"}}}
	got, err := EncodeValue(value.Node(5), Version{5, 4}, mat)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s, ok := got.(pack.Structure)
	if !ok || s.Tag != SigNode || s.Fields[0] != int64(5) {
		t.Errorf("encoded %#v", got)
	}
}

// TestEncodePathWorkedExample reproduces the spec's path worked example (doc 18
// §6.4): (a)-[r1]->(b)-[r2]->(a) gives distinct nodes [a,b], distinct rels
// [r1,r2], and index list [1,1,2,0].
func TestEncodePathWorkedExample(t *testing.T) {
	const a, b uint64 = 1, 2
	const r1, r2 uint64 = 10, 20
	mat := fakeMat{
		nodes: map[uint64]Node{
			a: {ID: 1, ElementID: "4:db:1"},
			b: {ID: 2, ElementID: "4:db:2"},
		},
		rels: map[uint64]Rel{
			r1: {ID: 10, StartID: 1, EndID: 2, Type: "T", ElementID: "5:db:10"}, // a->b
			r2: {ID: 20, StartID: 2, EndID: 1, Type: "T", ElementID: "5:db:20"}, // b->a
		},
	}
	// Path value: a, r1, b, r2, a.
	p := value.Path(value.Node(a), value.Rel(r1), value.Node(b), value.Rel(r2), value.Node(a))
	got, err := EncodeValue(p, Version{5, 4}, mat)
	if err != nil {
		t.Fatalf("encode path: %v", err)
	}
	s := got.(pack.Structure)
	if s.Tag != SigPath || len(s.Fields) != 3 {
		t.Fatalf("path structure %#v", s)
	}
	nodes := s.Fields[0].([]any)
	rels := s.Fields[1].([]any)
	indices := s.Fields[2].([]any)
	if len(nodes) != 2 {
		t.Errorf("distinct nodes = %d, want 2", len(nodes))
	}
	if len(rels) != 2 {
		t.Errorf("distinct rels = %d, want 2", len(rels))
	}
	want := []any{int64(1), int64(1), int64(2), int64(0)}
	if !reflect.DeepEqual(indices, want) {
		t.Errorf("index list %v, want %v", indices, want)
	}
}

// TestEncodePathReverseDirection confirms the relationship index sign goes
// negative when a step traverses a relationship against its stored direction
// (doc 18 §6.4).
func TestEncodePathReverseDirection(t *testing.T) {
	const a, b uint64 = 1, 2
	const r1 uint64 = 10
	mat := fakeMat{
		nodes: map[uint64]Node{a: {ID: 1}, b: {ID: 2}},
		// r1 is stored b->a, but the path traverses a->b, so the step is against
		// the stored direction: index -1.
		rels: map[uint64]Rel{r1: {ID: 10, StartID: 2, EndID: 1, Type: "T"}},
	}
	p := value.Path(value.Node(a), value.Rel(r1), value.Node(b))
	got, _ := EncodeValue(p, Version{5, 4}, mat)
	indices := got.(pack.Structure).Fields[2].([]any)
	if !reflect.DeepEqual(indices, []any{int64(-1), int64(1)}) {
		t.Errorf("index list %v, want [-1 1]", indices)
	}
}

// TestDecodeParamScalars confirms the reverse mapping for scalars, lists, and
// maps (doc 18 §6.9).
func TestDecodeParamScalars(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want value.Value
	}{
		{"null", nil, value.Null},
		{"bool", true, value.Bool(true)},
		{"int", int64(7), value.Int(7)},
		{"float", 2.5, value.Float(2.5)},
		{"string", "x", value.String("x")},
		{"list", []any{int64(1), "a"}, value.List(value.Int(1), value.String("a"))},
		{"map", map[string]any{"k": int64(1)}, value.Map(map[string]value.Value{"k": value.Int(1)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeParam(tc.in)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("decoded %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecodeParamNodeToElementID confirms a Node parameter reduces to its element
// id string, the convenience that lets a driver pass a node object back (doc 18
// §6.9).
func TestDecodeParamNodeToElementID(t *testing.T) {
	node := pack.Structure{Tag: SigNode, Fields: []any{int64(1), []any{"X"}, map[string]any{}, "4:db:1"}}
	got, err := DecodeParam(node)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Equal(value.String("4:db:1")) {
		t.Errorf("decoded %v, want element id 4:db:1", got)
	}
	rel := pack.Structure{Tag: SigRelationship, Fields: []any{int64(7), int64(1), int64(2), "T", map[string]any{}, "5:db:7", "4:db:1", "4:db:2"}}
	got, err = DecodeParam(rel)
	if err != nil {
		t.Fatalf("decode rel: %v", err)
	}
	if !got.Equal(value.String("5:db:7")) {
		t.Errorf("decoded %v, want element id 5:db:7", got)
	}
}

// TestDecodeParamUnknownStructure confirms an unrecognized parameter structure is
// a loud error (doc 18 §6.9: Neo.ClientError.Statement.TypeError).
func TestDecodeParamUnknownStructure(t *testing.T) {
	if _, err := DecodeParam(pack.Structure{Tag: 0x44, Fields: []any{int64(0)}}); err == nil {
		t.Error("unknown structure decoded without error")
	}
}

// TestEncodeDecodeParamRoundTrip confirms a scalar value round-trips out through
// EncodeValue and back through DecodeParam.
func TestEncodeDecodeParamRoundTrip(t *testing.T) {
	for _, v := range []value.Value{
		value.Int(123),
		value.String("round trip"),
		value.List(value.Bool(true), value.Float(3.5)),
		value.Map(map[string]value.Value{"a": value.Int(1), "b": value.String("two")}),
	} {
		enc, err := EncodeValue(v, Version{5, 4}, fakeMat{})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		dec, err := DecodeParam(enc)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !dec.Equal(v) {
			t.Errorf("round trip %v != %v", dec, v)
		}
	}
}
