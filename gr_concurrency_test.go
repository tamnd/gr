package gr

import (
	"errors"
	"sync"
	"testing"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// TestMergeConcurrentSingleNode confirms many goroutines running the same MERGE on
// the same key converge on one node. The engine serializes write transactions, so
// the first MERGE creates the node and commits, and every later MERGE begins after
// that commit, probes, finds the node, and matches instead of creating a duplicate
// (doc 13 §11). A uniqueness constraint guards the invariant as a backstop.
func TestMergeConcurrentSingleNode(t *testing.T) {
	db := openMem(t, "mergeconc.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (u:User) REQUIRE u.email IS UNIQUE", nil)

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.Exec("MERGE (:User {email: 'a@x'})", nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d MERGE failed: %v", i, err)
		}
	}
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count after %d concurrent MERGE = %d, want 1", workers, n)
	}
}

// TestMergeConcurrentNoConstraint confirms the same convergence holds with no
// constraint declared: the serialization plus read-your-writes alone keep concurrent
// identical MERGE from making duplicates, because each MERGE that runs after the
// first sees the committed node and matches it.
func TestMergeConcurrentNoConstraint(t *testing.T) {
	db := openMem(t, "mergeconcnoc.gr")
	defer func() { _ = db.Close() }()

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.Exec("MERGE (:User {email: 'a@x'})", nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d MERGE failed: %v", i, err)
		}
	}
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count after %d concurrent MERGE = %d, want 1", workers, n)
	}
}

// TestConcurrentCreateUniqueOneWins confirms the commit-time uniqueness check under
// the engine write lock is the single serialization point: many goroutines that each
// plainly CREATE the same unique value race, exactly one commits, and the rest abort
// with a ConstraintError, leaving one node. Unlike MERGE this has no match branch, so
// the only thing stopping duplicates is the constraint.
func TestConcurrentCreateUniqueOneWins(t *testing.T) {
	db := openMem(t, "createconc.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (u:User) REQUIRE u.email IS UNIQUE", nil)

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.Exec("CREATE (:User {email: 'a@x'})", nil)
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case isConstraintError(err):
			conflicts++
		default:
			t.Fatalf("worker %d failed with an unexpected error: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	if conflicts != workers-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, workers-1)
	}
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1", n)
	}
}

// TestConcurrentDistinctCreates confirms the write lock serializes without losing
// writes: many goroutines that each create a distinct node all commit, and the final
// count is exactly the number of workers.
func TestConcurrentDistinctCreates(t *testing.T) {
	db := openMem(t, "distinctconc.gr")
	defer func() { _ = db.Close() }()

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.Exec("CREATE (:User {n: $n})", map[string]value.Value{"n": value.Int(int64(i))})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d create failed: %v", i, err)
		}
	}
	if n := nodeCount(t, db); n != workers {
		t.Fatalf("node count = %d, want %d (a write was lost)", n, workers)
	}
}

func isConstraintError(err error) bool {
	var ce *engine.ConstraintError
	return errors.As(err, &ce)
}
