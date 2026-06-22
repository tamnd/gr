package gr

import (
	"errors"
	"fmt"

	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/value"
)

// Value is a Cypher value surfaced to a Go program (doc 16 §9.1). It is a type alias
// for any, so a value read from a record is a plain interface value carrying one of
// the concrete Go types the value model maps each Cypher type to (§9.2): nil for
// null, bool, int64, float64, string, []byte, []Value for a list, map[string]Value
// for a map, and Node/Relationship/Path for the graph types.
type Value = any

// Params is the parameter map passed to a query: parameter names without the $
// mapped to Go values (doc 16 §7.3). It is a type alias for map[string]any, so a
// plain map literal and a gr.Params literal are interchangeable. A nil map is fine
// for a parameterless query.
type Params = map[string]any

// ErrParam is returned when a parameter value is not a representable Cypher value
// (doc 16 §7.3, §15.9): a channel, a function, or any Go type the value model does
// not map. It is surfaced before execution so a program learns of the bad parameter
// at the call site, not mid-stream.
var ErrParam = errors.New("gr: parameter is not a representable Cypher value")

// ErrNoColumn is returned by a record's typed accessor when the named column is not
// in the result (doc 16 §8.3). It marks a programming error (a wrong column name),
// distinct from a column that is present but null.
var ErrNoColumn = errors.New("gr: no such column")

// ErrType is returned by a record's typed accessor when the column's value is not of
// the requested type (doc 16 §9.5). It marks a mismatch between the asserted Go type
// and the value the column actually holds.
var ErrType = errors.New("gr: value is not of the requested type")

// Node is a node value read from a result (doc 16 §9.3, §10). It carries the node's
// identity; the lazily-fetched labels and properties are a later slice (the
// graph-object model, §10.6), so v1's Node exposes the id the executor binds.
type Node struct{ ID uint64 }

// Relationship is a relationship value read from a result (doc 16 §9.3, §10). Like
// Node it carries identity now; its type and properties follow with the graph-object
// model.
type Relationship struct{ ID uint64 }

// Path is a path value read from a result: an alternating sequence of node and
// relationship values, node first (doc 16 §9.3). It mirrors the executor's runtime
// path handle.
type Path struct{ Elements []Value }

// toValue converts a Go parameter value to a Cypher value (doc 16 §7.3, §9). It
// mirrors the result mapping in reverse: a Go int of any width becomes an Integer, a
// float becomes a Float, a string a String, a []byte a Bytes, a bool a Boolean, a
// []any a List, and a map[string]any a Map, recursively. A value the model does not
// represent returns ErrParam, so a bad parameter is caught before the query runs.
func toValue(v any) (value.Value, error) {
	switch x := v.(type) {
	case nil:
		return value.Value{}, nil
	case bool:
		return value.Bool(x), nil
	case int:
		return value.Int(int64(x)), nil
	case int8:
		return value.Int(int64(x)), nil
	case int16:
		return value.Int(int64(x)), nil
	case int32:
		return value.Int(int64(x)), nil
	case int64:
		return value.Int(x), nil
	case uint:
		return value.Int(int64(x)), nil
	case uint8:
		return value.Int(int64(x)), nil
	case uint16:
		return value.Int(int64(x)), nil
	case uint32:
		return value.Int(int64(x)), nil
	case uint64:
		return value.Int(int64(x)), nil
	case float32:
		return value.Float(float64(x)), nil
	case float64:
		return value.Float(x), nil
	case string:
		return value.String(x), nil
	case []byte:
		return value.Bytes(x), nil
	case []any:
		out := make([]value.Value, len(x))
		for i, e := range x {
			cv, err := toValue(e)
			if err != nil {
				return value.Value{}, err
			}
			out[i] = cv
		}
		return value.List(out...), nil
	case map[string]any:
		out := make(map[string]value.Value, len(x))
		for k, e := range x {
			cv, err := toValue(e)
			if err != nil {
				return value.Value{}, err
			}
			out[k] = cv
		}
		return value.Map(out), nil
	default:
		return value.Value{}, fmt.Errorf("%w: %T", ErrParam, v)
	}
}

