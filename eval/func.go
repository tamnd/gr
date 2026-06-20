package eval

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tamnd/gr/value"
)

// scalarFunc is a read-side scalar (non-aggregate) function: it maps already-
// evaluated arguments to a value, with the environment available for the few that
// need it. Aggregates (count, sum, collect …) are not here; they are computed by
// the Aggregate operator over a row stream (doc 09 §8).
type scalarFunc func(args []value.Value, env *Env) (value.Value, error)

// scalarFuncs is the registered read-side function set (doc 09 §7). The entity
// and path functions (labels, type, keys, properties, nodes, relationships) live
// in entity.go; they read through the Env's snapshot and reverse name resolvers.
var scalarFuncs = map[string]scalarFunc{
	"coalesce":      fnCoalesce,
	"id":            fnID,
	"exists":        fnExists,
	"size":          fnSize,
	"length":        fnLength,
	"head":          fnHead,
	"last":          fnLast,
	"tail":          fnTail,
	"reverse":       fnReverse,
	"range":         fnRange,
	"tostring":      fnToString,
	"tointeger":     fnToInteger,
	"tofloat":       fnToFloat,
	"toboolean":     fnToBoolean,
	"abs":           fnAbs,
	"sign":          fnSign,
	"ceil":          floatFn(math.Ceil),
	"floor":         floatFn(math.Floor),
	"round":         floatFn(math.Round),
	"sqrt":          floatFn(math.Sqrt),
	"toupper":       strFn(strings.ToUpper),
	"tolower":       strFn(strings.ToLower),
	"trim":          strFn(strings.TrimSpace),
	"ltrim":         strFn(func(s string) string { return strings.TrimLeft(s, " \t\r\n") }),
	"rtrim":         strFn(func(s string) string { return strings.TrimRight(s, " \t\r\n") }),
	"substring":     fnSubstring,
	"replace":       fnReplace,
	"split":         fnSplit,
	"labels":        fnLabels,
	"type":          fnType,
	"keys":          fnKeys,
	"properties":    fnProperties,
	"nodes":         fnNodes,
	"relationships": fnRelationships,
}

// strFn lifts a string-to-string function into a scalar function with null
// propagation and a single string argument.
func strFn(f func(string) string) scalarFunc {
	return func(args []value.Value, _ *Env) (value.Value, error) {
		if len(args) != 1 {
			return value.Null, fmt.Errorf("eval: function expects 1 argument, got %d", len(args))
		}
		if args[0].IsNull() {
			return value.Null, nil
		}
		s, ok := args[0].AsString()
		if !ok {
			return value.Null, fmt.Errorf("eval: function requires a string, got %s", args[0].Type())
		}
		return value.String(f(s)), nil
	}
}

// floatFn lifts a float-to-float function into a scalar function with null
// propagation and a single numeric argument.
func floatFn(f func(float64) float64) scalarFunc {
	return func(args []value.Value, _ *Env) (value.Value, error) {
		if len(args) != 1 {
			return value.Null, fmt.Errorf("eval: function expects 1 argument, got %d", len(args))
		}
		if args[0].IsNull() {
			return value.Null, nil
		}
		x, ok := args[0].AsFloat()
		if !ok {
			return value.Null, fmt.Errorf("eval: function requires a number, got %s", args[0].Type())
		}
		return value.Float(f(x)), nil
	}
}

func fnCoalesce(args []value.Value, _ *Env) (value.Value, error) {
	for _, a := range args {
		if !a.IsNull() {
			return a, nil
		}
	}
	return value.Null, nil
}

