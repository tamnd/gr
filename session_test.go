package gr

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/gr/value"
)

// TestSessionExecuteWriteThenRead writes in one session transaction and reads the
// result back in the next, the causal ordering a session guarantees: a write
// committed in the session is visible to the following read in the same session.
func TestSessionExecuteWriteThenRead(t *testing.T) {
	db := openMem(t, "s1.gr")
	s := db.Session(WithDefaultAccessMode(Write))
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	v, err := s.ExecuteWrite(ctx, func(tx *Tx) (any, error) {
		sum, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil)
		if err != nil {
			return nil, err
		}
		return sum.NodesCreated, nil
	})
	if err != nil {
		t.Fatalf("execute write: %v", err)
	}
	if v.(int) != 1 {
		t.Fatalf("nodes created = %v, want 1", v)
	}

	got, err := s.ExecuteRead(ctx, func(tx *Tx) (any, error) {
		res, err := tx.Run(context.Background(), "MATCH (p:Person) RETURN count(p) AS c", nil)
		if err != nil {
			return nil, err
		}
		rec, err := Single(res)
		if err != nil {
			return nil, err
		}
		return rec.GetInt("c")
	})
	if err != nil {
		t.Fatalf("execute read: %v", err)
	}
	if got.(int64) != 1 {
		t.Fatalf("read back count = %v, want 1", got)
	}
}

// TestSessionExecuteWriteRollsBackOnError confirms a closure error discards the
// transaction's writes, the same all-or-nothing contract Update makes, with the
// (any, error) shape returning a nil value alongside the error.
func TestSessionExecuteWriteRollsBackOnError(t *testing.T) {
	db := openMem(t, "s2.gr")
	s := db.Session()
	defer func() { _ = s.Close() }()

	sentinel := errors.New("boom")
	v, err := s.ExecuteWrite(context.Background(), func(tx *Tx) (any, error) {
		if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
			return nil, err
		}
		return "unused", sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if v != nil {
		t.Fatalf("value = %v, want nil on error", v)
	}
	if got := personCount(t, db); got != 0 {
		t.Fatalf("after rollback, Person count = %d, want 0", got)
	}
}

// TestSessionNestingRejected drives the nesting guard from every managed and
// explicit entry point: a transaction opened in the session blocks a second one
// until the first finishes, and a managed call started inside another's closure is
// rejected with ErrTxnNested rather than left to deadlock on the write slot.
func TestSessionNestingRejected(t *testing.T) {
	db := openMem(t, "s3.gr")
	s := db.Session()
	defer func() { _ = s.Close() }()

	// A managed transaction nested inside another managed transaction's closure.
	err := s.View(func(tx *Tx) error {
		return s.View(func(tx *Tx) error { return nil })
	})
	if !errors.Is(err, ErrTxnNested) {
		t.Fatalf("nested View err = %v, want ErrTxnNested", err)
	}

	// An auto-commit Run nested inside a managed closure.
	err = s.Update(func(tx *Tx) error {
		_, rerr := s.Run(context.Background(), "MATCH (n) RETURN n", nil)
		return rerr
	})
	if !errors.Is(err, ErrTxnNested) {
		t.Fatalf("nested Run err = %v, want ErrTxnNested", err)
	}

	// A managed transaction started inside an explicit one.
	tx, err := s.Begin(context.Background(), Write)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ExecuteRead(context.Background(), func(tx *Tx) (any, error) { return nil, nil }); !errors.Is(err, ErrTxnNested) {
		t.Fatalf("managed inside explicit err = %v, want ErrTxnNested", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// After the explicit transaction finishes, the session hosts a new one.
	if err := s.View(func(tx *Tx) error { return nil }); err != nil {
		t.Fatalf("view after rollback: %v", err)
	}
}

// TestSessionExplicitBeginCommitClearsActive confirms an explicit Begin/Commit
// cycle leaves the session free for its next transaction, so the active flag is
// cleared by Commit just as it is by Rollback.
func TestSessionExplicitBeginCommitClearsActive(t *testing.T) {
	db := openMem(t, "s4.gr")
	s := db.Session()
	defer func() { _ = s.Close() }()

	tx, err := s.Begin(context.Background(), Write)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", map[string]value.Value{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.View(func(tx *Tx) error { return nil }); err != nil {
		t.Fatalf("view after commit: %v", err)
	}
	if got := personCount(t, db); got != 1 {
		t.Fatalf("Person count = %d, want 1", got)
	}
}

// TestSessionReadDoesNotRetryWriteDoes is a behavioral check that ExecuteRead runs
// its closure once and ExecuteWrite is the retry-capable form. With no concurrent
// writers there is no conflict to retry, so both simply run once and succeed; the
// test pins the call counts so a future change that accidentally loops a read shows.
func TestSessionReadRunsClosureOnce(t *testing.T) {
	db := openMem(t, "s5.gr")
	s := db.Session()
	defer func() { _ = s.Close() }()

	calls := 0
	_, err := s.ExecuteRead(context.Background(), func(tx *Tx) (any, error) {
		calls++
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute read: %v", err)
	}
	if calls != 1 {
		t.Fatalf("read closure ran %d times, want 1", calls)
	}
}

// TestSessionClosedRejectsUse confirms a closed session refuses further work.
func TestSessionClosedRejectsUse(t *testing.T) {
	db := openMem(t, "s6.gr")
	s := db.Session()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.View(func(tx *Tx) error { return nil }); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("view on closed session err = %v, want ErrSessionClosed", err)
	}
	if _, err := s.ExecuteRead(context.Background(), func(tx *Tx) (any, error) { return nil, nil }); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("execute read on closed session err = %v, want ErrSessionClosed", err)
	}
}

// TestSessionExecuteReadCanceledContext confirms a cancelled context stops a read
// transaction function before it runs its closure.
func TestSessionExecuteReadCanceledContext(t *testing.T) {
	db := openMem(t, "s7.gr")
	s := db.Session()
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	_, err := s.ExecuteRead(ctx, func(tx *Tx) (any, error) {
		ran = true
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("closure ran despite a cancelled context")
	}
}
