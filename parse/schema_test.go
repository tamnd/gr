package parse_test

import (
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/value"
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

func TestParseCreateConstraintUniqueType(t *testing.T) {
	q, err := parse.Parse("CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE")
	if err != nil {
		t.Fatal(err)
	}
	cc := q.Schema.(*ast.CreateConstraint)
	if cc.Type != ast.ConstraintUnique {
		t.Fatalf("type = %v, want ConstraintUnique", cc.Type)
	}
}

func TestParseCreateExistenceConstraint(t *testing.T) {
	q, err := parse.Parse("CREATE CONSTRAINT person_email IF NOT EXISTS FOR (p:Person) REQUIRE p.email IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := q.Schema.(*ast.CreateConstraint)
	if !ok {
		t.Fatalf("Schema is %T, want *ast.CreateConstraint", q.Schema)
	}
	if cc.Type != ast.ConstraintExists {
		t.Fatalf("type = %v, want ConstraintExists", cc.Type)
	}
	if cc.Name != "person_email" || !cc.IfNotExists {
		t.Fatalf("name/ifnotexists = %q/%v", cc.Name, cc.IfNotExists)
	}
	if cc.Var != "p" || cc.Label != "Person" || len(cc.Props) != 1 || cc.Props[0] != "email" {
		t.Fatalf("var/label/props = %q/%q/%v", cc.Var, cc.Label, cc.Props)
	}
}

func TestParseCreateTypeConstraint(t *testing.T) {
	cases := []struct {
		src  string
		want value.Type
	}{
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", value.TypeInt},
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.name IS :: STRING", value.TypeString},
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.score IS :: FLOAT", value.TypeFloat},
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.active IS :: BOOLEAN", value.TypeBool},
		// The TYPED synonym and lower-case type name are accepted.
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS TYPED INTEGER", value.TypeInt},
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: integer", value.TypeInt},
	}
	for _, c := range cases {
		q, err := parse.Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.src, err)
		}
		cc, ok := q.Schema.(*ast.CreateConstraint)
		if !ok {
			t.Fatalf("Parse(%q): Schema is %T, want *ast.CreateConstraint", c.src, q.Schema)
		}
		if cc.Type != ast.ConstraintPropertyType {
			t.Fatalf("Parse(%q): type = %v, want ConstraintPropertyType", c.src, cc.Type)
		}
		if cc.PropType != c.want {
			t.Fatalf("Parse(%q): proptype = %v, want %v", c.src, cc.PropType, c.want)
		}
	}
}

// TestParseTypeConstraintErrors confirms the type grammar rejects a missing or
// unknown type name.
func TestParseTypeConstraintErrors(t *testing.T) {
	bad := []string{
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS ::",         // missing type name
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS : INTEGER",  // single colon
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: NUMERIC", // unsupported type
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS TYPED",      // TYPED without a type
	}
	for _, src := range bad {
		if _, err := parse.Parse(src); err == nil {
			t.Fatalf("Parse(%q): expected an error, got none", src)
		}
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

func TestParseCreateIndex(t *testing.T) {
	q, err := parse.Parse("CREATE INDEX FOR (p:Person) ON (p.email)")
	if err != nil {
		t.Fatal(err)
	}
	ci, ok := q.Schema.(*ast.CreateIndex)
	if !ok {
		t.Fatalf("Schema is %T, want *ast.CreateIndex", q.Schema)
	}
	if ci.Name != "" || ci.IfNotExists {
		t.Fatalf("unexpected name/ifnotexists: %q %v", ci.Name, ci.IfNotExists)
	}
	if ci.Var != "p" || ci.Label != "Person" {
		t.Fatalf("var/label = %q/%q", ci.Var, ci.Label)
	}
	if len(ci.Props) != 1 || ci.Props[0] != "email" {
		t.Fatalf("props = %v", ci.Props)
	}
	if q.First != nil {
		t.Fatal("schema command should leave First nil")
	}
}

func TestParseCreateIndexNamedIfNotExists(t *testing.T) {
	q, err := parse.Parse("CREATE INDEX person_email IF NOT EXISTS FOR (p:Person) ON (p.email)")
	if err != nil {
		t.Fatal(err)
	}
	ci := q.Schema.(*ast.CreateIndex)
	if ci.Name != "person_email" {
		t.Fatalf("name = %q", ci.Name)
	}
	if !ci.IfNotExists {
		t.Fatal("IfNotExists not set")
	}
}

func TestParseDropIndex(t *testing.T) {
	q, err := parse.Parse("DROP INDEX person_email")
	if err != nil {
		t.Fatal(err)
	}
	di, ok := q.Schema.(*ast.DropIndex)
	if !ok {
		t.Fatalf("Schema is %T, want *ast.DropIndex", q.Schema)
	}
	if di.Name != "person_email" || di.IfExists {
		t.Fatalf("name/ifexists = %q/%v", di.Name, di.IfExists)
	}
}

func TestParseDropIndexIfExists(t *testing.T) {
	q, err := parse.Parse("DROP INDEX i IF EXISTS")
	if err != nil {
		t.Fatal(err)
	}
	di := q.Schema.(*ast.DropIndex)
	if !di.IfExists {
		t.Fatal("IfExists not set")
	}
}

// TestParseIndexErrors confirms the index grammar rejects malformed statements.
func TestParseIndexErrors(t *testing.T) {
	bad := []string{
		"CREATE INDEX FOR (p:Person) ON (q.email)", // wrong variable
		"CREATE INDEX FOR p:Person ON (p.email)",   // missing parens around pattern
		"CREATE INDEX FOR (p:Person) (p.email)",    // missing ON
		"CREATE INDEX FOR (p:Person) ON p.email",   // missing parens around property
		"CREATE INDEX FOR (p:Person) ON (p.email",  // unclosed property parens
		"DROP INDEX",      // missing name
		"DROP INDEX i IF", // dangling IF
	}
	for _, src := range bad {
		if _, err := parse.Parse(src); err == nil {
			t.Fatalf("Parse(%q): expected an error, got none", src)
		}
	}
}

// TestParseIndexSoftKeyword confirms INDEX remains usable as an ordinary name in a
// normal query, since the index grammar matches it as a soft keyword rather than
// reserving it.
func TestParseIndexSoftKeyword(t *testing.T) {
	for _, src := range []string{
		"MATCH (index) RETURN index",
		"MATCH (n:Index) RETURN n",
	} {
		if _, err := parse.Parse(src); err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
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
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT",          // NOT without NULL
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
