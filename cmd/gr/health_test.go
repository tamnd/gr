package main

import (
	"strings"
	"testing"
)

// TestYesNo confirms the ready-flag renderer maps a boolean to yes or no.
func TestYesNo(t *testing.T) {
	if got := yesNo(true); got != "yes" {
		t.Errorf("yesNo(true) = %q, want yes", got)
	}
	if got := yesNo(false); got != "no" {
		t.Errorf("yesNo(false) = %q, want no", got)
	}
}

// TestHealthCommand exercises .health and gr health over a seeded database: both print
// the same report with an open, ready engine and a nonzero commit count from the seed
// writes (doc 17 §6.17, §7.9).
func TestHealthCommand(t *testing.T) {
	dir := t.TempDir()
	db := dir + "/social.gr"
	seedDB(t, db)

	want := []string{
		"State:            open",
		"Ready:            yes",
		"Open transactions:0",
	}

	// .health goes to the chatter channel (stderr).
	_, errb, code := runCLI(t, []string{db, "-c", ".health"}, "")
	if code != exitOK {
		t.Fatalf(".health: code=%d", code)
	}
	for _, w := range want {
		if !strings.Contains(errb, w) {
			t.Errorf(".health missing %q\ngot:\n%s", w, errb)
		}
	}
	if !strings.Contains(errb, "Commits:") {
		t.Errorf(".health has no commits line:\n%s", errb)
	}

	// gr health goes to stdout so it can be captured.
	out, _, code := runCLI(t, []string{"health", db}, "")
	if code != exitOK {
		t.Fatalf("gr health: code=%d", code)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("gr health missing %q\ngot:\n%s", w, out)
		}
	}
}

// TestHealthReportsWarning confirms a session that committed but never checkpointed
// flags the WAL-replay warning the report carries. The commit counter is per-open, so
// the write and the .health run in one session (doc 17 §6.17, doc 20 §13.3).
func TestHealthReportsWarning(t *testing.T) {
	dir := t.TempDir()
	db := dir + "/x.gr"
	_, errb, code := runCLI(t, []string{db, "-c", "CREATE (:Person {name:'a'})", "-c", ".health"}, "")
	if code != exitOK {
		t.Fatalf(".health: code=%d", code)
	}
	if !strings.Contains(errb, "Commits:") || strings.Contains(errb, "Commits:          0") {
		t.Errorf("commit count not reflected after a write:\n%s", errb)
	}
	if strings.Contains(errb, "Last checkpoint:  (none)") &&
		!strings.Contains(errb, "no checkpoint has run yet") {
		t.Errorf("no-checkpoint warning missing with no checkpoint:\n%s", errb)
	}
}

// TestHealthCmdNeedsDatabase confirms gr health without a database argument is a usage
// error.
func TestHealthCmdNeedsDatabase(t *testing.T) {
	_, _, code := runCLI(t, []string{"health"}, "")
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}
