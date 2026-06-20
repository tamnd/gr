package lex_test

import (
	"testing"

	"github.com/tamnd/gr/lex"
)

// kinds drains a query to its token kinds (dropping the trailing EOF) and fails
// the test on any lexical error.
func kinds(t *testing.T, src string) []lex.Kind {
	t.Helper()
	toks, err := lex.Tokens(src)
	if err != nil {
		t.Fatalf("Tokens(%q): %v", src, err)
	}
	if len(toks) == 0 || toks[len(toks)-1].Kind != lex.EOF {
		t.Fatalf("Tokens(%q): missing EOF terminator", src)
	}
	out := make([]lex.Kind, len(toks)-1)
	for i := range out {
		out[i] = toks[i].Kind
	}
	return out
}

func eq(t *testing.T, got, want []lex.Kind, src string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%q: got %d tokens %v, want %d %v", src, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%q: token %d = %v, want %v", src, i, got[i], want[i])
		}
	}
}

// TestReadQuery lexes a representative read query and checks the full kind
// sequence, the friends-of-friends shape from the M2 demo.
func TestReadQuery(t *testing.T) {
	src := `MATCH (a:Person {name:$name})-[:KNOWS]->(:Person)-[r:KNOWS]->(fof:Person)
	        WHERE fof <> a
	        RETURN DISTINCT fof.name AS suggestion`
	want := []lex.Kind{
		lex.Match, lex.Lparen, lex.Ident, lex.Colon, lex.Ident, lex.Lbrace,
		lex.Ident, lex.Colon, lex.Param, lex.Rbrace, lex.Rparen,
		lex.Minus, lex.Lbracket, lex.Colon, lex.Ident, lex.Rbracket, lex.Minus, lex.Gt,
		lex.Lparen, lex.Colon, lex.Ident, lex.Rparen,
		lex.Minus, lex.Lbracket, lex.Ident, lex.Colon, lex.Ident, lex.Rbracket, lex.Minus, lex.Gt,
		lex.Lparen, lex.Ident, lex.Colon, lex.Ident, lex.Rparen,
		lex.Where, lex.Ident, lex.Ne, lex.Ident,
		lex.Return, lex.Distinct, lex.Ident, lex.Dot, lex.Ident, lex.As, lex.Ident,
	}
	eq(t, kinds(t, src), want, src)
}

// TestKeywordsCaseInsensitive confirms keywords fold case while their Text keeps
// the original spelling.
func TestKeywordsCaseInsensitive(t *testing.T) {
	toks, err := lex.Tokens("MaTcH mAtCh return")
	if err != nil {
		t.Fatal(err)
	}
	if toks[0].Kind != lex.Match || toks[1].Kind != lex.Match || toks[2].Kind != lex.Return {
		t.Fatalf("case folding failed: %v %v %v", toks[0].Kind, toks[1].Kind, toks[2].Kind)
	}
	if toks[0].Text != "MaTcH" {
		t.Fatalf("keyword Text = %q, want original spelling MaTcH", toks[0].Text)
	}
}

// TestIdentifiersCaseSensitive confirms identifiers do not fold and that
// backtick quoting allows arbitrary characters and never yields a keyword.
func TestIdentifiersCaseSensitive(t *testing.T) {
	toks, err := lex.Tokens("Person person `weird name` `match` `a``b`")
	if err != nil {
		t.Fatal(err)
	}
	if toks[0].Kind != lex.Ident || toks[0].Text != "Person" {
		t.Fatalf("Person: %v %q", toks[0].Kind, toks[0].Text)
	}
	if toks[1].Kind != lex.Ident || toks[1].Text != "person" {
		t.Fatalf("person: %v %q", toks[1].Kind, toks[1].Text)
	}
	if toks[2].Kind != lex.Ident || toks[2].Text != "weird name" {
		t.Fatalf("quoted: %v %q", toks[2].Kind, toks[2].Text)
	}
	if toks[3].Kind != lex.Ident || toks[3].Text != "match" {
		t.Fatalf("quoted keyword stays ident: %v %q", toks[3].Kind, toks[3].Text)
	}
	if toks[4].Kind != lex.Ident || toks[4].Text != "a`b" {
		t.Fatalf("doubled backtick: %v %q", toks[4].Kind, toks[4].Text)
	}
}

