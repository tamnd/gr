package gr

import "testing"

// TestStatementKind classifies statements by what they do without executing them, the
// pre-execution signal an authorization layer checks before a side effect can happen.
func TestStatementKind(t *testing.T) {
	db := openMem(t, "kind.gr")
	defer db.Close()

	cases := []struct {
		cypher string
		want   Kind
	}{
		{"RETURN 1 AS n", ReadStatement},
		{"MATCH (n) RETURN n", ReadStatement},
		{"CREATE (:Person {name: 'a'})", WriteStatement},
		{"MATCH (n:Person) SET n.age = 30", WriteStatement},
		{"MATCH (n:Person) DELETE n", WriteStatement},
		{"MERGE (n:Person {name: 'a'})", WriteStatement},
		{"MATCH (n:Person) REMOVE n.age", WriteStatement},
		{"CREATE INDEX FOR (p:Person) ON (p.email)", SchemaStatement},
		{"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", SchemaStatement},
		// EXPLAIN never executes, so it is a read regardless of the underlying kind.
		{"EXPLAIN CREATE (:Person)", ReadStatement},
		{"EXPLAIN MATCH (n) RETURN n", ReadStatement},
	}
	for _, c := range cases {
		got, err := db.StatementKind(c.cypher)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.cypher, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: kind = %v, want %v", c.cypher, got, c.want)
		}
	}
}

// TestStatementKindParseError returns the parse error so the caller can let the normal
// execution path surface it as a syntax error rather than guessing a kind.
func TestStatementKindParseError(t *testing.T) {
	db := openMem(t, "kinderr.gr")
	defer db.Close()
	if _, err := db.StatementKind("THIS IS NOT CYPHER"); err == nil {
		t.Error("expected a parse error for an unparseable statement")
	}
}

// TestKindString names each kind for diagnostics.
func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		ReadStatement:   "read",
		WriteStatement:  "write",
		SchemaStatement: "schema",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}
