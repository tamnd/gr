// Package eval is the Cypher expression evaluator: it computes one expression
// against one row of bindings, implementing the value type system, three-valued
// logic and null handling, and the read-side scalar operators and functions
// (spec 2060 doc 02 §6, §7; doc 09 §6, §7; doc 25 §5.2 deliverable 7).
//
// It is the read path's arithmetic core, consumed by the executor's Filter,
// Project, and Sort operators (deliverable 6). It is strictly per-row and does
// not aggregate: an aggregate (count, sum, collect …) is computed by the
// Aggregate operator over a stream of rows, not here. The entity functions that
// name a node's or relationship's catalog facts (labels, type, keys, properties)
// live in entity.go and read through the Tx plus the reverse name resolvers on
// the Env; the path functions (nodes, relationships) are deferred until the Path
// value type lands with shortestPath (deliverable 8b).
//
// Evaluation distinguishes null from error. A comparison or arithmetic step over
// a null operand yields null (three-valued logic); a genuine type misuse (adding
// a list to a number, NOT of a string, an unknown function) is an error. The
// executor turns a WHERE result into a kept/dropped decision: a row survives only
// when its predicate evaluates to true, never to null or false (doc 02 §6.2).
package eval

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// Row is one tuple of bindings: variable name to value. A node or relationship
// binding is held as a value.Node / value.Rel entity handle. M2 is tuple-at-a-
// time; the vectorized row batch (doc 12 §2) replaces this shape in M4 without
// changing the evaluator's per-value semantics.
type Row map[string]value.Value

// Env is the evaluation context for one row: the row's bindings, the query
// parameters, the read transaction used to resolve entity properties, and the
// property-key resolver (name to catalog token, with ok=false for an unknown key
// under the schema-optional model). Tx and Resolve may be nil when no expression
// reads an entity property.
type Env struct {
	Row     Row
	Params  map[string]value.Value
	Tx      engine.Tx
	Resolve func(name string) (engine.Token, bool)
	// LabelName, RelTypeName, and PropKeyName are the reverse resolvers: a catalog
	// token to the name it interns. They back the entity functions labels(), type(),
	// and keys()/properties() (doc 09 §7), which return names rather than tokens.
	// They may be nil when no expression names an entity's labels, type, or keys.
	LabelName   func(t engine.Token) (string, bool)
	RelTypeName func(t engine.Token) (string, bool)
	PropKeyName func(t engine.Token) (string, bool)
}

// Eval computes an expression against the environment. It returns a value (which
// may be null) or an error for an operation that is a genuine misuse rather than
// a null-propagating one.
func Eval(e ast.Expr, env *Env) (value.Value, error) {
	switch x := e.(type) {
	case *ast.Literal:
		return x.Value, nil
	case *ast.Param:
		v, ok := env.Params[x.Name]
		if !ok {
			return value.Null, fmt.Errorf("eval: parameter $%s not supplied", x.Name)
		}
		return v, nil
	case *ast.Variable:
		// An unbound variable reads as null (an OPTIONAL MATCH leaves its variables
		// null when it does not match); the binder has already rejected references
		// to names that are not in scope at all.
		if env.Row == nil {
			return value.Null, nil
		}
		return env.Row[x.Name], nil
	case *ast.Property:
		base, err := Eval(x.Base, env)
		if err != nil {
			return value.Null, err
		}
		return propertyOf(base, x.Key, env)
	case *ast.Index:
		return evalIndex(x, env)
	case *ast.Slice:
		return evalSlice(x, env)
	case *ast.Unary:
		return evalUnary(x, env)
	case *ast.Binary:
		return evalBinary(x, env)
	case *ast.IsNull:
		return evalIsNull(x, env)
	case *ast.ListLit:
		return evalList(x, env)
	case *ast.MapLit:
		return evalMap(x, env)
	case *ast.FunctionCall:
		return evalCall(x, env)
	case *ast.Case:
		return evalCase(x, env)
	default:
		return value.Null, fmt.Errorf("eval: unsupported expression %T", e)
	}
}

// propertyOf reads a key from a map or an entity. Access on null is null, and an
// absent map key or an unknown property key (schema-optional) is null too.
func propertyOf(base value.Value, key string, env *Env) (value.Value, error) {
	switch base.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeMap:
		m, _ := base.AsMap()
		return m[key], nil
	case value.TypeNode:
		id, _ := base.AsNode()
		return entityProperty(env, true, id, key)
	case value.TypeRel:
		id, _ := base.AsRel()
		return entityProperty(env, false, id, key)
	default:
		return value.Null, fmt.Errorf("eval: cannot read property %q of %s", key, base.Type())
	}
}

