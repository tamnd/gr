package eval

import (
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/value"
)

// evalExpr parses a bare expression (wrapped in RETURN so the parser accepts it),
// extracts the single projected expression, and evaluates it against env.
func evalExpr(t *testing.T, src string, env *Env) value.Value {
	t.Helper()
	q, err := parse.Parse("RETURN " + src + " AS x")
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	e := q.First.Clauses[0].(*ast.Return).Items[0].Expr
	v, err := Eval(e, env)
	if err != nil {
		t.Fatalf("eval(%q): %v", src, err)
	}
	return v
}

func env() *Env { return &Env{Row: Row{}, Params: map[string]value.Value{}} }

func wantBool(t *testing.T, src string, e *Env, want bool) {
	t.Helper()
	v := evalExpr(t, src, e)
	b, ok := v.AsBool()
	if !ok || b != want {
		t.Fatalf("%s: got %s, want %v", src, v, want)
	}
}

func wantNull(t *testing.T, src string, e *Env) {
	t.Helper()
	v := evalExpr(t, src, e)
	if !v.IsNull() {
		t.Fatalf("%s: got %s, want null", src, v)
	}
}

func wantInt(t *testing.T, src string, e *Env, want int64) {
	t.Helper()
	v := evalExpr(t, src, e)
	i, ok := v.AsInt()
	if !ok || i != want {
		t.Fatalf("%s: got %s, want %d", src, v, want)
	}
}

func wantStr(t *testing.T, src string, e *Env, want string) {
	t.Helper()
	v := evalExpr(t, src, e)
	s, ok := v.AsString()
	if !ok || s != want {
		t.Fatalf("%s: got %s, want %q", src, v, want)
	}
}

func TestArithmetic(t *testing.T) {
	e := env()
	wantInt(t, "1 + 2 * 3", e, 7)
	wantInt(t, "10 % 3", e, 1)
	wantInt(t, "-(3 - 5)", e, 2)
	v := evalExpr(t, "7 / 2.0", e)
	if f, _ := v.AsFloat(); f != 3.5 {
		t.Fatalf("7 / 2.0: got %s", v)
	}
	wantInt(t, "7 / 2", e, 3) // integer division truncates
}

func TestNumericEquality(t *testing.T) {
	e := env()
	wantBool(t, "1 = 1.0", e, true) // cross-type numeric equality
	wantBool(t, "1 <> 2", e, true)
	wantBool(t, "2 < 10", e, true)
	wantBool(t, `"a" < "b"`, e, true) // string order by code point
	wantBool(t, `1 = "1"`, e, false)  // cross-type non-numeric is not equal
}

func TestThreeValuedComparison(t *testing.T) {
	e := env()
	wantNull(t, "null = 1", e)
	wantNull(t, "null < 1", e)
	wantNull(t, "1 < null", e)
}

func TestKleeneLogic(t *testing.T) {
	e := env()
	wantBool(t, "false AND null", e, false) // false dominates AND
	wantNull(t, "true AND null", e)
	wantBool(t, "true OR null", e, true) // true dominates OR
	wantNull(t, "false OR null", e)
	wantNull(t, "NOT null", e)
	wantBool(t, "NOT (1 = 1)", e, false)
}

func TestIsNull(t *testing.T) {
	e := env()
	wantBool(t, "null IS NULL", e, true)
	wantBool(t, "1 IS NOT NULL", e, true)
	wantBool(t, "null IS NOT NULL", e, false) // never yields null itself
}

func TestStringPredicates(t *testing.T) {
	e := env()
	wantBool(t, `"hello" STARTS WITH "he"`, e, true)
	wantBool(t, `"hello" ENDS WITH "lo"`, e, true)
	wantBool(t, `"hello" CONTAINS "ell"`, e, true)
}

func TestInList(t *testing.T) {
	e := env()
	wantBool(t, "2 IN [1, 2, 3]", e, true)
	wantBool(t, "9 IN [1, 2, 3]", e, false)
	wantNull(t, "9 IN [1, null, 3]", e) // unmatched with a null present is null
	wantBool(t, "2 IN [1, null, 2]", e, true)
	wantBool(t, "null IN []", e, false)
}

func TestConcatAndCoerce(t *testing.T) {
	e := env()
	wantStr(t, `"a" + "b"`, e, "ab")
	wantStr(t, `"n=" + 1`, e, "n=1") // number coerced for string concat
	v := evalExpr(t, "[1, 2] + 3", e)
	l, _ := v.AsList()
	if len(l) != 3 {
		t.Fatalf("list concat: got %s", v)
	}
}

