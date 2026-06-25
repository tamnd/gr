package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCSV writes a CSV file for an import test and returns its path.
func writeCSV(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestImportNodes confirms gr import loads a CSV as nodes with their columns as
// properties (doc 17 §6.10).
func TestImportNodes(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	csv := writeCSV(t, dir, "people.csv", "name,city\nAda,London\nLin,Berlin\n")

	_, errb, code := runCLI(t, []string{"import", db, csv, "--as", "Person"}, "")
	if code != exitOK {
		t.Fatalf("import: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "imported 2 nodes") {
		t.Errorf("summary = %q", errb)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c",
		"MATCH (p:Person {name:'Ada'}) RETURN p.city AS city"}, "")
	if !strings.Contains(out, `"city":"London"`) {
		t.Errorf("Ada's city not imported: %s", out)
	}
}

// TestImportTypeCoercion confirms --type coerces a column to a non-string value.
func TestImportTypeCoercion(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	csv := writeCSV(t, dir, "people.csv", "name,age\nAda,36\n")

	if _, _, code := runCLI(t, []string{"import", db, csv, "--as", "Person", "--type", "age:INTEGER"}, ""); code != exitOK {
		t.Fatalf("import: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (p:Person) RETURN p.age AS age"}, "")
	if !strings.Contains(out, `"age":36`) {
		t.Errorf("age not an integer: %s", out)
	}
}

// TestImportMerge confirms --merge upserts on the id column so a re-import updates
// rather than duplicating (doc 17 §6.10).
func TestImportMerge(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	first := writeCSV(t, dir, "v1.csv", "id,city\n1,London\n")
	second := writeCSV(t, dir, "v2.csv", "id,city\n1,Paris\n")

	if _, _, code := runCLI(t, []string{"import", db, first, "--as", "Person", "--id-col", "id", "--merge"}, ""); code != exitOK {
		t.Fatalf("import 1: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", db, second, "--as", "Person", "--id-col", "id", "--merge"}, ""); code != exitOK {
		t.Fatalf("import 2: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (p:Person) RETURN count(p) AS c"}, "")
	if !strings.Contains(out, `"c":1`) {
		t.Errorf("merge duplicated the node: %s", out)
	}
	city, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (p:Person) RETURN p.city AS city"}, "")
	if !strings.Contains(city, `"city":"Paris"`) {
		t.Errorf("merge did not update the city: %s", city)
	}
}

// TestImportNoHeader confirms --no-header treats every row as data and names columns
// positionally.
func TestImportNoHeader(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	csv := writeCSV(t, dir, "raw.csv", "Ada\nLin\n")

	if _, _, code := runCLI(t, []string{"import", db, csv, "--as", "Tag", "--no-header"}, ""); code != exitOK {
		t.Fatalf("import: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (n:Tag) RETURN count(n) AS c"}, "")
	if !strings.Contains(out, `"c":2`) {
		t.Errorf("expected 2 nodes from 2 data rows: %s", out)
	}
	val, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (n:Tag {col1:'Ada'}) RETURN n.col1 AS v"}, "")
	if !strings.Contains(val, `"v":"Ada"`) {
		t.Errorf("positional column col1 not set: %s", val)
	}
}

// TestImportSkipLimit confirms --skip and --limit bound the imported rows.
func TestImportSkipLimit(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	csv := writeCSV(t, dir, "people.csv", "name\nA\nB\nC\nD\n")

	if _, _, code := runCLI(t, []string{"import", db, csv, "--as", "P", "--skip", "1", "--limit", "2"}, ""); code != exitOK {
		t.Fatalf("import: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (n:P) RETURN count(n) AS c"}, "")
	if !strings.Contains(out, `"c":2`) {
		t.Errorf("skip/limit wrong count: %s", out)
	}
	gotB, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (n:P {name:'B'}) RETURN count(n) AS c"}, "")
	if !strings.Contains(gotB, `"c":1`) {
		t.Errorf("skip should have dropped A and kept B: %s", gotB)
	}
}

// TestImportExportRoundTrip confirms an export feeds back through an import to an
// equivalent set of nodes (doc 19 §6).
func TestImportExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	dst := filepath.Join(dir, "dst.gr")
	out := filepath.Join(dir, "people.csv")
	seedDB(t, src)

	if _, _, code := runCLI(t, []string{"export", src, "--nodes", "Person", "--to", out}, ""); code != exitOK {
		t.Fatalf("export: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", dst, out, "--as", "Person"}, ""); code != exitOK {
		t.Fatalf("import: code=%d", code)
	}
	got, _, _ := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (n:Person) RETURN count(n) AS c"}, "")
	if !strings.Contains(got, `"c":2`) {
		t.Errorf("round trip lost nodes: %s", got)
	}
	name, _, _ := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (n:Person {name:'Ada'}) RETURN n.name AS n"}, "")
	if !strings.Contains(name, `"n":"Ada"`) {
		t.Errorf("round trip lost Ada: %s", name)
	}
}