// toValues converts a whole parameter map to the internal value map a query runs
// against (doc 16 §7.3). A nil map passes through as nil; a single unrepresentable
// value fails the whole call with ErrParam, the fail-before-execution contract.
func toValues(params Params) (map[string]value.Value, error) {
	if params == nil {
		return nil, nil
	}
	out := make(map[string]value.Value, len(params))
	for k, v := range params {
		cv, err := toValue(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", k, err)
		}
		out[k] = cv
	}
	return out, nil
}

// fromValue converts an internal Cypher value to the Go value a record hands out
// (doc 16 §9.2, §9.3). It is the result-side mapping: null to nil, the scalars to
// their Go types, a list to []Value and a map to map[string]Value (recursively), and
// the graph types to Node/Relationship/Path.
func fromValue(v value.Value) Value {
	switch v.Type() {
	case value.TypeNull:
		return nil
	case value.TypeBool:
		b, _ := v.AsBool()
		return b
	case value.TypeInt:
		i, _ := v.AsInt()
		return i
	case value.TypeFloat:
		f, _ := v.AsFloat()
		return f
	case value.TypeString:
		s, _ := v.AsString()
		return s
	case value.TypeBytes:
		b, _ := v.AsBytes()
		return b
	case value.TypeList:
		l, _ := v.AsList()
		out := make([]Value, len(l))
		for i, e := range l {
			out[i] = fromValue(e)
		}
		return out
	case value.TypeMap:
		m, _ := v.AsMap()
		out := make(map[string]Value, len(m))
		for k, e := range m {
			out[k] = fromValue(e)
		}
		return out
	case value.TypeNode:
		id, _ := v.AsNode()
		return Node{ID: id}
	case value.TypeRel:
		id, _ := v.AsRel()
		return Relationship{ID: id}
	case value.TypePath:
		el, _ := v.AsPath()
		out := make([]Value, len(el))
		for i, e := range el {
			out[i] = fromValue(e)
		}
		return Path{Elements: out}
	default:
		return nil
	}
}

// Record is one row of a result, with columns accessible by name and by index (doc
// 16 §8.3). It is valid only until the next Next call on the result it came from
// (§8.5); a program that needs a value longer keeps the value, not the record.
type Record struct {
	keys []string
	keep map[string]struct{}
	row  eval.Row
}

// newRecord wraps a result row in a record over the result's column names. The key
// set lets Get distinguish a column that is absent from the result (a programming
// error) from one that is present but null (a query-level null).
func newRecord(keys []string, row eval.Row) *Record {
	keep := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keep[k] = struct{}{}
	}
	return &Record{keys: keys, keep: keep, row: row}
}

// Keys returns the record's column names in the order the RETURN or WITH clause
// produced them (doc 16 §8.3).
func (r *Record) Keys() []string { return r.keys }

// Values returns the column values in column order, positionally aligned with Keys
// (doc 16 §8.3).
func (r *Record) Values() []Value {
	out := make([]Value, len(r.keys))
	for i, k := range r.keys {
		out[i] = fromValue(r.row[k])
	}
	return out
}

// Get looks up a column by name (doc 16 §8.3). It returns ok=false when the name is
// not a column of the result, and (nil, true) for a column that is present but null,
// so a program can tell a missing column from a null value.
func (r *Record) Get(key string) (Value, bool) {
	if _, ok := r.keep[key]; !ok {
		return nil, false
	}
	return fromValue(r.row[key]), true
}

// GetByIndex reads the i-th column positionally, for code that iterates columns
// generically (doc 16 §8.3). It returns nil for an out-of-range index.
func (r *Record) GetByIndex(i int) Value {
	if i < 0 || i >= len(r.keys) {
		return nil
	}
	return fromValue(r.row[r.keys[i]])
}

