package gr

import (
	"context"
	"testing"
)

// TestHealthOpen confirms a freshly opened database reports an open, ready engine with no
// warnings (doc 20 §13.3).
func TestHealthOpen(t *testing.T) {
	db := openMem(t, "healthopen.gr")
	rep := db.Health()
	if rep.State != "open" {
		t.Errorf("state = %q, want open", rep.State)
	}
	if !rep.Ready {
		t.Error("ready = false, want true for a fresh engine")
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", rep.Warnings)
	}
}

// TestHealthClosed confirms a closed database reports a stopped, not-ready engine, so the
// health surface is safe to call after Close (doc 20 §13.3, §13.5).
func TestHealthClosed(t *testing.T) {
	db := openMem(t, "healthclosed.gr")
	_ = db.Close()
	rep := db.Health()
	if rep.State != "stopped" {
		t.Errorf("state = %q, want stopped", rep.State)
	}
	if rep.Ready {
		t.Error("ready = true, want false for a closed engine")
	}
}

// TestHealthTracksCommitsAndTransactions confirms the report's liveness counters move with the
// workload: a committed write bumps the commit count, and an open transaction shows in the
// open-transaction gauge (doc 20 §13.3).
func TestHealthTracksCommitsAndTransactions(t *testing.T) {
	db := openMem(t, "healthcommits.gr")
	mustExec(t, db, "CREATE (:Person {name: 'a'})", nil)

	rep := db.Health()
	if rep.Commits == 0 {
		t.Error("commits = 0 after a write, want nonzero")
	}
	if rep.OpenTransactions != 0 {
		t.Errorf("open transactions = %d, want 0 with none held", rep.OpenTransactions)
	}

	tx, err := db.Begin(context.Background(), Write)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if got := db.Health().OpenTransactions; got != 1 {
		t.Errorf("open transactions = %d, want 1 while one is held", got)
	}
}

// TestHealthWarnsWithoutCheckpoint confirms a database that has committed but never
// checkpointed flags the WAL-replay warning, the "what to look at" the report carries (doc 20
// §13.3).
func TestHealthWarnsWithoutCheckpoint(t *testing.T) {
	db := openMem(t, "healthnockpt.gr")
	mustExec(t, db, "CREATE (:Person {name: 'a'})", nil)

	rep := db.Health()
	if !rep.LastCheckpoint.IsZero() {
		t.Skip("a checkpoint ran on this path, the no-checkpoint warning does not apply")
	}
	found := false
	for _, w := range rep.Warnings {
		if w == "no checkpoint has run yet: recovery would replay the whole WAL" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want the no-checkpoint warning", rep.Warnings)
	}
}