// entityProperty resolves the key to a token and reads it from the snapshot. An
// unknown key (no token) reads as null without touching the engine, the
// schema-optional default (doc 08 §5.3).
func entityProperty(env *Env, node bool, id uint64, key string) (value.Value, error) {
	if env.Resolve == nil || env.Tx == nil {
		return value.Null, nil
	}
	tok, ok := env.Resolve(key)
	if !ok {
		return value.Null, nil
	}
	if node {
		return env.Tx.NodeProperty(engine.NodeID(id), tok)
	}
	return env.Tx.RelProperty(engine.RelID(id), tok)
}

func evalUnary(x *ast.Unary, env *Env) (value.Value, error) {
	v, err := Eval(x.X, env)
	if err != nil {
		return value.Null, err
	}
	switch x.Op {
	case ast.OpNot:
		if v.IsNull() {
			return value.Null, nil
		}
		b, ok := v.AsBool()
		if !ok {
			return value.Null, fmt.Errorf("eval: NOT requires a boolean, got %s", v.Type())
		}
		return value.Bool(!b), nil
	case ast.OpNeg:
		return negate(v)
	default:
		return value.Null, fmt.Errorf("eval: unknown unary operator")
	}
}

func negate(v value.Value) (value.Value, error) {
	switch v.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeInt:
		i, _ := v.AsInt()
		return value.Int(-i), nil
	case value.TypeFloat:
		f, _ := v.AsFloat()
		return value.Float(-f), nil
	default:
		return value.Null, fmt.Errorf("eval: unary minus requires a number, got %s", v.Type())
	}
}

// evalBinary dispatches by operator group. The boolean connectives are handled
// first because they short-circuit and follow Kleene logic, not strict operand
// evaluation.
func evalBinary(x *ast.Binary, env *Env) (value.Value, error) {
	switch x.Op {
	case ast.OpAnd, ast.OpOr, ast.OpXor:
		return evalBoolOp(x, env)
	}
	l, err := Eval(x.L, env)
	if err != nil {
		return value.Null, err
	}
	r, err := Eval(x.R, env)
	if err != nil {
		return value.Null, err
	}
	switch x.Op {
	case ast.OpAdd:
		return add(l, r)
	case ast.OpSub:
		return arith(l, r, '-')
	case ast.OpMul:
		return arith(l, r, '*')
	case ast.OpDiv:
		return arith(l, r, '/')
	case ast.OpMod:
		return arith(l, r, '%')
	case ast.OpPow:
		return power(l, r)
	case ast.OpEq:
		return eq3(l, r), nil
	case ast.OpNe:
		return neg3(eq3(l, r)), nil
	case ast.OpLt, ast.OpLe, ast.OpGt, ast.OpGe:
		return compare3(x.Op, l, r), nil
	case ast.OpStartsWith, ast.OpEndsWith, ast.OpContains:
		return stringPred(x.Op, l, r), nil
	case ast.OpRegex:
		return regexMatch(l, r)
	case ast.OpIn:
		return inList(l, r)
	default:
		return value.Null, fmt.Errorf("eval: unknown binary operator")
	}
}

// evalBoolOp evaluates AND/OR/XOR under three-valued (Kleene) logic, short-
// circuiting on a dominant left operand (false for AND, true for OR) so a
// guarded right operand is not evaluated when the result is already determined.
func evalBoolOp(x *ast.Binary, env *Env) (value.Value, error) {
	l, err := Eval(x.L, env)
	if err != nil {
		return value.Null, err
	}
	lb, lok, err := asBool3(l, x.Op)
	if err != nil {
		return value.Null, err
	}
	switch x.Op {
	case ast.OpAnd:
		if lok && !lb {
			return value.Bool(false), nil
		}
	case ast.OpOr:
		if lok && lb {
			return value.Bool(true), nil
		}
	}
	r, err := Eval(x.R, env)
	if err != nil {
		return value.Null, err
	}
	rb, rok, err := asBool3(r, x.Op)
	if err != nil {
		return value.Null, err
	}
	switch x.Op {
	case ast.OpAnd:
		if rok && !rb {
			return value.Bool(false), nil
		}
		if !lok || !rok {
			return value.Null, nil
		}
		return value.Bool(true), nil
	case ast.OpOr:
		if rok && rb {
			return value.Bool(true), nil
		}
		if !lok || !rok {
			return value.Null, nil
		}
		return value.Bool(false), nil
	default: // OpXor
		if !lok || !rok {
			return value.Null, nil
		}
		return value.Bool(lb != rb), nil
	}
}

