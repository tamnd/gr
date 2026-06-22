package gr

import (
	"context"
	"errors"
)

// ErrTxnNested is returned when a transaction is started inside another
// transaction's scope: calling Begin, View, Update, Run, ExecuteRead, or
// ExecuteWrite on a session that already has a transaction open (doc 16 §6.8). gr
// has no savepoints in v1, so a unit of work is one transaction; nesting is an
// error rather than a no-op so the mistake surfaces instead of deadlocking against
// the single-writer slot.
var ErrTxnNested = errors.New("gr: transaction may not be nested")

// ErrSessionClosed is returned by a session method called after the session has
// been closed. A closed session holds no resources, so this only guards against
// use after Close.
var ErrSessionClosed = errors.New("gr: session is closed")

// Session is an ordered scope for a sequence of transactions over a database (doc
// 16 §5). It is the Neo4j-driver shape: open a session, run some transactions in
// it, close it. In the embedded single-file model a session is deliberately thin,
// neither a network connection nor a pool checkout but a scope that carries a
// default access mode for its transactions, sequences them so a write committed in
// the session is visible to the next read in the same session (causal ordering,
// §5.4), and hosts the transaction-function helpers ExecuteRead and ExecuteWrite.
//
// A session is cheap to create and to close. It is not safe for concurrent use by
// multiple goroutines (§5.3): its transactions run one after another, and its
// causal-ordering guarantee depends on that sequencing, so a program that wants
// concurrency uses one session per goroutine over the one shared database.
//
// Sessions are optional sugar over the database (§5.2): View, Update, and Run also
// exist on *DB directly, and a program that does not need session-scoped ordering
// or driver-shaped code calls those instead. Both are the same machinery; the
// session adds only the scope and the ordering.
type Session struct {
	db   *DB
	mode AccessMode
	// active records that a transaction is open in this session, so a nested Begin,
	// View, Update, Run, or transaction function is rejected with ErrTxnNested rather
	// than blocking on the write slot. A session runs its transactions one at a time
	// (§5.3), so a single flag is enough; an explicit Begin clears it through the
	// returned transaction's Commit or Rollback (the session back-reference on *Tx).
	active bool
}

// SessionOption configures a session at creation (doc 16 §5.5).
type SessionOption func(*Session)

// WithDefaultAccessMode sets the access mode the session's auto-commit Run uses and
// the default the session carries for its transactions (doc 16 §5.5). It defaults
// to Read, so a session created without this option runs reads unless a transaction
// declares otherwise.
func WithDefaultAccessMode(m AccessMode) SessionOption {
	return func(s *Session) { s.mode = m }
}

