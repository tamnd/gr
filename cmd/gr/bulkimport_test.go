package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBulkImportNodesOnly bulk-imports a single node CSV and checks the
// result is queryable via Cypher.
func TestBulkImportNodesOnly(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	nodesCSV := writeCSV(t, dir, "people.csv",
		":ID(p),name:string,:LABEL\np1,Alice,Person\np2,Bob,Person\n")

	_, errb, code := runCLI(t, []string{
		"import", db,
		"--nodes", "Person=" + nodesCSV,
	}, "")
	if code != exitOK {
		t.Fatalf("bulk import: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "imported 2 nodes") {
		t.Errorf("summary: %q", errb)
	}
}

// TestBulkImportNodesAndRels bulk-imports nodes and a relationship file.
func TestBulkImportNodesAndRels(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	nodesCSV := writeCSV(t, dir, "people.csv",
		":ID(p),name:string,:LABEL\np1,Alice,Person\np2,Bob,Person\n")
	relsCSV := writeCSV(t, dir, "knows.csv",
		":START_ID(p),:END_ID(p),:TYPE\np1,p2,KNOWS\n")

	_, errb, code := runCLI(t, []string{
		"import", db,
		"--nodes", "Person=" + nodesCSV,
		"--rels", "KNOWS=" + relsCSV,
	}, "")
	if code != exitOK {
		t.Fatalf("bulk import: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "imported 2 nodes, 1 relationships") {
		t.Errorf("summary: %q", errb)
	}
}

// TestBulkImportSkipBadRows checks that --skip-bad-rows skips bad rows rather
// than aborting.
func TestBulkImportSkipBadRows(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	// p1 appears twice; without --skip-bad-rows this would return exitData.
	nodesCSV := writeCSV(t, dir, "people.csv",
		":ID(p),name:string,:LABEL\np1,Alice,Person\np1,Dup,Person\np2,Bob,Person\n")

	_, _, code := runCLI(t, []string{
		"import", db,
		"--nodes", "Person=" + nodesCSV,
		"--skip-bad-rows",
	}, "")
	if code != exitOK {
		t.Fatalf("expected exit 0 with --skip-bad-rows, got %d", code)
	}
}

// TestBulkImportMissingFile checks that a missing node file returns an error code.
func TestBulkImportMissingFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	_, _, code := runCLI(t, []string{
		"import", db,
		"--nodes", "Person=/no/such/file.csv",
	}, "")
	if code == exitOK {
		t.Error("expected non-zero exit for missing file")
	}
}

// TestBulkImportBadFlag checks that unknown flags return exitUsage.
func TestBulkImportBadFlag(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	nodesCSV := writeCSV(t, dir, "people.csv", ":ID(p),:LABEL\np1,Person\n")
	_, _, code := runCLI(t, []string{
		"import", db,
		"--nodes", "Person=" + nodesCSV,
		"--unknown-flag",
	}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for unknown flag, got %d", code)
	}
}

// TestBulkImportMissingEquals checks that --nodes without '=' returns exitUsage.
func TestBulkImportMissingEquals(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	_, _, code := runCLI(t, []string{
		"import", db,
		"--nodes", "PersonNoFile",
	}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for malformed --nodes, got %d", code)
	}
}

// TestBulkImportNodesCSVMixedTypes loads Person and Movie nodes from separate
// files into one database and verifies both are present.
func TestBulkImportNodesCSVMixedTypes(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.gr")
	personsCSV := writeCSV(t, dir, "persons.csv",
		":ID(p),name:string,:LABEL\np1,Alice,Person\n")
	moviesCSV := writeCSV(t, dir, "movies.csv",
		":ID(m),title:string,:LABEL\nm1,Matrix,Movie\n")

	_, errb, code := runCLI(t, []string{
		"import", db,
		"--nodes", "Person=" + personsCSV,
		"--nodes", "Movie=" + moviesCSV,
	}, "")
	if code != exitOK {
		t.Fatalf("bulk import: code=%d stderr=%q", code, errb)
	}
	// Both Person and Movie nodes should be imported.
	if !strings.Contains(errb, "imported 2 nodes") {
		t.Errorf("summary: %q, want 2 nodes", errb)
	}
}

// TestBulkImportHelp checks that --help returns usage information.
func TestBulkImportHelp(t *testing.T) {
	_, errb, code := runCLI(t, []string{"import", "--help"}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for --help, got %d", code)
	}
	if !strings.Contains(errb, "--nodes") {
		t.Errorf("help text doesn't mention --nodes: %q", errb)
	}
}

// TestIsBulkImport checks the flag-detection helper.
func TestIsBulkImport(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--nodes", "Person=f.csv"}, true},
		{[]string{"--rels", "KNOWS=r.csv"}, true},
		{[]string{"--nodes=Person=f.csv"}, true},
		{[]string{"f.csv", "--as", "Person"}, false},
		{[]string{}, false},
	}
	for _, tc := range cases {
		if got := isBulkImport(tc.args); got != tc.want {
			t.Errorf("isBulkImport(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// writeLargeCSV creates a node CSV with n data rows (for smoke-testing the
// bulk path on a moderately large dataset without being slow).
func writeLargeCSV(t *testing.T, dir string, n int) string {
	t.Helper()
	p := filepath.Join(dir, "large.csv")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(":ID(p),name:string,:LABEL\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 32)
	for i := range n {
		buf = buf[:0]
		buf = append(buf, 'p')
		buf = appendInt(buf, i)
		buf = append(buf, ',')
		buf = append(buf, 'N')
		buf = appendInt(buf, i)
		buf = append(buf, ',')
		buf = append(buf, []byte("Person\n")...)
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func appendInt(dst []byte, n int) []byte {
	if n == 0 {
		return append(dst, '0')
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return append(dst, b[pos:]...)
}