// TestNumbers covers integer, hex, octal, and float literal forms.
func TestNumbers(t *testing.T) {
	cases := []struct {
		src  string
		kind lex.Kind
		text string
	}{
		{"42", lex.Int, "42"},
		{"0x2A", lex.Int, "0x2A"},
		{"0o52", lex.Int, "0o52"},
		{"3.14", lex.Float, "3.14"},
		{"1e10", lex.Float, "1e10"},
		{"6.022e23", lex.Float, "6.022e23"},
		{".5", lex.Float, ".5"},
		{"10e-3", lex.Float, "10e-3"},
	}
	for _, c := range cases {
		toks, err := lex.Tokens(c.src)
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if toks[0].Kind != c.kind || toks[0].Text != c.text {
			t.Fatalf("%q: got (%v,%q), want (%v,%q)", c.src, toks[0].Kind, toks[0].Text, c.kind, c.text)
		}
	}
}

// TestNumberThenRange checks that 1..3 lexes as Int DotDot Int, not a float, so
// variable-length ranges scan correctly.
func TestNumberThenRange(t *testing.T) {
	eq(t, kinds(t, "1..3"), []lex.Kind{lex.Int, lex.DotDot, lex.Int}, "1..3")
}

// TestStringEscapes resolves escape sequences into the decoded Text.
func TestStringEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`'plain'`, "plain"},
		{`"double"`, "double"},
		{`'tab\there'`, "tab\there"},
		{`'new\nline'`, "new\nline"},
		{`'quote\'s'`, "quote's"},
		{`'A'`, "A"},
		{`'café'`, "café"},
	}
	for _, c := range cases {
		toks, err := lex.Tokens(c.src)
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if toks[0].Kind != lex.String || toks[0].Text != c.want {
			t.Fatalf("%q: got (%v,%q), want String %q", c.src, toks[0].Kind, toks[0].Text, c.want)
		}
	}
}

// TestParameters lexes named and positional parameters, stripping the dollar.
func TestParameters(t *testing.T) {
	toks, err := lex.Tokens("$name $0 $`odd one`")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"name", "0", "odd one"} {
		if toks[i].Kind != lex.Param || toks[i].Text != want {
			t.Fatalf("param %d = (%v,%q), want Param %q", i, toks[i].Kind, toks[i].Text, want)
		}
	}
}

// TestComments confirms line and block comments are skipped.
func TestComments(t *testing.T) {
	src := "MATCH // a line comment\n (n) /* block\n spanning */ RETURN n"
	want := []lex.Kind{lex.Match, lex.Lparen, lex.Ident, lex.Rparen, lex.Return, lex.Ident}
	eq(t, kinds(t, src), want, src)
}

// TestOperators covers the multi-character comparison operators.
func TestOperators(t *testing.T) {
	eq(t, kinds(t, "< <= > >= <> = + - * / % ^"),
		[]lex.Kind{lex.Lt, lex.Le, lex.Gt, lex.Ge, lex.Ne, lex.Eq,
			lex.Plus, lex.Minus, lex.Star, lex.Slash, lex.Percent, lex.Caret}, "operators")
}

// TestPositions checks that tokens carry their 1-based line and column.
func TestPositions(t *testing.T) {
	toks, err := lex.Tokens("MATCH\n  (n)")
	if err != nil {
		t.Fatal(err)
	}
	if toks[0].Line != 1 || toks[0].Col != 1 {
		t.Fatalf("MATCH at %d:%d, want 1:1", toks[0].Line, toks[0].Col)
	}
	if toks[1].Line != 2 || toks[1].Col != 3 {
		t.Fatalf("'(' at %d:%d, want 2:3", toks[1].Line, toks[1].Col)
	}
}

// TestLexErrors confirms malformed tokens are rejected with a positioned error,
// not repaired.
func TestLexErrors(t *testing.T) {
	bad := []string{
		`'unterminated`,
		"`unterminated ident",
		"/* unterminated block",
		`'bad \q escape'`,
		"0x",
		"#",
		"$",
	}
	for _, src := range bad {
		if _, err := lex.Tokens(src); err == nil {
			t.Fatalf("%q: expected a lexical error, got none", src)
		} else if _, ok := err.(*lex.Error); !ok {
			t.Fatalf("%q: error type = %T, want *lex.Error", src, err)
		}
	}
}