// AsMap returns the record as a name-to-value map (doc 16 §8.3).
func (r *Record) AsMap() map[string]Value {
	out := make(map[string]Value, len(r.keys))
	for _, k := range r.keys {
		out[k] = fromValue(r.row[k])
	}
	return out
}

// GetString reads a column asserted to be a String (doc 16 §9.5). It returns
// ErrNoColumn when the column is absent and ErrType when its value is not a string.
func (r *Record) GetString(key string) (string, error) {
	v, ok := r.Get(key)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: column %q is %T, not string", ErrType, key, v)
	}
	return s, nil
}

// GetInt reads a column asserted to be an Integer, returned as int64 (doc 16 §9.2,
// §9.5).
func (r *Record) GetInt(key string) (int64, error) {
	v, ok := r.Get(key)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	i, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("%w: column %q is %T, not int64", ErrType, key, v)
	}
	return i, nil
}

// GetFloat reads a column asserted to be a Float (doc 16 §9.5).
func (r *Record) GetFloat(key string) (float64, error) {
	v, ok := r.Get(key)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("%w: column %q is %T, not float64", ErrType, key, v)
	}
	return f, nil
}

// GetBool reads a column asserted to be a Boolean (doc 16 §9.5).
func (r *Record) GetBool(key string) (bool, error) {
	v, ok := r.Get(key)
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%w: column %q is %T, not bool", ErrType, key, v)
	}
	return b, nil
}

// GetBytes reads a column asserted to be a Bytes value (doc 16 §9.5). The returned
// slice may reference engine memory and is valid only until the next Next (doc 16
// §8.5); a program that needs it longer copies it.
func (r *Record) GetBytes(key string) ([]byte, error) {
	v, ok := r.Get(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("%w: column %q is %T, not []byte", ErrType, key, v)
	}
	return b, nil
}

// GetNode reads a column asserted to be a Node (doc 16 §9.5).
func (r *Record) GetNode(key string) (Node, error) {
	v, ok := r.Get(key)
	if !ok {
		return Node{}, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	n, ok := v.(Node)
	if !ok {
		return Node{}, fmt.Errorf("%w: column %q is %T, not Node", ErrType, key, v)
	}
	return n, nil
}

// GetRelationship reads a column asserted to be a Relationship (doc 16 §9.5).
func (r *Record) GetRelationship(key string) (Relationship, error) {
	v, ok := r.Get(key)
	if !ok {
		return Relationship{}, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	rel, ok := v.(Relationship)
	if !ok {
		return Relationship{}, fmt.Errorf("%w: column %q is %T, not Relationship", ErrType, key, v)
	}
	return rel, nil
}

// GetPath reads a column asserted to be a Path (doc 16 §9.5).
func (r *Record) GetPath(key string) (Path, error) {
	v, ok := r.Get(key)
	if !ok {
		return Path{}, fmt.Errorf("%w: %q", ErrNoColumn, key)
	}
	p, ok := v.(Path)
	if !ok {
		return Path{}, fmt.Errorf("%w: column %q is %T, not Path", ErrType, key, v)
	}
	return p, nil
}

// Single returns the one record of a result that must hold exactly one row (doc 16
// §6.6). It is the helper a transaction function uses to pull a single RETURN row: it
// fails if the result errors, if it is empty, or if it has more than one row, and it
// closes the result before returning. It is the gr spelling of the Neo4j driver's
// Single.
func Single(res *Result) (*Record, error) {
	defer func() { _ = res.Close() }()
	if !res.Next() {
		if err := res.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("gr: Single: result is empty")
	}
	rec := res.Record()
	// The next Next may reuse the row buffer, so clone the record's row before the
	// lookahead that checks for a second row, or the returned record could alias a
	// later row.
	rec = newRecord(rec.keys, cloneRow(rec.row))
	if res.Next() {
		return nil, errors.New("gr: Single: result has more than one row")
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return rec, nil
}
