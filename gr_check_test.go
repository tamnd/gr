package gr

import (
	"testing"

	"github.com/tamnd/gr/vfs"
)

// buildCheckDB creates a small but non-trivial database for checker tests.
// It creates two Person nodes, one Movie node, and two KNOWS relationships.
func buildCheckDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open("check.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE (a:Person {name:'Alice'})-[:KNOWS]->(b:Person {name:'Bob'})`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE (m:Movie {title:'Matrix'})`, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestCheckQuickClean verifies that a freshly written database passes the quick check.
func TestCheckQuickClean(t *testing.T) {
	db := buildCheckDB(t)
	defer func() { _ = db.Close() }()

	rep, err := db.Check(CheckQuick)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("clean DB has quick-check findings: %v", rep.Findings)
	}
	if rep.Stats.PagesScanned == 0 {
		t.Error("PagesScanned should be > 0")
	}
}

// TestCheckDefaultClean verifies that a freshly written database passes the default check.
func TestCheckDefaultClean(t *testing.T) {
	db := buildCheckDB(t)
	defer func() { _ = db.Close() }()

	rep, err := db.Check(CheckDefault)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("clean DB has default-check findings: %v", rep.Findings)
	}
}

// TestCheckFullClean verifies that a freshly written database passes the full check.
func TestCheckFullClean(t *testing.T) {
	db := buildCheckDB(t)
	defer func() { _ = db.Close() }()

	rep, err := db.Check(CheckFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("clean DB has full-check findings: %v", rep.Findings)
	}
}

// TestCheckForensicClean verifies that the forensic level also passes on a clean DB.
func TestCheckForensicClean(t *testing.T) {
	db := buildCheckDB(t)
	defer func() { _ = db.Close() }()

	rep, err := db.Check(CheckForensic)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("clean DB has forensic-check findings: %v", rep.Findings)
	}
}

// TestCheckClosedDB verifies that Check returns ErrClosed on a closed database.
func TestCheckClosedDB(t *testing.T) {
	db := buildCheckDB(t)
	_ = db.Close()
	_, err := db.Check(CheckDefault)
	if err != ErrClosed {
		t.Errorf("expected ErrClosed, got %v", err)
	}
}

// TestCheckReportHas verifies the Has helper on CheckReport.
func TestCheckReportHas(t *testing.T) {
	rep := CheckReport{
		Findings: []CheckFinding{
			{Severity: CheckCorruption, Code: "PAGE_CHECKSUM", Detail: "test"},
		},
	}
	if !rep.Has("PAGE_CHECKSUM") {
		t.Error("Has should find PAGE_CHECKSUM")
	}
	if rep.Has("FREE_LIST_CORRUPT") {
		t.Error("Has should not find FREE_LIST_CORRUPT")
	}
}

// TestCheckSeverityStrings verifies Severity.String() returns the expected labels.
func TestCheckSeverityStrings(t *testing.T) {
	cases := []struct {
		s    CheckSeverity
		want string
	}{
		{CheckWarning, "WARNING"},
		{CheckInconsistency, "INCONSISTENCY"},
		{CheckCorruption, "CORRUPTION"},
		{CheckFatal, "FATAL"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestCheckEmptyDB checks that an empty database (no data, just the schema) passes.
func TestCheckEmptyDB(t *testing.T) {
	db, err := Open("empty.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	rep, err := db.Check(CheckFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("empty DB has findings: %v", rep.Findings)
	}
}

// TestCheckAfterBulkLoad runs the full check on a database that was opened read-only
// (simulating a post-bulk-import verification).
func TestCheckAfterBulkLoad(t *testing.T) {
	mem := vfs.NewMem()
	db, err := Open("bl.gr", Options{VFS: mem})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := db.Exec(`CREATE (p:Person {name:'x'})`, nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	// Reopen read-only and check.
	roDB, err := Open("bl.gr", Options{VFS: mem, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roDB.Close() }()

	rep, err := roDB.Check(CheckFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) > 0 {
		t.Errorf("post-load DB has findings: %v", rep.Findings)
	}
}

// assertIntegrity is the in-test oracle helper specified in doc 23 §8.11. Every
// test that builds a database can call this to verify structural health.
func assertIntegrity(t *testing.T, db *DB) {
	t.Helper()
	rep, err := db.Check(CheckFull)
	if err != nil {
		t.Fatalf("assertIntegrity: Check failed: %v", err)
	}
	for _, f := range rep.Findings {
		if f.Severity >= CheckInconsistency {
			t.Errorf("integrity: [%s] %s page=%d elem=%d: %s",
				f.Severity, f.Code, f.Page, f.Element, f.Detail)
		}
	}
}
