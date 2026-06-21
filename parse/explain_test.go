package parse_test

import "testing"

// TestParseExplainPrefix confirms a leading EXPLAIN sets the Explain flag and leaves
// the body parsed exactly as the same statement without the prefix.
func TestParseExplainPrefix(t *testing.T) {
	plain := mustParse(t, "MATCH (p:Person) RETURN p")
	explained := mustParse(t, "EXPLAIN MATCH (p:Person) RETURN p")

	if plain.Explain {
		t.Fatal("a statement without the prefix has Explain set")
	}
	if !explained.Explain {
		t.Fatal("EXPLAIN did not set Explain")
	}
	if sexpr(plain) != sexpr(explained) {
		t.Fatalf("EXPLAIN changed the body:\n plain: %s\n  expl: %s", sexpr(plain), sexpr(explained))
	}
}

// TestParseExplainSchemaCommand confirms EXPLAIN attaches to a schema command too,
// so the layer above can reject it with a clear error rather than mis-parsing.
func TestParseExplainSchemaCommand(t *testing.T) {
	q := mustParse(t, "EXPLAIN CREATE INDEX FOR (p:Person) ON (p.email)")
	if !q.Explain {
		t.Fatal("EXPLAIN did not set Explain on a schema command")
	}
	if q.Schema == nil {
		t.Fatal("the schema command body was not parsed")
	}
}

// TestExplainIsSoftKeyword confirms EXPLAIN is only a prefix at the very start: used
// as a variable name inside the body it stays an ordinary identifier.
func TestExplainIsSoftKeyword(t *testing.T) {
	q := mustParse(t, "MATCH (explain:Person) RETURN explain")
	if q.Explain {
		t.Fatal("an EXPLAIN identifier in the body was read as the prefix")
	}
}