func TestParamsAndVariables(t *testing.T) {
	e := &Env{
		Row:    Row{"n": value.Int(42)},
		Params: map[string]value.Value{"p": value.String("hi")},
	}
	wantInt(t, "n", e, 42)
	wantStr(t, "$p", e, "hi")
	wantInt(t, "n + 8", e, 50)
}

func TestIndexAndSlice(t *testing.T) {
	e := env()
	wantInt(t, "[10, 20, 30][1]", e, 20)
	wantInt(t, "[10, 20, 30][-1]", e, 30) // negative index from the end
	wantNull(t, "[10, 20, 30][9]", e)     // out of range is null
	v := evalExpr(t, "[1, 2, 3, 4][1..3]", e)
	l, _ := v.AsList()
	if len(l) != 2 {
		t.Fatalf("slice: got %s", v)
	}
	wantInt(t, `{a: 1, b: 2}.b`, e, 2)
	wantInt(t, `{a: 1, b: 2}["a"]`, e, 1)
}

func TestCase(t *testing.T) {
	e := env()
	wantStr(t, `CASE 2 WHEN 1 THEN "one" WHEN 2 THEN "two" ELSE "other" END`, e, "two")
	wantStr(t, `CASE WHEN 1 > 2 THEN "no" WHEN 2 > 1 THEN "yes" END`, e, "yes")
	wantNull(t, `CASE WHEN false THEN "no" END`, e) // no branch, no ELSE
}

func TestFunctions(t *testing.T) {
	e := env()
	wantInt(t, "abs(-5)", e, 5)
	wantInt(t, "size([1, 2, 3])", e, 3)
	wantInt(t, `size("héllo")`, e, 5) // rune count
	wantInt(t, "head([7, 8, 9])", e, 7)
	wantInt(t, "last([7, 8, 9])", e, 9)
	wantStr(t, `toUpper("ab")`, e, "AB")
	wantStr(t, `substring("hello", 1, 3)`, e, "ell")
	wantStr(t, `replace("a.b.c", ".", "-")`, e, "a-b-c")
	wantInt(t, "coalesce(null, null, 3)", e, 3)
	wantInt(t, `toInteger("42")`, e, 42)
	wantNull(t, `toInteger("nope")`, e) // unparseable is null, not an error
	wantInt(t, `toInteger(3.9)`, e, 3)  // truncates
	v := evalExpr(t, "range(1, 5, 2)", e)
	l, _ := v.AsList()
	if len(l) != 3 {
		t.Fatalf("range: got %s", v)
	}
}

func TestNullPropagation(t *testing.T) {
	e := env()
	wantNull(t, "1 + null", e)
	wantNull(t, "abs(null)", e)
	wantNull(t, `toUpper(null)`, e)
	wantNull(t, "null[0]", e)
}

func TestEntityProperty(t *testing.T) {
	tx := &fakeTx{
		nodeProps: map[propKey]value.Value{
			{1, 10}: value.String("Alice"),
		},
		relProps: map[propKey]value.Value{
			{2, 11}: value.Int(2020),
		},
	}
	resolve := func(name string) (engine.Token, bool) {
		switch name {
		case "name":
			return 10, true
		case "since":
			return 11, true
		}
		return 0, false // unknown key: schema-optional null
	}
	e := &Env{
		Row:     Row{"n": value.Node(1), "r": value.Rel(2)},
		Params:  map[string]value.Value{},
		Tx:      tx,
		Resolve: resolve,
	}
	wantStr(t, "n.name", e, "Alice")
	wantInt(t, "r.since", e, 2020)
	wantNull(t, "n.missing", e) // unknown property key resolves to null
}

func TestEntityEquality(t *testing.T) {
	e := &Env{
		Row:    Row{"a": value.Node(1), "b": value.Node(1), "c": value.Node(2)},
		Params: map[string]value.Value{},
	}
	wantBool(t, "a = b", e, true)  // same element id
	wantBool(t, "a = c", e, false) // different id
	wantInt(t, "id(a)", e, 1)
}

func TestOrderTotalOrder(t *testing.T) {
	// null sorts last, NaN sorts as the greatest number, ints and floats interleave.
	nan := value.Float(naN())
	cases := []struct {
		l, r value.Value
		want int
	}{
		{value.Int(1), value.Float(1.5), -1},
		{value.Int(2), value.Float(1.5), 1},
		{nan, value.Float(1e308), 1}, // NaN greatest among numbers
		{value.Null, value.Int(0), 1},
		{value.String("a"), value.Int(0), 1}, // string ranks after number
		{value.Int(5), value.Int(5), 0},
	}
	for _, c := range cases {
		if got := Order(c.l, c.r); sign(got) != c.want {
			t.Fatalf("Order(%s, %s) = %d, want sign %d", c.l, c.r, got, c.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func naN() float64 {
	z := 0.0
	return z / z
}
