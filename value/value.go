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

func Int(v int64) Value    { return Value{t: TypeInt, scalar: uint64(v)} }
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
	case TypeBool, TypeInt:
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
	case TypeList:
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
	default:
		return "?"
	}
}
