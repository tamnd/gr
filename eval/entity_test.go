package eval

import (
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/value"
)

// entityEnv builds an Env over a fakeTx wired with one node (labels Person/Admin,
// props name/age) and one relationship (type KNOWS, prop since), plus the reverse
// resolvers the entity functions read.
func entityEnv() *Env {
	const (
		tokName  = engine.Token(10)
		tokAge   = engine.Token(11)
		tokSince = engine.Token(12)
		labPers  = engine.Token(20)
		labAdmin = engine.Token(21)
		relKnows = engine.Token(30)
	)
	tx := &fakeTx{
		nodeProps: map[propKey]value.Value{
			{1, tokName}: value.String("Alice"),
			{1, tokAge}:  value.Int(30),
		},
		relProps: map[propKey]value.Value{
			{2, tokSince}: value.Int(2020),
		},
		nodeLabels: map[uint64][]engine.Token{1: {labPers, labAdmin}},
		relTypes:   map[uint64]engine.Token{2: relKnows},
	}
	labelName := map[engine.Token]string{labPers: "Person", labAdmin: "Admin"}
	relTypeName := map[engine.Token]string{relKnows: "KNOWS"}
	propKeyName := map[engine.Token]string{tokName: "name", tokAge: "age", tokSince: "since"}
	return &Env{
		Row:         Row{"n": value.Node(1), "r": value.Rel(2)},
		Params:      map[string]value.Value{},
		Tx:          tx,
		LabelName:   func(t engine.Token) (string, bool) { s, ok := labelName[t]; return s, ok },
		RelTypeName: func(t engine.Token) (string, bool) { s, ok := relTypeName[t]; return s, ok },
		PropKeyName: func(t engine.Token) (string, bool) { s, ok := propKeyName[t]; return s, ok },
	}
}

// mustExpr parses a bare expression (wrapped in RETURN) and returns it unevaluated,
// for tests that assert on the evaluation error.
func mustExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	q, err := parse.Parse("RETURN " + src + " AS x")
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	return q.First.Clauses[0].(*ast.Return).Items[0].Expr
}

func TestLabels(t *testing.T) {
	e := entityEnv()
	v := evalExpr(t, "labels(n)", e)
	l, ok := v.AsList()
	if !ok {
		t.Fatalf("labels(n): got %s, want list", v)
	}
	got := map[string]bool{}
	for _, x := range l {
		s, _ := x.AsString()
		got[s] = true
	}
	if !got["Person"] || !got["Admin"] || len(got) != 2 {
		t.Fatalf("labels(n): got %v, want {Person, Admin}", got)
	}
	wantNull(t, "labels(null)", e)
}

func TestType(t *testing.T) {
	e := entityEnv()
	wantStr(t, "type(r)", e, "KNOWS")
	wantNull(t, "type(null)", e)
}

func TestKeys(t *testing.T) {
	e := entityEnv()
	v := evalExpr(t, "keys(n)", e)
	l, ok := v.AsList()
	if !ok || len(l) != 2 {
		t.Fatalf("keys(n): got %s, want 2-element list", v)
	}
	// keys() sorts its result for determinism.
	if s, _ := l[0].AsString(); s != "age" {
		t.Fatalf("keys(n)[0]: got %s, want age", l[0])
	}
	if s, _ := l[1].AsString(); s != "name" {
		t.Fatalf("keys(n)[1]: got %s, want name", l[1])
	}
	wantNull(t, "keys(null)", e)
}

func TestProperties(t *testing.T) {
	e := entityEnv()
	v := evalExpr(t, "properties(n)", e)
	m, ok := v.AsMap()
	if !ok {
		t.Fatalf("properties(n): got %s, want map", v)
	}
	if s, _ := m["name"].AsString(); s != "Alice" {
		t.Fatalf("properties(n).name: got %s, want Alice", m["name"])
	}
	if i, _ := m["age"].AsInt(); i != 30 {
		t.Fatalf("properties(n).age: got %s, want 30", m["age"])
	}
	rv := evalExpr(t, "properties(r)", e)
	rm, ok := rv.AsMap()
	if !ok {
		t.Fatalf("properties(r): got %s, want map", rv)
	}
	if i, _ := rm["since"].AsInt(); i != 2020 {
		t.Fatalf("properties(r).since: got %s, want 2020", rm["since"])
	}
	wantNull(t, "properties(null)", e)
}

func TestPropertiesOfMap(t *testing.T) {
	e := entityEnv()
	// properties() on a literal map is the identity.
	v := evalExpr(t, "properties({a: 1, b: 2})", e)
	m, ok := v.AsMap()
	if !ok || len(m) != 2 {
		t.Fatalf("properties(map): got %s, want 2-key map", v)
	}
}

func TestEntityTypeMisuse(t *testing.T) {
	e := entityEnv()
	for _, src := range []string{"labels(r)", "type(n)", "keys(1)", "properties(1)"} {
		q := mustExpr(t, src)
		if _, err := Eval(q, e); err == nil {
			t.Fatalf("%s: want type-misuse error, got none", src)
		}
	}
}