// asBool3 reads a boolean operand for a connective: ok is false for null, and a
// non-boolean non-null operand is an error.
func asBool3(v value.Value, op ast.BinaryOp) (b, ok bool, err error) {
	if v.IsNull() {
		return false, false, nil
	}
	b, ok = v.AsBool()
	if !ok {
		return false, false, fmt.Errorf("eval: %s requires booleans, got %s", op, v.Type())
	}
	return b, true, nil
}

func evalIsNull(x *ast.IsNull, env *Env) (value.Value, error) {
	v, err := Eval(x.X, env)
	if err != nil {
		return value.Null, err
	}
	// IS NULL / IS NOT NULL are always definitely true or false, never null.
	if x.Negate {
		return value.Bool(!v.IsNull()), nil
	}
	return value.Bool(v.IsNull()), nil
}

func evalList(x *ast.ListLit, env *Env) (value.Value, error) {
	out := make([]value.Value, len(x.Elems))
	for i, el := range x.Elems {
		v, err := Eval(el, env)
		if err != nil {
			return value.Null, err
		}
		out[i] = v
	}
	return value.List(out...), nil
}

func evalMap(x *ast.MapLit, env *Env) (value.Value, error) {
	m := make(map[string]value.Value, len(x.Entries))
	for _, e := range x.Entries {
		v, err := Eval(e.Value, env)
		if err != nil {
			return value.Null, err
		}
		m[e.Key] = v
	}
	return value.Map(m), nil
}

// evalIndex is dynamic element access: a list position (negative counts from the
// end, out of range is null), a map key, or dynamic property access on an entity.
func evalIndex(x *ast.Index, env *Env) (value.Value, error) {
	base, err := Eval(x.Base, env)
	if err != nil {
		return value.Null, err
	}
	idx, err := Eval(x.Index, env)
	if err != nil {
		return value.Null, err
	}
	if base.IsNull() || idx.IsNull() {
		return value.Null, nil
	}
	switch base.Type() {
	case value.TypeList:
		lst, _ := base.AsList()
		i, ok := idx.AsInt()
		if !ok {
			return value.Null, fmt.Errorf("eval: list index must be an integer, got %s", idx.Type())
		}
		n := int64(len(lst))
		if i < 0 {
			i += n
		}
		if i < 0 || i >= n {
			return value.Null, nil
		}
		return lst[i], nil
	case value.TypeMap, value.TypeNode, value.TypeRel:
		key, ok := idx.AsString()
		if !ok {
			return value.Null, fmt.Errorf("eval: property key must be a string, got %s", idx.Type())
		}
		return propertyOf(base, key, env)
	default:
		return value.Null, fmt.Errorf("eval: cannot index %s", base.Type())
	}
}

// evalSlice is list slicing base[lo..hi]; either bound may be open, negative
// bounds count from the end, and the range is clamped to the list.
func evalSlice(x *ast.Slice, env *Env) (value.Value, error) {
	base, err := Eval(x.Base, env)
	if err != nil {
		return value.Null, err
	}
	if base.IsNull() {
		return value.Null, nil
	}
	lst, ok := base.AsList()
	if !ok {
		return value.Null, fmt.Errorf("eval: slice requires a list, got %s", base.Type())
	}
	n := int64(len(lst))
	lo, hi := int64(0), n
	if x.Lo != nil {
		i, null, err := intBound(x.Lo, env)
		if err != nil || null {
			return value.Null, err
		}
		lo = i
	}
	if x.Hi != nil {
		i, null, err := intBound(x.Hi, env)
		if err != nil || null {
			return value.Null, err
		}
		hi = i
	}
	lo, hi = clampSlice(lo, hi, n)
	return value.List(append([]value.Value(nil), lst[lo:hi]...)...), nil
}

func intBound(e ast.Expr, env *Env) (n int64, null bool, err error) {
	v, err := Eval(e, env)
	if err != nil {
		return 0, false, err
	}
	if v.IsNull() {
		return 0, true, nil
	}
	i, ok := v.AsInt()
	if !ok {
		return 0, false, fmt.Errorf("eval: slice bound must be an integer, got %s", v.Type())
	}
	return i, false, nil
}

