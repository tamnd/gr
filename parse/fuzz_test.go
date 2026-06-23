package parse_test

import (
	"testing"

	"github.com/tamnd/gr/lex"
	"github.com/tamnd/gr/parse"
)

// FuzzCypherParse verifies that parsing any byte sequence never panics (doc 23 §7.3).
// A clean parse error is a pass; only a panic or nil-AST-with-nil-error is a failure.
func FuzzCypherParse(f *testing.F) {
	// Seed corpus: a range of valid and tricky queries.
	seeds := []string{
		"MATCH (n) RETURN n",
		"MATCH (n:Person {name: 'Alice'}) RETURN n.name",
		"CREATE (a:A)-[:T]->(b:B) RETURN a, b",
		"MATCH (a)-[r*1..3]->(b) RETURN a, r, b",
		"MATCH (n) WHERE n.x > 1 AND n.y < 2 RETURN count(*)",
		"WITH 1 AS x RETURN x + 2",
		"UNWIND [1,2,3] AS x RETURN x",
		"MATCH (n) DELETE n",
		"MERGE (n:N {k:1}) ON CREATE SET n.new=true ON MATCH SET n.seen=true RETURN n",
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE",
		"DROP CONSTRAINT myc",
		"CREATE INDEX FOR (n:N) ON (n.k)",
		"EXPLAIN MATCH (n) RETURN n",
		"PROFILE MATCH (n) RETURN n",
		"",
		"(",
		"MATCH (n",
		"RETURN 1 + ",
		"'unterminated",
		"/* unclosed comment",
		"MATCH (n) RETURN n.`backtick prop`",
		"MATCH path = (a)-[:T*]->(b) RETURN length(path)",
		"MATCH (a) OPTIONAL MATCH (a)-[r]->(b) RETURN a, b",
		"WITH collect(n) AS ns UNWIND ns AS x RETURN x",
		"FOREACH (x IN [1,2,3] | CREATE (:N {v:x}))",
		"MATCH (n) WHERE NOT (n)-[:R]-() RETURN n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, query string) {
		// PROPERTY: Parse never panics; it returns an AST or a typed error.
		// We use a defer/recover to catch any panic and turn it into a test failure.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse panicked on input %q: %v", query, r)
			}
		}()

		ast, err := parse.Parse(query)
		if err != nil {
			return // clean error is a pass
		}
		if ast == nil {
			t.Fatalf("Parse returned nil AST with nil error for query %q", query)
		}
	})
}

// FuzzCypherTokenize verifies that the lexer never panics on any input (doc 23 §7.3).
func FuzzCypherTokenize(f *testing.F) {
	seeds := []string{
		"MATCH (n) RETURN n",
		"",
		"'hello'",
		`"world"`,
		"123",
		"3.14",
		"/* comment */",
		"// line comment\n",
		"$param",
		"`backtick id`",
		"\x00\xff",
		string([]byte{0xc0, 0x80}), // overlong UTF-8
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Tokenize panicked on input %q: %v", src, r)
			}
		}()
		// Tokenize exhausts the lexer; ignore errors (they are clean rejections).
		_, _ = lex.Tokens(src)
	})
}