// TestDotImport confirms the shell .import loads a file like the subcommand does.
func TestDotImport(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	csv := writeCSV(t, dir, "people.csv", "name\nAda\n")

	if _, errb, code := runCLI(t, []string{db, "-c", ".import " + csv + " --as Person"}, ""); code != exitOK {
		t.Fatalf(".import: code=%d stderr=%q", code, errb)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH (n:Person) RETURN count(n) AS c"}, "")
	if !strings.Contains(out, `"c":1`) {
		t.Errorf(".import lost data: %s", out)
	}
}

// TestImportRelationships confirms gr import loads a CSV as relationships between nodes
// matched on their id property (doc 17 §6.10, doc 19 §7.3). A row whose endpoint is not
// found is counted as dangling and skipped, not an error.
func TestImportRelationships(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	nodes := writeCSV(t, dir, "nodes.csv", "id,name\n1,Ada\n2,Lin\n")
	rels := writeCSV(t, dir, "rels.csv", "from,to,since\n1,2,2019\n1,9,2020\n")

	if _, _, code := runCLI(t, []string{"import", db, nodes, "--as", "Person", "--id-col", "id"}, ""); code != exitOK {
		t.Fatalf("node import: code=%d", code)
	}
	_, errb, code := runCLI(t, []string{"import", db, rels, "--as-rel", "KNOWS",
		"--from", "Person:from", "--to", "Person:to", "--id-col", "id", "--type", "since:INTEGER"}, "")
	if code != exitOK {
		t.Fatalf("rel import: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "imported 1 relationships") || !strings.Contains(errb, "1 rows skipped") {
		t.Errorf("summary = %q", errb)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c",
		"MATCH (:Person {name:'Ada'})-[r:KNOWS]->(:Person {name:'Lin'}) RETURN r.since AS since"}, "")
	if !strings.Contains(out, `"since":2019`) {
		t.Errorf("relationship or its property not imported: %s", out)
	}
}

// TestImportRelMerge confirms --merge on a relationship import does not duplicate an
// edge that already exists between the same endpoints.
func TestImportRelMerge(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	nodes := writeCSV(t, dir, "nodes.csv", "id\n1\n2\n")
	rels := writeCSV(t, dir, "rels.csv", "from,to\n1,2\n1,2\n")

	if _, _, code := runCLI(t, []string{"import", db, nodes, "--as", "Person", "--id-col", "id"}, ""); code != exitOK {
		t.Fatalf("node import: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", db, rels, "--as-rel", "KNOWS",
		"--from", "Person:from", "--to", "Person:to", "--id-col", "id", "--merge"}, ""); code != exitOK {
		t.Fatalf("rel import: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c", "MATCH ()-[r:KNOWS]->() RETURN count(r) AS c"}, "")
	if !strings.Contains(out, `"c":1`) {
		t.Errorf("merge duplicated the relationship: %s", out)
	}
}

// TestImportRelDistinctKeys confirms --from-key and --to-key match endpoints in
// different node sets keyed on different properties (doc 19 §7.3, the multi-space case).
func TestImportRelDistinctKeys(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	people := writeCSV(t, dir, "people.csv", "pid,name\n1,Ada\n")
	movies := writeCSV(t, dir, "movies.csv", "mid,title\n1,Solaris\n")
	rels := writeCSV(t, dir, "rated.csv", "person,movie,stars\n1,1,5\n")

	if _, _, code := runCLI(t, []string{"import", db, people, "--as", "Person", "--id-col", "pid"}, ""); code != exitOK {
		t.Fatalf("people: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", db, movies, "--as", "Movie", "--id-col", "mid"}, ""); code != exitOK {
		t.Fatalf("movies: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", db, rels, "--as-rel", "RATED",
		"--from", "Person:person", "--to", "Movie:movie", "--from-key", "pid", "--to-key", "mid", "--type", "stars:INTEGER"}, ""); code != exitOK {
		t.Fatalf("rel import: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{db, "--mode", "jsonl", "-c",
		"MATCH (:Person {name:'Ada'})-[r:RATED]->(:Movie {title:'Solaris'}) RETURN r.stars AS stars"}, "")
	if !strings.Contains(out, `"stars":5`) {
		t.Errorf("cross-label relationship not imported: %s", out)
	}
}

// TestImportArgs confirms the import argument checks.
func TestImportArgs(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	csv := writeCSV(t, dir, "people.csv", "name\nAda\n")
	pq := writeCSV(t, dir, "data.parquet", "x")

	cases := [][]string{
		{"import", db, csv},                                                                   // no --as
		{"import", db, "--as", "Person"},                                                      // no file
		{"import", db, csv, "--as", "Person", "--merge"},                                      // --merge without --id-col
		{"import", db, pq, "--as", "Movie"},                                                   // parquet not supported
		{"import", db, csv, "--as", "Person", "--bogus"},                                      // unknown flag
		{"import", db, csv, "--as-rel", "KNOWS"},                                              // rel without --from/--to
		{"import", db, csv, "--from", "P:a", "--to", "P:b"},                                   // endpoints without --as-rel
		{"import", db, csv, "--as", "P", "--as-rel", "KNOWS", "--from", "P:a", "--to", "P:b"}, // both node and rel
		{"import", db, csv, "--as-rel", "KNOWS", "--from", "bad", "--to", "P:b"},              // --from missing LABEL:COL
	}
	for i, args := range cases {
		if _, _, code := runCLI(t, args, ""); code != exitUsage {
			t.Errorf("case %d %v: code = %d, want exitUsage", i, args, code)
		}
	}
}
