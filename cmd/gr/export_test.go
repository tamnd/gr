package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportNodesCSV confirms gr export --nodes writes an _id column plus the union of
// the label's property keys, with an absent property left blank (doc 17 §6.11).
func TestExportNodesCSV(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "people.csv")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{"export", src, "--nodes", "Person", "--to", out}, ""); code != exitOK {
		t.Fatalf("export: code=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if lines[0] != "_id,age,name" {
		t.Errorf("header = %q, want _id,age,name", lines[0])
	}
	if !strings.Contains(text, ",36,Ada") {
		t.Errorf("Ada row missing or wrong: %s", text)
	}
	// Lin has no age, so the age cell is blank.
	if !strings.Contains(text, ",,Lin") {
		t.Errorf("Lin row should have a blank age: %s", text)
	}
	if n := len(lines); n != 3 {
		t.Errorf("got %d lines, want 3 (header + 2 nodes)", n)
	}
}

// TestExportRelsCSV confirms gr export --rels writes _start/_end id columns plus the
// relationship's properties (doc 17 §6.11).
func TestExportRelsCSV(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "knows.csv")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{"export", src, "--rels", "KNOWS", "--to", out}, ""); code != exitOK {
		t.Fatalf("export: code=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if lines[0] != "_start,_end,since" {
		t.Errorf("header = %q, want _start,_end,since", lines[0])
	}
	if !strings.HasSuffix(lines[1], ",2019") {
		t.Errorf("KNOWS row missing the since property: %q", lines[1])
	}
}

// TestExportRelsRelink confirms --from-property/--to-property emit endpoint properties
// in the _start/_end columns instead of opaque element ids (doc 19 §7.3).
func TestExportRelsRelink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "knows.csv")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{"export", src, "--rels", "KNOWS",
		"--from-property", "name", "--to-property", "name", "--to", out}, ""); code != exitOK {
		t.Fatalf("export: code=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if lines[0] != "_start,_end,since" {
		t.Errorf("header = %q", lines[0])
	}
	if lines[1] != "Ada,Lin,2019" {
		t.Errorf("relink row = %q, want Ada,Lin,2019", lines[1])
	}
}

// TestExportImportRelinkRoundTrip confirms a node export plus a relink relationship
// export re-import to an equivalent graph through the CLI (doc 19 §7.3): the full
// node-and-relationship round trip these commands are meant to support.
func TestExportImportRelinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	dst := filepath.Join(dir, "dst.gr")
	nodes := filepath.Join(dir, "nodes.csv")
	rels := filepath.Join(dir, "rels.csv")
	seedDB(t, src)

	if _, _, code := runCLI(t, []string{"export", src, "--nodes", "Person", "--to", nodes}, ""); code != exitOK {
		t.Fatalf("export nodes: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"export", src, "--rels", "KNOWS",
		"--from-property", "name", "--to-property", "name", "--to", rels}, ""); code != exitOK {
		t.Fatalf("export rels: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", dst, nodes, "--as", "Person", "--id-col", "name"}, ""); code != exitOK {
		t.Fatalf("import nodes: code=%d", code)
	}
	if _, _, code := runCLI(t, []string{"import", dst, rels, "--as-rel", "KNOWS",
		"--from", "Person:_start", "--to", "Person:_end", "--id-col", "name", "--type", "since:INTEGER"}, ""); code != exitOK {
		t.Fatalf("import rels: code=%d", code)
	}
	out, _, _ := runCLI(t, []string{dst, "--mode", "jsonl", "-c",
		"MATCH (:Person {name:'Ada'})-[r:KNOWS]->(:Person {name:'Lin'}) RETURN r.since AS since"}, "")
	if !strings.Contains(out, `"since":2019`) {
		t.Errorf("round trip lost the KNOWS edge: %s", out)
	}
}

// TestExportQueryCSV confirms gr export --query writes the query result with the query's
// own column names (doc 17 §6.11).
func TestExportQueryCSV(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "names.csv")
	seedDB(t, src)

	q := "MATCH (p:Person) RETURN p.name AS name, p.age AS age ORDER BY name"
	if _, errb, code := runCLI(t, []string{"export", src, "--query", q, "--to", out}, ""); code != exitOK {
		t.Fatalf("export: code=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	want := []string{"name,age", "Ada,36", "Lin,"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(lines), lines, len(want))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestExportTSV confirms the tsv format is tab-separated, inferred from the .tsv
// extension when --format is not given.
func TestExportTSV(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "knows.tsv")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{"export", src, "--rels", "KNOWS", "--to", out}, ""); code != exitOK {
		t.Fatalf("export: code=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "_start\t_end\tsince") {
		t.Errorf("tsv header not tab-separated: %q", string(b))
	}
}

// TestExportNoHeader confirms --no-header drops the header row.
func TestExportNoHeader(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "people.csv")
	seedDB(t, src)

	if _, _, code := runCLI(t, []string{"export", src, "--nodes", "Person", "--to", out, "--no-header"}, ""); code != exitOK {
		t.Fatalf("export: code=%d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "_id") {
		t.Errorf("--no-header still wrote a header: %q", string(b))
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want 2 nodes", len(lines))
	}
}

// TestExportToStdout confirms --to - streams the export to stdout.
func TestExportToStdout(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	seedDB(t, src)

	out, _, code := runCLI(t, []string{"export", src, "--nodes", "Person", "--to", "-"}, "")
	if code != exitOK {
		t.Fatalf("export -: code=%d", code)
	}
	if !strings.Contains(out, "_id,age,name") || !strings.Contains(out, "Ada") {
		t.Errorf("stdout export missing data: %q", out)
	}
}

// TestDotExport confirms the shell .export writes the same file the subcommand does.
func TestDotExport(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "people.csv")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{src, "-c", ".export --nodes Person --to " + out}, ""); code != exitOK {
		t.Fatalf(".export: code=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Ada") {
		t.Errorf(".export lost data: %q", string(b))
	}
}

// TestExportArgs confirms the export argument checks: a missing source selector, a
// missing --to, and mutually exclusive selectors are usage errors.
func TestExportArgs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "x.csv")
	seedDB(t, src)

	cases := [][]string{
		{"export", src, "--to", out},         // no selector
		{"export", src, "--nodes", "Person"}, // no --to
		{"export", src, "--nodes", "Person", "--query", "MATCH (n) RETURN n", "--to", out}, // both
		{"export", src, "--nodes", "Person", "--to", out, "--bogus"},                       // unknown flag
		{"export", src, "--nodes", "Person", "--to", out, "--from-property", "name"},       // relink on nodes
	}
	for i, args := range cases {
		if _, _, code := runCLI(t, args, ""); code != exitUsage {
			t.Errorf("case %d: code = %d, want exitUsage", i, code)
		}
	}
}
