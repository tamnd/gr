package eval

import (
	"fmt"
	"sort"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// The entity functions read a node's or relationship's catalog-resolved facts —
// its labels, its type, its property keys, its property map — and return them as
// names and values rather than tokens (doc 09 §7). They need the snapshot (the
// Tx) to read the facts and the reverse resolvers (token to name) to name them,
// both carried on the Env. A null argument yields null; a non-entity argument is
// a type error, except properties() which also accepts a map.

// fnLabels returns the labels of a node as a list of label-name strings.
func fnLabels(args []value.Value, env *Env) (value.Value, error) {
	if err := arity("labels", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	id, ok := a.AsNode()
	if !ok {
		return value.Null, fmt.Errorf("eval: labels requires a node, got %s", a.Type())
	}
	if env.Tx == nil {
		return value.Null, nil
	}
	toks, err := env.Tx.NodeLabels(engine.NodeID(id))
	if err != nil {
		return value.Null, err
	}
	return tokenNames(toks, env.LabelName), nil
}

// fnType returns the type of a relationship as a string.
func fnType(args []value.Value, env *Env) (value.Value, error) {
	if err := arity("type", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	id, ok := a.AsRel()
	if !ok {
		return value.Null, fmt.Errorf("eval: type requires a relationship, got %s", a.Type())
	}
	if env.Tx == nil {
		return value.Null, nil
	}
	tok, err := env.Tx.RelType(engine.RelID(id))
	if err != nil {
		return value.Null, err
	}
	if name, ok := nameOf(env.RelTypeName, tok); ok {
		return value.String(name), nil
	}
	return value.Null, nil
}

// fnKeys returns the property keys of a node or relationship as a sorted list of
// key-name strings. The sort makes the result deterministic (the key order in the
// store is an implementation detail).
func fnKeys(args []value.Value, env *Env) (value.Value, error) {
	if err := arity("keys", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	toks, err := entityKeys(env, a)
	if err != nil {
		return value.Null, err
	}
	names := make([]string, 0, len(toks))
	for _, t := range toks {
		if name, ok := nameOf(env.PropKeyName, t); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	vs := make([]value.Value, len(names))
	for i, n := range names {
		vs[i] = value.String(n)
	}
	return value.List(vs...), nil
}

// fnProperties returns the properties of a node or relationship as a map from
// key name to value; a map argument is returned unchanged (the Cypher identity on
// maps), and null is null.
func fnProperties(args []value.Value, env *Env) (value.Value, error) {
	if err := arity("properties", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	switch a.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeMap:
		return a, nil
	case value.TypeNode, value.TypeRel:
		return entityProperties(env, a)
	default:
		return value.Null, fmt.Errorf("eval: properties requires a node, relationship, or map, got %s", a.Type())
	}
}

// entityKeys returns the property-key tokens of a node or relationship value.
func entityKeys(env *Env, a value.Value) ([]engine.Token, error) {
	if env.Tx == nil {
		return nil, nil
	}
	if id, ok := a.AsNode(); ok {
		return env.Tx.NodePropertyKeys(engine.NodeID(id))
	}
	if id, ok := a.AsRel(); ok {
		return env.Tx.RelPropertyKeys(engine.RelID(id))
	}
	return nil, fmt.Errorf("eval: keys requires a node or relationship, got %s", a.Type())
}

// entityProperties builds the property map of a node or relationship: each key it
// carries, named through the reverse resolver, mapped to its value.
func entityProperties(env *Env, a value.Value) (value.Value, error) {
	if env.Tx == nil {
		return value.Null, nil
	}
	node := a.Type() == value.TypeNode
	id, _ := rawID(a)
	toks, err := entityKeys(env, a)
	if err != nil {
		return value.Null, err
	}
	m := make(map[string]value.Value, len(toks))
	for _, t := range toks {
		name, ok := nameOf(env.PropKeyName, t)
		if !ok {
			continue
		}
		var v value.Value
		if node {
			v, err = env.Tx.NodeProperty(engine.NodeID(id), t)
		} else {
			v, err = env.Tx.RelProperty(engine.RelID(id), t)
		}
		if err != nil {
			return value.Null, err
		}
		m[name] = v
	}
	return value.Map(m), nil
}

// rawID extracts the raw element id from a node or relationship value.
func rawID(a value.Value) (uint64, bool) {
	if id, ok := a.AsNode(); ok {
		return id, true
	}
	if id, ok := a.AsRel(); ok {
		return id, true
	}
	return 0, false
}

// tokenNames maps a token slice to a value.List of their names, dropping any the
// resolver cannot name (a defensive case the catalog should make impossible).
func tokenNames(toks []engine.Token, resolve func(engine.Token) (string, bool)) value.Value {
	vs := make([]value.Value, 0, len(toks))
	for _, t := range toks {
		if name, ok := nameOf(resolve, t); ok {
			vs = append(vs, value.String(name))
		}
	}
	return value.List(vs...)
}

// nameOf applies a reverse resolver, reporting false if it is nil or misses.
func nameOf(resolve func(engine.Token) (string, bool), t engine.Token) (string, bool) {
	if resolve == nil {
		return "", false
	}
	return resolve(t)
}
