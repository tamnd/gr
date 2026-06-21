package gr

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestRetrySucceedsFirstTry confirms a closure that returns nil runs exactly once.
func TestRetrySucceedsFirstTry(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 5, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Retry returned %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("closure ran %d times, want 1", calls)
	}
}

// TestRetryRetriesConflict confirms a closure that returns a conflict a few times
// then succeeds is re-run until it succeeds, and the eventual nil is returned.
func TestRetryRetriesConflict(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 5, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("attempt %d: %w", calls, ErrConflict)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry returned %v, want nil after recovery", err)
	}
	if calls != 3 {
		t.Fatalf("closure ran %d times, want 3", calls)
	}
}

// TestRetryGivesUpAfterAttempts confirms a closure that always conflicts is run
// exactly attempts times and the last error is returned.
func TestRetryGivesUpAfterAttempts(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 4, func() error {
		calls++
		return fmt.Errorf("try %d: %w", calls, ErrConflict)
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Retry returned %v, want a wrapped ErrConflict", err)
	}
	if calls != 4 {
		t.Fatalf("closure ran %d times, want 4", calls)
	}
}

// TestRetryDoesNotRetryNonConflict confirms a non-retryable error is returned at
// once, without a second attempt.
func TestRetryDoesNotRetryNonConflict(t *testing.T) {
	sentinel := errors.New("boom")
	calls := 0
	err := Retry(context.Background(), 5, func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Retry returned %v, want the sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("a non-retryable error was retried: closure ran %d times, want 1", calls)
	}
}

// TestRetryStopsOnCancelledContext confirms a cancelled context stops the loop
// before the next attempt and returns the context error.
func TestRetryStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Retry(ctx, 5, func() error {
		calls++
		cancel()
		return fmt.Errorf("conflict %d: %w", calls, ErrConflict)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry returned %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("closure ran %d times after cancel, want 1", calls)
	}
}

// TestRetryZeroAttemptsRunsOnce confirms a non-positive attempt count is normalized
// to a single try rather than running zero times.
func TestRetryZeroAttemptsRunsOnce(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 0, func() error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("Retry(0) ran %d times with err %v, want 1 time and nil", calls, err)
	}
}

// TestIsRetryable confirms only a conflict is classed retryable.
func TestIsRetryable(t *testing.T) {
	if !IsRetryable(ErrConflict) {
		t.Fatal("ErrConflict is not retryable")
	}
	if !IsRetryable(fmt.Errorf("wrapped: %w", ErrConflict)) {
		t.Fatal("a wrapped ErrConflict is not retryable")
	}
	if IsRetryable(errors.New("other")) {
		t.Fatal("an unrelated error is retryable")
	}
	if IsRetryable(nil) {
		t.Fatal("nil is retryable")
	}
}

// TestUpdateCommitsThroughRetry confirms Update still commits a successful write on
// the single-writer path, where the retry is dormant and the closure runs once.
func TestUpdateCommitsThroughRetry(t *testing.T) {
	db := openMem(t, "updateretry.gr")
	defer func() { _ = db.Close() }()

	runs := 0
	err := db.Update(func(tx *Tx) error {
		runs++
		_, err := tx.Exec("CREATE (:Person {name: 'A'})", nil)
		return err
	})
	if err != nil {
		t.Fatalf("Update returned %v", err)
	}
	if runs != 1 {
		t.Fatalf("closure ran %d times on the single-writer path, want 1", runs)
	}
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1 (Update did not commit)", n)
	}
}

// TestUpdateDoesNotRetryNonConflict confirms a closure that fails with a
// non-conflict error runs exactly once: Update rolls back and returns it without
// retrying, since re-running cannot help.
func TestUpdateDoesNotRetryNonConflict(t *testing.T) {
	db := openMem(t, "updatenoretry.gr")
	defer func() { _ = db.Close() }()

	sentinel := errors.New("abort this")
	runs := 0
	err := db.Update(func(tx *Tx) error {
		runs++
		if _, err := tx.Exec("CREATE (:Person {name: 'A'})", nil); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update returned %v, want the sentinel", err)
	}
	if runs != 1 {
		t.Fatalf("a non-conflict error was retried: closure ran %d times, want 1", runs)
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d, want 0 (the failed Update should have rolled back)", n)
	}
}