func clampSlice(lo, hi, n int64) (int64, int64) {
	if lo < 0 {
		lo += n
	}
	if hi < 0 {
		hi += n
	}
	if lo < 0 {
		lo = 0
	}
	if lo > n {
		lo = n
	}
	if hi > n {
		hi = n
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// evalCase evaluates the simple form (CASE x WHEN v …, matched by equality) and
// the searched form (CASE WHEN cond …, matched by a true predicate), falling to
// ELSE or null.
func evalCase(x *ast.Case, env *Env) (value.Value, error) {
	if x.Subject != nil {
		subj, err := Eval(x.Subject, env)
		if err != nil {
			return value.Null, err
		}
		for _, w := range x.Whens {
			cmp, err := Eval(w.When, env)
			if err != nil {
				return value.Null, err
			}
			if truthy(eq3(subj, cmp)) {
				return Eval(w.Then, env)
			}
		}
	} else {
		for _, w := range x.Whens {
			cond, err := Eval(w.When, env)
			if err != nil {
				return value.Null, err
			}
			if truthy(cond) {
				return Eval(w.Then, env)
			}
		}
	}
	if x.Else != nil {
		return Eval(x.Else, env)
	}
	return value.Null, nil
}

// truthy reports whether a value is definitely the boolean true (null and
// non-booleans are not).
func truthy(v value.Value) bool {
	b, ok := v.AsBool()
	return ok && b
}

func evalCall(x *ast.FunctionCall, env *Env) (value.Value, error) {
	if x.Star {
		return value.Null, fmt.Errorf("eval: %s(*) is an aggregate, not valid in a scalar position", x.Name)
	}
	name := strings.ToLower(x.Name)
	fn, ok := scalarFuncs[name]
	if !ok {
		return value.Null, fmt.Errorf("eval: unknown function %s", x.Name)
	}
	args := make([]value.Value, len(x.Args))
	for i, a := range x.Args {
		v, err := Eval(a, env)
		if err != nil {
			return value.Null, err
		}
		args[i] = v
	}
	return fn(args, env)
}

// --- string predicates and regex ---

func stringPred(op ast.BinaryOp, l, r value.Value) value.Value {
	if l.IsNull() || r.IsNull() {
		return value.Null
	}
	ls, lok := l.AsString()
	rs, rok := r.AsString()
	if !lok || !rok {
		return value.Null
	}
	switch op {
	case ast.OpStartsWith:
		return value.Bool(strings.HasPrefix(ls, rs))
	case ast.OpEndsWith:
		return value.Bool(strings.HasSuffix(ls, rs))
	case ast.OpContains:
		return value.Bool(strings.Contains(ls, rs))
	}
	return value.Null
}

// regexMatch implements =~, which matches the whole string (the pattern is
// implicitly anchored). The pattern is compiled per call; a plan-level regex
// cache is a later optimization.
func regexMatch(l, r value.Value) (value.Value, error) {
	if l.IsNull() || r.IsNull() {
		return value.Null, nil
	}
	s, sok := l.AsString()
	p, pok := r.AsString()
	if !sok || !pok {
		return value.Null, nil
	}
	re, err := regexp.Compile("^(?:" + p + ")$")
	if err != nil {
		return value.Null, fmt.Errorf("eval: invalid regular expression %q: %w", p, err)
	}
	return value.Bool(re.MatchString(s)), nil
}

// inList implements x IN list under three-valued logic: a found element is true,
// otherwise null if either the list is null or it holds a null (the membership is
// then indeterminate), else false.
func inList(l, r value.Value) (value.Value, error) {
	if r.IsNull() {
		return value.Null, nil
	}
	lst, ok := r.AsList()
	if !ok {
		return value.Null, fmt.Errorf("eval: IN requires a list, got %s", r.Type())
	}
	if l.IsNull() {
		if len(lst) == 0 {
			return value.Bool(false), nil
		}
		return value.Null, nil
	}
	sawNull := false
	for _, e := range lst {
		if e.IsNull() {
			sawNull = true
			continue
		}
		if valuesEqual(l, e) {
			return value.Bool(true), nil
		}
	}
	if sawNull {
		return value.Null, nil
	}
	return value.Bool(false), nil
}