func fnID(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("id", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	if id, ok := a.AsNode(); ok {
		return value.Int(int64(id)), nil
	}
	if id, ok := a.AsRel(); ok {
		return value.Int(int64(id)), nil
	}
	return value.Null, fmt.Errorf("eval: id requires a node or relationship, got %s", a.Type())
}

func fnExists(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("exists", args, 1); err != nil {
		return value.Null, err
	}
	return value.Bool(!args[0].IsNull()), nil
}

func fnSize(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("size", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	if l, ok := a.AsList(); ok {
		return value.Int(int64(len(l))), nil
	}
	if s, ok := a.AsString(); ok {
		return value.Int(int64(len([]rune(s)))), nil
	}
	if m, ok := a.AsMap(); ok {
		return value.Int(int64(len(m))), nil
	}
	return value.Null, fmt.Errorf("eval: size requires a list, string, or map, got %s", a.Type())
}

func fnHead(args []value.Value, _ *Env) (value.Value, error) {
	l, null, err := listArg("head", args)
	if err != nil || null {
		return value.Null, err
	}
	if len(l) == 0 {
		return value.Null, nil
	}
	return l[0], nil
}

func fnLast(args []value.Value, _ *Env) (value.Value, error) {
	l, null, err := listArg("last", args)
	if err != nil || null {
		return value.Null, err
	}
	if len(l) == 0 {
		return value.Null, nil
	}
	return l[len(l)-1], nil
}

func fnTail(args []value.Value, _ *Env) (value.Value, error) {
	l, null, err := listArg("tail", args)
	if err != nil || null {
		return value.Null, err
	}
	if len(l) <= 1 {
		return value.List(), nil
	}
	return value.List(l[1:]...), nil
}

func fnReverse(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("reverse", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	if s, ok := a.AsString(); ok {
		r := []rune(s)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return value.String(string(r)), nil
	}
	if l, ok := a.AsList(); ok {
		out := make([]value.Value, len(l))
		for i := range l {
			out[len(l)-1-i] = l[i]
		}
		return value.List(out...), nil
	}
	return value.Null, fmt.Errorf("eval: reverse requires a string or list, got %s", a.Type())
}

// fnRange builds an inclusive integer range with an optional step (doc 09 §7).
func fnRange(args []value.Value, _ *Env) (value.Value, error) {
	if len(args) != 2 && len(args) != 3 {
		return value.Null, fmt.Errorf("eval: range expects 2 or 3 arguments, got %d", len(args))
	}
	start, ok := args[0].AsInt()
	if !ok {
		return value.Null, fmt.Errorf("eval: range start must be an integer, got %s", args[0].Type())
	}
	end, ok := args[1].AsInt()
	if !ok {
		return value.Null, fmt.Errorf("eval: range end must be an integer, got %s", args[1].Type())
	}
	step := int64(1)
	if len(args) == 3 {
		s, ok := args[2].AsInt()
		if !ok {
			return value.Null, fmt.Errorf("eval: range step must be an integer, got %s", args[2].Type())
		}
		step = s
	}
	if step == 0 {
		return value.Null, fmt.Errorf("eval: range step must be non-zero")
	}
	var out []value.Value
	if step > 0 {
		for i := start; i <= end; i += step {
			out = append(out, value.Int(i))
		}
	} else {
		for i := start; i >= end; i += step {
			out = append(out, value.Int(i))
		}
	}
	return value.List(out...), nil
}

func fnToString(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("toString", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	switch a.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeString:
		return a, nil
	case value.TypeInt, value.TypeFloat, value.TypeBool:
		return value.String(a.String()), nil
	default:
		return value.Null, fmt.Errorf("eval: toString cannot convert %s", a.Type())
	}
}

// fnToInteger converts to an integer, returning null for an unparseable string
// (the Cypher behaviour) rather than erroring.
func fnToInteger(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("toInteger", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	switch a.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeInt:
		return a, nil
	case value.TypeFloat:
		f, _ := a.AsFloat()
		return value.Int(int64(math.Trunc(f))), nil
	case value.TypeString:
		s, _ := a.AsString()
		s = strings.TrimSpace(s)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return value.Int(i), nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return value.Int(int64(math.Trunc(f))), nil
		}
		return value.Null, nil
	default:
		return value.Null, fmt.Errorf("eval: toInteger cannot convert %s", a.Type())
	}
}

func fnToFloat(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("toFloat", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	switch a.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeFloat:
		return a, nil
	case value.TypeInt:
		f, _ := a.AsFloat()
		return value.Float(f), nil
	case value.TypeString:
		s, _ := a.AsString()
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return value.Float(f), nil
		}
		return value.Null, nil
	default:
		return value.Null, fmt.Errorf("eval: toFloat cannot convert %s", a.Type())
	}
}

func fnToBoolean(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("toBoolean", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	switch a.Type() {
	case value.TypeNull:
		return value.Null, nil
	case value.TypeBool:
		return a, nil
	case value.TypeString:
		s, _ := a.AsString()
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return value.Bool(true), nil
		case "false":
			return value.Bool(false), nil
		}
		return value.Null, nil
	default:
		return value.Null, fmt.Errorf("eval: toBoolean cannot convert %s", a.Type())
	}
}

func fnAbs(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("abs", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	if i, ok := a.AsInt(); ok {
		if i < 0 {
			i = -i
		}
		return value.Int(i), nil
	}
	if f, ok := a.AsFloat(); ok {
		return value.Float(math.Abs(f)), nil
	}
	return value.Null, fmt.Errorf("eval: abs requires a number, got %s", a.Type())
}

func fnSign(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("sign", args, 1); err != nil {
		return value.Null, err
	}
	a := args[0]
	if a.IsNull() {
		return value.Null, nil
	}
	f, ok := a.AsFloat()
	if !ok {
		return value.Null, fmt.Errorf("eval: sign requires a number, got %s", a.Type())
	}
	switch {
	case f > 0:
		return value.Int(1), nil
	case f < 0:
		return value.Int(-1), nil
	default:
		return value.Int(0), nil
	}
}

// fnSubstring is substring(s, start[, length]) with 0-based start over runes; an
// omitted length runs to the end, and bounds are clamped to the string.
func fnSubstring(args []value.Value, _ *Env) (value.Value, error) {
	if len(args) != 2 && len(args) != 3 {
		return value.Null, fmt.Errorf("eval: substring expects 2 or 3 arguments, got %d", len(args))
	}
	if args[0].IsNull() {
		return value.Null, nil
	}
	s, ok := args[0].AsString()
	if !ok {
		return value.Null, fmt.Errorf("eval: substring requires a string, got %s", args[0].Type())
	}
	if args[1].IsNull() {
		return value.Null, nil
	}
	start, ok := args[1].AsInt()
	if !ok {
		return value.Null, fmt.Errorf("eval: substring start must be an integer, got %s", args[1].Type())
	}
	r := []rune(s)
	n := int64(len(r))
	if start < 0 {
		start = 0
	}
	if start > n {
		start = n
	}
	end := n
	if len(args) == 3 {
		if args[2].IsNull() {
			return value.Null, nil
		}
		length, ok := args[2].AsInt()
		if !ok {
			return value.Null, fmt.Errorf("eval: substring length must be an integer, got %s", args[2].Type())
		}
		if length < 0 {
			return value.Null, fmt.Errorf("eval: substring length must not be negative")
		}
		end = start + length
		if end > n {
			end = n
		}
	}
	return value.String(string(r[start:end])), nil
}

func fnReplace(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("replace", args, 3); err != nil {
		return value.Null, err
	}
	for _, a := range args {
		if a.IsNull() {
			return value.Null, nil
		}
	}
	s, ok := args[0].AsString()
	if !ok {
		return value.Null, fmt.Errorf("eval: replace requires strings, got %s", args[0].Type())
	}
	search, ok := args[1].AsString()
	if !ok {
		return value.Null, fmt.Errorf("eval: replace requires strings, got %s", args[1].Type())
	}
	repl, ok := args[2].AsString()
	if !ok {
		return value.Null, fmt.Errorf("eval: replace requires strings, got %s", args[2].Type())
	}
	return value.String(strings.ReplaceAll(s, search, repl)), nil
}

func fnSplit(args []value.Value, _ *Env) (value.Value, error) {
	if err := arity("split", args, 2); err != nil {
		return value.Null, err
	}
	if args[0].IsNull() || args[1].IsNull() {
		return value.Null, nil
	}
	s, ok := args[0].AsString()
	if !ok {
		return value.Null, fmt.Errorf("eval: split requires strings, got %s", args[0].Type())
	}
	sep, ok := args[1].AsString()
	if !ok {
		return value.Null, fmt.Errorf("eval: split requires strings, got %s", args[1].Type())
	}
	parts := strings.Split(s, sep)
	out := make([]value.Value, len(parts))
	for i, p := range parts {
		out[i] = value.String(p)
	}
	return value.List(out...), nil
}

// arity checks a fixed argument count.
func arity(name string, args []value.Value, n int) error {
	if len(args) != n {
		return fmt.Errorf("eval: %s expects %d argument(s), got %d", name, n, len(args))
	}
	return nil
}

// listArg reads a single list argument, reporting null=true when the argument is
// null (so the caller returns null).
func listArg(name string, args []value.Value) (list []value.Value, null bool, err error) {
	if e := arity(name, args, 1); e != nil {
		return nil, false, e
	}
	if args[0].IsNull() {
		return nil, true, nil
	}
	l, ok := args[0].AsList()
	if !ok {
		return nil, false, fmt.Errorf("eval: %s requires a list, got %s", name, args[0].Type())
	}
	return l, false, nil
}
