package parse_test

import (
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/parse"
)

func TestParseCreateConstraint(t *testing.T) {
	q, err := parse.Parse("CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE")
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := q.Schema.(*ast.CreateConstraint)
	if !ok {
		t.Fatalf("Schema is %T, want *ast.CreateConstraint", q.Schema)
	}
	if cc.Name != "" || cc.IfNotExists {
		t.Fatalf("unexpected name/ifnotexists: %q %v", cc.Name, cc.IfNotExists)
	}
	if cc.Var != "p" || cc.Label != "Person" {
		t.Fatalf("var/label = %q/%q", cc.Var, cc.Label)
	}
	if len(cc.Props) != 1 || cc.Props[0] != "email" {
		t.Fatalf("props = %v", cc.Props)
	}
	if q.First != nil {
		t.Fatal("schema command should leave First nil")
	}
}

func TestParseCreateConstraintNamedIfNotExists(t *testing.T) {
	q, err := parse.Parse("CREATE CONSTRAINT person_email IF NOT EXISTS FOR (p:Person) REQUIRE p.email IS UNIQUE")
	if err != nil {
		t.Fatal(err)
	}
	cc := q.Schema.(*ast.CreateConstraint)
	if cc.Name != "person_email" {
		t.Fatalf("name = %q", cc.Name)
	}
	if !cc.IfNotExists {
		t.Fatal("IfNotExists not set")
	}
}

func TestParseDropConstraint(t *testing.T) {
	q, err := parse.Parse("DROP CONSTRAINT person_email")
	if err != nil {
		t.Fatal(err)
	}
	dc, ok := q.Schema.(*ast.DropConstraint)
	if !ok {
		t.Fatalf("Schema is %T, want *ast.DropConstraint", q.Schema)
	}
	if dc.Name != "person_email" || dc.IfExists {
		t.Fatalf("name/ifexists = %q/%v", dc.Name, dc.IfExists)
	}
}

func TestParseDropConstraintIfExists(t *testing.T) {
	q, err := parse.Parse("DROP CONSTRAINT c IF EXISTS")
	if err != nil {
		t.Fatal(err)
	}
	dc := q.Schema.(*ast.DropConstraint)
	if !dc.IfExists {
		t.Fatal("IfExists not set")
	}
}

// TestParseConstraintSoftKeywords confirms the schema grammar's soft keywords are
// still usable as ordinary names in a normal query, since the parser never
// reserved them.
func TestParseConstraintSoftKeywords(t *testing.T) {
	for _, src := range []string{
		"MATCH (constraint) RETURN constraint",
		"MATCH (n:Require) RETURN n",
		"MATCH (n) WHERE n.unique = 1 RETURN n",
	} {
		if _, err := parse.Parse(src); err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
	}
}

func TestParseConstraintErrors(t *testing.T) {
	bad := []string{
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE q.email IS UNIQUE",       // wrong variable
		"CREATE CONSTRAINT FOR p:Person REQUIRE p.email IS UNIQUE",         // missing parens
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS",              // missing UNIQUE
		"CREATE CONSTRAINT FOR (p:Person) p.email IS UNIQUE",               // missing REQUIRE
		"CREATE CONSTRAINT IF EXISTS FOR (p:Person) REQUIRE p.x IS UNIQUE", // IF without NOT
		"DROP CONSTRAINT",      // missing name
		"DROP CONSTRAINT c IF", // dangling IF
	}
	for _, src := range bad {
		if _, err := parse.Parse(src); err == nil {
			t.Fatalf("Parse(%q): expected an error, got none", src)
		}
	}
}