// Session opens a session over the database (doc 16 §5.1). The session is a thin,
// single-goroutine scope; it holds no engine resources of its own until a
// transaction runs in it, so creating one is cheap and a program may use one per
// unit of work. The options set the session's defaults (§5.5); the zero set leaves
// a Read-default session.
func (db *DB) Session(opts ...SessionOption) *Session {
	s := &Session{db: db, mode: Read}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Close closes the session (doc 16 §5.1). A session holds no engine resources
// between transactions, so Close only marks it unusable; a transaction the caller
// left open through an explicit Begin is the caller's to finish, not the session's.
// Close is idempotent.
func (s *Session) Close() error {
	s.db = nil
	return nil
}

// Begin opens an explicit transaction scoped to the session (doc 16 §6.3). The
// transaction clears the session's active flag when it commits or rolls back, so
// the session can host the next transaction; until then a second Begin, a managed
// View or Update, or an auto-commit Run on this session returns ErrTxnNested. The
// caller drives the transaction with Run and Exec and finishes it with Commit or
// Rollback, exactly as with the database-level Begin.
func (s *Session) Begin(ctx context.Context, mode AccessMode) (*Tx, error) {
	if s.db == nil {
		return nil, ErrSessionClosed
	}
	if s.active {
		return nil, ErrTxnNested
	}
	tx, err := s.db.Begin(ctx, mode)
	if err != nil {
		return nil, err
	}
	tx.session = s
	s.active = true
	return tx, nil
}

// View runs fn inside a read transaction scoped to the session (doc 16 §6.2). It is
// the session-scoped form of DB.View: same machinery, with the session's nesting
// guard so a managed transaction started inside another transaction's scope is
// rejected rather than left to deadlock.
func (s *Session) View(fn func(tx *Tx) error) error {
	return s.ViewContext(context.Background(), fn)
}

// ViewContext is View with an explicit context (doc 16 §6.2). The context is passed
// through to the underlying transaction so a cancellation is honoured at begin.
func (s *Session) ViewContext(ctx context.Context, fn func(tx *Tx) error) error {
	if s.db == nil {
		return ErrSessionClosed
	}
	if s.active {
		return ErrTxnNested
	}
	s.active = true
	defer func() { s.active = false }()
	return s.db.ViewContext(ctx, fn)
}

// Update runs fn inside a read-write transaction scoped to the session, retrying on
// conflict (doc 16 §6.2). It is the session-scoped form of DB.Update, with the
// session's nesting guard. The closure must be re-runnable, since a conflict on the
// concurrent-writer path re-runs it against a fresh snapshot.
func (s *Session) Update(fn func(tx *Tx) error) error {
	return s.UpdateContext(context.Background(), fn)
}

// UpdateContext is Update with an explicit context (doc 16 §6.2, §6.4). The context
// bounds the retry loop and is honoured at each begin.
func (s *Session) UpdateContext(ctx context.Context, fn func(tx *Tx) error) error {
	if s.db == nil {
		return ErrSessionClosed
	}
	if s.active {
		return ErrTxnNested
	}
	s.active = true
	defer func() { s.active = false }()
	return s.db.UpdateContext(ctx, fn)
}

// Run executes a single statement in an implicit transaction scoped to the session
// (doc 16 §6.7, §7.6). It is the auto-commit form: it begins an implicit
// transaction, runs the statement, and commits when the result is consumed, with
// the access mode inferred from the statement. The session's nesting guard rejects
// an auto-commit Run while an explicit transaction is open in the session, since
// that would start a second transaction against the single-writer slot.
func (s *Session) Run(ctx context.Context, cypher string, params Params) (*Result, error) {
	if s.db == nil {
		return nil, ErrSessionClosed
	}
	if s.active {
		return nil, ErrTxnNested
	}
	return s.db.Run(ctx, cypher, params)
}

// ExecuteRead runs fn inside a managed read transaction and returns the value fn
// produces (doc 16 §6.6). It is the Neo4j-driver transaction-function spelling of
// View: the difference is the signature, fn returning (any, error) so the caller
// receives a result value rather than closing over a variable. A read does not
// conflict, so ExecuteRead does not retry. The context's cancellation is honored
// before the closure runs.
func (s *Session) ExecuteRead(ctx context.Context, fn func(tx *Tx) (any, error)) (any, error) {
	return s.execute(ctx, Read, fn)
}

// ExecuteWrite runs fn inside a managed read-write transaction, retrying on
// conflict, and returns the value fn produces (doc 16 §6.6). It is the Neo4j-driver
// transaction-function spelling of Update: same retry semantics, so fn must be
// re-runnable (doc 16 §6.2), with the (any, error) signature that hands the caller a
// result value. The retry is bounded by the database's configured retry count and
// honors the context's cancellation between attempts.
func (s *Session) ExecuteWrite(ctx context.Context, fn func(tx *Tx) (any, error)) (any, error) {
	return s.execute(ctx, Write, fn)
}

// execute is the shared body of ExecuteRead and ExecuteWrite (doc 16 §6.6). A Read
// runs the closure once against a snapshot and never retries; a Write wraps the
// begin, closure, and commit in Retry, so a conflict on the concurrent-writer path
// re-runs the whole thing against a fresh snapshot, and the produced value is taken
// only after the commit succeeds so a retried attempt's value never leaks out.
func (s *Session) execute(ctx context.Context, mode AccessMode, fn func(tx *Tx) (any, error)) (any, error) {
	if s.db == nil {
		return nil, ErrSessionClosed
	}
	if s.active {
		return nil, ErrTxnNested
	}
	s.active = true
	defer func() { s.active = false }()

	if mode == Read {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tx, err := s.db.Begin(ctx, Read)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		return fn(tx)
	}

	var out any
	err := Retry(ctx, s.db.maxRetries, func() error {
		tx, err := s.db.Begin(ctx, Write)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		v, err := fn(tx)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
