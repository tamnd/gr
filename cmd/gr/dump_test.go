package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tamnd/gr"
)

// TestQuoteIdent confirms a plain identifier passes through and a name with a special
// character (the dump's `~n`/`~id` scaffolding, a space, an embedded backtick) is
// backtick-quoted with backticks doubled.
func TestQuoteIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Person", "Person"},
		{"_x9", "_x9"},
		{"~id", "`~id`"},
		{"has space", "`has space`"},
		{"a`b", "`a``b`"},
		{"9lead", "`9lead`"},
		{"", "``"},
	}
	for _, c := range cases {
		if got := quoteIdent(c.in); got != c.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestConstraintDDL confirms each constraint kind renders its REQUIRE predicate.
func TestConstraintDDL(t *testing.T) {
	cases := []struct {
		c    gr.ConstraintInfo
		want string
	}{
		{gr.ConstraintInfo{Name: "u", Label: "Person", Props: []string{"name"}, Kind: "UNIQUE"},
			"CREATE CONSTRAINT u IF NOT EXISTS FOR (n:Person) REQUIRE n.name IS UNIQUE;"},
		{gr.ConstraintInfo{Name: "e", Label: "Person", Props: []string{"name"}, Kind: "EXISTS"},
			"CREATE CONSTRAINT e IF NOT EXISTS FOR (n:Person) REQUIRE n.name IS NOT NULL;"},
		{gr.ConstraintInfo{Name: "t", Label: "Person", Props: []string{"born"}, Kind: "TYPE", PropType: "INTEGER"},
			"CREATE CONSTRAINT t IF NOT EXISTS FOR (n:Person) REQUIRE n.born IS :: INTEGER;"},
	}
	for _, c := range cases {
		if got := constraintDDL(c.c); got != c.want {
			t.Errorf("constraintDDL(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

// TestIndexDDL confirms an index renders the CREATE INDEX form.
func TestIndexDDL(t *testing.T) {
	ix := gr.IndexInfo{Name: "person_age", Label: "Person", Props: []string{"age"}}
	want := "CREATE INDEX person_age IF NOT EXISTS FOR (n:Person) ON (n.age);"
	if got := indexDDL(ix); got != want {
		t.Errorf("indexDDL = %q, want %q", got, want)
	}
}

// seedDB creates a small graph with a constraint and an index in dbPath via the CLI.
func seedDB(t *testing.T, dbPath string) {
	t.Helper()
	stmts := []string{
		"CREATE CONSTRAINT person_name IF NOT EXISTS FOR (p:Person) REQUIRE p.name IS UNIQUE",
		"CREATE INDEX person_age IF NOT EXISTS FOR (p:Person) ON (p.age)",
		"CREATE (a:Person {name:'Ada', age:36})-[:KNOWS {since:2019}]->(b:Person {name:'Lin'}), (a)-[:LIKES {rating:5}]->(:Genre {name:'Jazz'})",
	}
	for _, s := range stmts {
		_, errb, code := runCLI(t, []string{dbPath, "-c", s}, "")
		if code != exitOK {
			t.Fatalf("seed %q: code=%d stderr=%q", s, code, errb)
		}
	}
}

// TestDumpLoadRoundTrip is the central round-trip contract (doc 17 §13.7): a dump
// loaded into a fresh database reproduces a logically-equal graph, with the same nodes,
// relationships, properties, and schema.
func TestDumpLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	dst := filepath.Join(dir, "dst.gr")
	dumpFile := filepath.Join(dir, "d.cypher")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{src, "-c", ".dump " + dumpFile}, ""); code != exitOK {
		t.Fatalf("dump: code=%d stderr=%q", code, errb)
	}
	if _, errb, code := runCLI(t, []string{dst, "-c", ".load " + dumpFile}, ""); code != exitOK {
		t.Fatalf("load: code=%d stderr=%q", code, errb)
	}

	// Nodes and their properties survived.
	out, _, code := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (n:Person) RETURN n ORDER BY n.name"}, "")
	if code != exitOK {
		t.Fatalf("query nodes: code=%d", code)
	}
	for _, want := range []string{`"name":"Ada"`, `"age":36`, `"name":"Lin"`, `"_labels":["Person"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("loaded nodes missing %q\ngot %s", want, out)
		}
	}
	// The scaffolding is stripped from elements: no node carries the `~n` label or
	// the `~id` property.
	if strings.Contains(out, "~n") || strings.Contains(out, "~id") {
		t.Errorf("scaffolding leaked into loaded nodes: %s", out)
	}

	// Relationships re-linked to the right endpoints with their properties.
	rels, _, code := runCLI(t, []string{dst, "--mode", "jsonl", "-c",
		"MATCH (a)-[r]->(b) RETURN a.name AS s, type(r) AS t, b.name AS e, r.since AS since, r.rating AS rating ORDER BY t"}, "")
	if code != exitOK {
		t.Fatalf("query rels: code=%d", code)
	}
	for _, want := range []string{`"s":"Ada","t":"KNOWS","e":"Lin"`, `"since":2019`, `"t":"LIKES","e":"Jazz"`, `"rating":5`} {
		if !strings.Contains(rels, want) {
			t.Errorf("loaded rels missing %q\ngot %s", want, rels)
		}
	}

	// The schema (constraint and index) was re-created.
	schema, _, code := runCLI(t, []string{dst, "-c", ".indexes"}, "")
	if code != exitOK {
		t.Fatalf("indexes: code=%d", code)
	}
	if !strings.Contains(schema, "person_age on :Person(age)") {
		t.Errorf("index not re-created: %s", schema)
	}
}

// TestDumpSchemaOnly and the data-only case confirm the two halves of a dump.
func TestDumpSchemaAndDataOnly(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	seedDB(t, src)

	schema, _, code := runCLI(t, []string{src, "-c", ".dump --schema-only"}, "")
	if code != exitOK {
		t.Fatalf("schema-only: code=%d", code)
	}
	if !strings.Contains(schema, "CREATE INDEX") || strings.Contains(schema, "MERGE (n:`~n`") {
		t.Errorf("schema-only dump wrong:\n%s", schema)
	}

	data, _, code := runCLI(t, []string{src, "-c", ".dump --data-only"}, "")
	if code != exitOK {
		t.Fatalf("data-only: code=%d", code)
	}
	if strings.Contains(data, "CREATE INDEX") || !strings.Contains(data, "MERGE (n:`~n`") {
		t.Errorf("data-only dump wrong:\n%s", data)
	}
}

// TestDumpMutualExclusion confirms --schema-only and --data-only together is a usage
// error.
func TestDumpMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	seedDB(t, src)
	_, _, code := runCLI(t, []string{src, "-c", ".dump --schema-only --data-only"}, "")
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestLoadNotADump confirms a non-dump file is rejected as a format error (doc 17
// §13.5).
func TestLoadNotADump(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.cypher")
	if err := os.WriteFile(bad, []byte("MATCH (n) RETURN n;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errb, code := runCLI(t, []string{filepath.Join(dir, "x.gr"), "-c", ".load " + bad}, "")
	if code != exitFormat {
		t.Errorf("code = %d, want exitFormat", code)
	}
	if !strings.Contains(errb, "not a gr dump") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestLoadTruncated confirms a dump missing its completion marker is a data error (doc
// 17 §13.6).
func TestLoadTruncated(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	full := filepath.Join(dir, "full.cypher")
	trunc := filepath.Join(dir, "trunc.cypher")
	seedDB(t, src)
	if _, _, code := runCLI(t, []string{src, "-c", ".dump " + full}, ""); code != exitOK {
		t.Fatal("dump failed")
	}
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the completion marker line.
	cut := strings.Index(string(body), "// gr dump complete")
	if cut < 0 {
		t.Fatal("no marker in dump")
	}
	if err := os.WriteFile(trunc, body[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	_, errb, code := runCLI(t, []string{filepath.Join(dir, "y.gr"), "-c", ".load " + trunc}, "")
	if code != exitData {
		t.Errorf("code = %d, want exitData", code)
	}
	if !strings.Contains(errb, "truncated") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestDumpLoadSubcommands exercises the gr dump and gr load subcommands and the pipe
// between them (doc 17 §7.4, §7.5).
func TestDumpLoadSubcommands(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	dst := filepath.Join(dir, "dst.gr")
	seedDB(t, src)

	dump, _, code := runCLI(t, []string{"dump", src}, "")
	if code != exitOK {
		t.Fatalf("gr dump: code=%d", code)
	}
	if !strings.Contains(dump, "// gr dump complete") {
		t.Fatalf("dump to stdout lacks marker:\n%s", dump)
	}
	// Pipe the dump into gr load via stdin.
	_, errb, code := runCLI(t, []string{"load", dst}, dump)
	if code != exitOK {
		t.Fatalf("gr load: code=%d stderr=%q", code, errb)
	}
	out, _, code := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (a)-[r:KNOWS]->(b) RETURN a.name AS s, b.name AS e"}, "")
	if code != exitOK || !strings.Contains(out, `"s":"Ada","e":"Lin"`) {
		t.Errorf("subcommand round-trip lost the relationship: code=%d out=%s", code, out)
	}
}
