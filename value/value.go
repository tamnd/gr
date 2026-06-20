// Package value defines gr's property value type system: the typed values that
// nodes and relationships carry, aligned with the Cypher/openCypher value model
// (spec 2060 doc 02, the data model). The set here is the M0/M1 core; temporal
// and spatial types are layered on later without changing the Value shape.
package value

import (
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Type is the dynamic type tag of a Value. The on-disk encodings (doc 03 §15,
// doc 15) key off these tags; the numeric values are part of the storage
// contract and must not be renumbered.
type Type uint8

const (
	TypeNull   Type = 0
	TypeBool   Type = 1
	TypeInt    Type = 2 // 64-bit signed integer
	TypeFloat  Type = 3 // IEEE-754 double
	TypeString Type = 4 // UTF-8 text
	TypeBytes  Type = 5 // opaque byte string
	TypeList   Type = 6 // ordered, heterogeneous
	TypeMap    Type = 7 // string-keyed, heterogeneous

	// Entity values are runtime-only query values: a handle to a node or
	// relationship, carrying its element id. They are never stored as a property
	// value, so they have no on-disk encoding (and need no reserved low tag); they
	// exist so the expression evaluator can carry entities through expressions —
	// n = m structural equality, id(n), RETURN n (doc 02 §4.4, doc 09 §6).
	TypeNode Type = 8
	TypeRel  Type = 9

	// TypePath is a runtime-only value: an alternating sequence of node and
	// relationship handles, node, rel, node, ..., node, produced by a named path
	// pattern (MATCH p = ...) and consumed by nodes(), relationships(), and
	// length() (doc 02 §4.4, doc 09 §3.4, §7). Like the entity handles it is never
	// stored as a property value, so it has no on-disk encoding.
	TypePath Type = 10
)

func (t Type) String() string {
	switch t {
	case TypeNull:
		return "NULL"
	case TypeBool:
		return "BOOLEAN"
	case TypeInt:
		return "INTEGER"
	case TypeFloat:
		return "FLOAT"
	case TypeString:
		return "STRING"
	case TypeBytes:
		return "BYTES"
	case TypeList:
		return "LIST"
	case TypeMap:
		return "MAP"
	case TypeNode:
		return "NODE"
	case TypeRel:
		return "RELATIONSHIP"
	case TypePath:
		return "PATH"
	default:
		return "UNKNOWN(" + strconv.Itoa(int(t)) + ")"
	}
}

// Value is a single typed property value. The zero Value is Null, so a Value
// field that is never set reads as null, matching Cypher's missing-property
// semantics.
type Value struct {
	t Type
	// scalar holds bool/int/float bit-packed; b holds string/bytes; list/m hold
	// the composite payloads. Only the field(s) relevant to t are meaningful.
	scalar uint64
	b      []byte
	list   []Value
	m      map[string]Value
}

// Null is the singleton null value.
var Null = Value{t: TypeNull}

// Constructors.

func Bool(v bool) Value {
	var s uint64
	if v {
		s = 1
	}
	return Value{t: TypeBool, scalar: s}
}

func Int(v int64) Value     { return Value{t: TypeInt, scalar: uint64(v)} }
func Float(v float64) Value { return Value{t: TypeFloat, scalar: math.Float64bits(v)} }
func String(v string) Value { return Value{t: TypeString, b: []byte(v)} }

func Bytes(v []byte) Value {
	cp := make([]byte, len(v))
	copy(cp, v)
	return Value{t: TypeBytes, b: cp}
}

func List(vs ...Value) Value {
	cp := make([]Value, len(vs))
	copy(cp, vs)
	return Value{t: TypeList, list: cp}
}

func Map(m map[string]Value) Value {
	cp := make(map[string]Value, len(m))
	maps.Copy(cp, m)
	return Value{t: TypeMap, m: cp}
}

// Node and Rel build entity handles carrying an element id. The id is the engine
// NodeID/RelID widened to uint64; the value package stays free of an engine
// import by holding the raw id, with the caller converting at the boundary.
func Node(id uint64) Value { return Value{t: TypeNode, scalar: id} }
func Rel(id uint64) Value  { return Value{t: TypeRel, scalar: id} }

// Path builds a path value from its alternating elements: a node, then a
// relationship and a node per traversal step. The caller guarantees the
// alternation and that the sequence begins and ends with a node (a path has one
// more node than relationship). The elements are held as one ordered sequence;
// PathNodes and PathRels are the two projections that back nodes() and
// relationships().
func Path(elems ...Value) Value {
	cp := make([]Value, len(elems))
	copy(cp, elems)
	return Value{t: TypePath, list: cp}
}

// Accessors.

func (v Value) Type() Type   { return v.t }
func (v Value) IsNull() bool { return v.t == TypeNull }

func (v Value) AsBool() (bool, bool) {
	if v.t != TypeBool {
		return false, false
	}
	return v.scalar != 0, true
}

func (v Value) AsInt() (int64, bool) {
	if v.t != TypeInt {
		return 0, false
	}
	return int64(v.scalar), true
}

func (v Value) AsFloat() (float64, bool) {
	switch v.t {
	case TypeFloat:
		return math.Float64frombits(v.scalar), true
	case TypeInt:
		return float64(int64(v.scalar)), true // numeric widening
	default:
		return 0, false
	}
}

func (v Value) AsString() (string, bool) {
	if v.t != TypeString {
		return "", false
	}
	return string(v.b), true
}

func (v Value) AsBytes() ([]byte, bool) {
	if v.t != TypeBytes {
		return nil, false
	}
	return v.b, true
}

func (v Value) AsList() ([]Value, bool) {
	if v.t != TypeList {
		return nil, false
	}
	return v.list, true
}

func (v Value) AsMap() (map[string]Value, bool) {
	if v.t != TypeMap {
		return nil, false
	}
	return v.m, true
}

func (v Value) AsNode() (uint64, bool) {
	if v.t != TypeNode {
		return 0, false
	}
	return v.scalar, true
}

func (v Value) AsRel() (uint64, bool) {
	if v.t != TypeRel {
		return 0, false
	}
	return v.scalar, true
}

// AsPath returns the path's alternating element sequence (node, rel, ..., node).
func (v Value) AsPath() ([]Value, bool) {
	if v.t != TypePath {
		return nil, false
	}
	return v.list, true
}

// PathNodes returns the path's nodes, the even positions in the element sequence.
func (v Value) PathNodes() []Value {
	if v.t != TypePath {
		return nil
	}
	out := make([]Value, 0, (len(v.list)+1)/2)
	for i := 0; i < len(v.list); i += 2 {
		out = append(out, v.list[i])
	}
	return out
}

// PathRels returns the path's relationships, the odd positions in the sequence.
func (v Value) PathRels() []Value {
	if v.t != TypePath {
		return nil
	}
	out := make([]Value, 0, len(v.list)/2)
	for i := 1; i < len(v.list); i += 2 {
		out = append(out, v.list[i])
	}
	return out
}

// PathLen returns the path's length: its number of relationships.
func (v Value) PathLen() int {
	if v.t != TypePath {
		return 0
	}
	return len(v.list) / 2
}

// Equal reports structural equality, used by tests and the storage round-trip
// checks. Two nulls are equal here (this is value identity, not Cypher's
// three-valued IS comparison, which lives in the expression evaluator).
func (v Value) Equal(o Value) bool {
	if v.t != o.t {
		return false
	}
	switch v.t {
	case TypeNull:
		return true
	case TypeBool, TypeInt, TypeNode, TypeRel:
		return v.scalar == o.scalar
	case TypeFloat:
		a, b := math.Float64frombits(v.scalar), math.Float64frombits(o.scalar)
		if math.IsNaN(a) && math.IsNaN(b) {
			return true
		}
		return a == b
	case TypeString, TypeBytes:
		if len(v.b) != len(o.b) {
			return false
		}
		for i := range v.b {
			if v.b[i] != o.b[i] {
				return false
			}
		}
		return true
	case TypeList, TypePath:
		if len(v.list) != len(o.list) {
			return false
		}
		for i := range v.list {
			if !v.list[i].Equal(o.list[i]) {
				return false
			}
		}
		return true
	case TypeMap:
		if len(v.m) != len(o.m) {
			return false
		}
		for k, a := range v.m {
			b, ok := o.m[k]
			if !ok || !a.Equal(b) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// String renders a value for display and debugging. It is deterministic for
// maps (keys sorted), so golden tests are stable.
func (v Value) String() string {
	switch v.t {
	case TypeNull:
		return "null"
	case TypeBool:
		if v.scalar != 0 {
			return "true"
		}
		return "false"
	case TypeInt:
		return strconv.FormatInt(int64(v.scalar), 10)
	case TypeFloat:
		return strconv.FormatFloat(math.Float64frombits(v.scalar), 'g', -1, 64)
	case TypeString:
		return strconv.Quote(string(v.b))
	case TypeBytes:
		return fmt.Sprintf("0x%x", v.b)
	case TypeList:
		parts := make([]string, len(v.list))
		for i, e := range v.list {
			parts[i] = e.String()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case TypeMap:
		keys := make([]string, 0, len(v.m))
		for k := range v.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ": " + v.m[k].String()
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case TypeNode:
		return "node(" + strconv.FormatUint(v.scalar, 10) + ")"
	case TypeRel:
		return "rel(" + strconv.FormatUint(v.scalar, 10) + ")"
	case TypePath:
		parts := make([]string, len(v.list))
		for i, e := range v.list {
			parts[i] = e.String()
		}
		return "path[" + strings.Join(parts, ", ") + "]"
	default:
		return "?"
	}
}
