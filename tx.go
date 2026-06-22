package gr

import (
	"context"
	"errors"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/exec"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// ErrReadOnly is returned when a write statement runs inside a read transaction:
// a managed View, an explicit Begin(Read), or a Run/Exec on such a transaction
// (doc 16 §6.5).
var ErrReadOnly = errors.New("gr: write statement in a read-only transaction")

// ErrTxnDone is returned by a transaction method called after the transaction has
// committed or rolled back (doc 16 §6.8). A *Tx is valid only until it finishes.
var ErrTxnDone = errors.New("gr: transaction already committed or rolled back")

// AccessMode declares whether a transaction may write (doc 16 §6.5). A Read
// transaction runs against a snapshot, never takes the write slot, and rejects a
// writing statement; a Write transaction may read and write, and its reads see its
// own uncommitted writes.
type AccessMode int

const (
	// Read is a snapshot-only transaction that may not write.
	Read AccessMode = iota
	// Write is a read-write transaction.
	Write
)

// Tx is a managed unit of work over the database: a snapshot for a Read
// transaction, a snapshot plus an accumulating write set for a Write transaction
// (doc 16 §6). Run streams a read against it, Exec runs a write against it, and
// the work becomes durable at Commit or is discarded at Rollback. A *Tx is driven
// by one goroutine at a time and is valid only until it finishes (doc 16 §6.8).
type Tx struct {
	db    *DB
	etx   engine.Tx
	write bool
	done  bool
	// session is set when the transaction was opened through Session.Begin, so that
	// finishing the transaction (Commit or Rollback) clears the session's active flag
	// and lets the session host its next transaction. It is nil for a transaction
	// begun on the database directly or run inside a managed closure.
	session *Session
}

// Begin opens an explicit transaction in the given access mode (doc 16 §6.3). The
// caller drives it with Run and Exec and finishes it with Commit or Rollback;
// Rollback (or Commit) must always be called, so the idiom is a deferred Rollback,
// which is a no-op once the transaction has committed.
func (db *DB) Begin(mode AccessMode) (*Tx, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	etx, err := db.eng.Begin(mode == Write)
	if err != nil {
		return nil, err
	}
	return &Tx{db: db, etx: etx, write: mode == Write}, nil
}

// Commit makes a write transaction's changes durable and visible, then finishes
// the transaction (doc 16 §6.3). On a read transaction there is nothing to commit,
// so it simply finishes. A constraint the transaction would violate is reported
// here, with the transaction left rolled back (doc 13 §12). Commit on an already
// finished transaction returns ErrTxnDone.
func (tx *Tx) Commit() error {
	if tx.done {
		return ErrTxnDone
	}
	tx.done = true
	tx.releaseSession()
	return tx.etx.Commit()
}

// Rollback discards a transaction's uncommitted changes and finishes it (doc 16
// §6.3). It is a no-op on an already finished transaction, so a deferred Rollback
// after a successful Commit does nothing and the idiom defer tx.Rollback() is
// always safe.
func (tx *Tx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	tx.releaseSession()
	return tx.etx.Abort()
}

// releaseSession clears the active flag of the session that opened this transaction
// through Session.Begin, so the session can host its next transaction once this one
// finishes. It is a no-op for a transaction begun on the database directly or inside
// a managed closure, where the session has no flag to clear.
func (tx *Tx) releaseSession() {
	if tx.session != nil {
		tx.session.active = false
		tx.session = nil
	}
}

// View runs fn inside a read transaction (doc 16 §6.2). It takes a snapshot at
// begin, runs the closure, and releases the snapshot when the closure returns; it
// commits nothing, since a read transaction has nothing to commit, and it never
// conflicts. The closure's error is returned to the caller unchanged.
func (db *DB) View(fn func(tx *Tx) error) error {
	if db.eng == nil {
		return ErrClosed
	}
	tx, err := db.Begin(Read)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Update runs fn inside a read-write transaction (doc 16 §6.2). On a nil return it
// commits, and on a non-nil return it rolls back and returns the error, so the
// common write path is correct by construction. It is [Retry] wrapped around a
// begin, the closure, and a commit: on the concurrent-writer path a conflict at
// commit re-runs the whole thing against a fresh snapshot, up to the database's
// configured retry bound (doc 16 §6.4). The single-writer path never conflicts, so
// the retry is dormant, but the closure must still be re-runnable: it must compute
// the same writes from the same inputs each time and hold no side effect outside the
// transaction that a re-run would double (doc 16 §6.2).
func (db *DB) Update(fn func(tx *Tx) error) error {
	if db.eng == nil {
		return ErrClosed
	}
	return Retry(context.Background(), db.maxRetries, func() error {
		tx, err := db.Begin(Write)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// Run executes a Cypher statement against the transaction and returns a streaming
// result (doc 16 §7.1). It is the single entry point for both reads and writes: a
// read streams lazily from the transaction's snapshot, and a write executes and
// reports its mutations through the result's Summary (doc 13 §3). The result
// borrows the transaction, so it does not commit or abort anything on Close, and it
// is valid only within the transaction (doc 16 §8.5): close it before the
// transaction finishes.
//
// A statement run inside a write transaction sees the transaction's own uncommitted
// writes (read-your-writes, doc 06 §2.3) and binds against the transaction's
// catalog view, so it resolves names the transaction has interned but not yet
// committed. A write in a read transaction is rejected with ErrReadOnly.
func (tx *Tx) Run(cypher string, params map[string]value.Value) (*Result, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return nil, err
	}
	if q.Explain {
		// A write transaction holds the engine lock, so it must plan against its own
		// catalog view and skip the seek rewrite (a nil index oracle) and the cost
		// estimates (nil statistics), exactly as its execution path does; a read
		// transaction has no such lock and plans against the engine with its index
		// oracle and statistics.
		if tx.write {
			return tx.db.explain(q, tx.etx, nil, nil)
		}
		return tx.db.explain(q, tx.db.eng, indexLookup{tx.db.eng}, engineStats{tx.db.eng})
	}
	if q.Schema != nil {
		return nil, ErrSchemaCommand
	}
	if queryHasWrites(q) {
		if !tx.write {
			return nil, ErrReadOnly
		}
		return tx.runWrite(q, params)
	}
	return tx.runRead(cypher, q, params)
}

// runRead opens a read statement over the transaction's snapshot and returns a
// result that streams lazily from the cursor. A read transaction reuses the
// database plan cache (which binds against the engine catalog, safe because a read
// transaction holds no write lock); a write transaction binds against its own
// catalog view, since it holds the engine lock (an engine lookup would deadlock)
// and must see its own uncommitted interned names.
func (tx *Tx) runRead(cypher string, q *ast.Query, params map[string]value.Value) (*Result, error) {
	var b *bind.Bound
	var op plan.Op
	if tx.write {
		bound, err := bind.Bind(q, bind.NewEngineCatalog(tx.etx), false)
		if err != nil {
			return nil, err
		}
		b, op = bound, plan.Plan(bound)
	} else {
		entry, err := tx.db.compile(cypher)
		if err != nil {
			return nil, err
		}
		b, op = entry.Bound, entry.Op
	}
	cur, err := exec.Open(op, tx.readCtx(b, params))
	if err != nil {
		return nil, err
	}
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx.etx, ownTx: false}, nil
}

// runWrite executes a write statement eagerly and returns a result over its
// materialized RETURN rows. The statement runs to exhaustion before Run returns, so
// every mutation lands and the summary is complete; a lazily streamed write would
// leave the statement half-applied if the caller stopped iterating before commit.
// Names are interned inside the transaction and the bind resolves against its
// catalog view (doc 13 §9), so the write takes no lock it does not already hold and
// leaves no orphan token on rollback (doc 13 §16).
func (tx *Tx) runWrite(q *ast.Query, params map[string]value.Value) (*Result, error) {
	cols, buf, summary, err := tx.db.execWriteBuffered(tx.etx, q, params)
	if err != nil {
		return nil, err
	}
	return &Result{cols: cols, buf: buf, summary: summary, tx: tx.etx, ownTx: false}, nil
}

// Exec runs a Cypher write statement against the transaction and returns a summary
// of its mutations (doc 16 §7.1, doc 13 §3). It requires a write transaction,
// returning ErrReadOnly on a read transaction. Unlike the database-level Exec it
// does not begin or commit a transaction of its own: it applies the statement to
// the caller's open transaction, which the caller commits or rolls back. An error
// leaves the statement's partial effects in the transaction for the caller's
// Rollback to discard; Update does this automatically.
//
// The statement's names are interned inside this transaction and the bind resolves
// against the transaction's catalog view (doc 13 §9), so the write needs no engine
// lock it does not already hold and leaves no orphan token on rollback (doc 13 §16).
func (tx *Tx) Exec(cypher string, params map[string]value.Value) (Summary, error) {
	if tx.done {
		return Summary{}, ErrTxnDone
	}
	if !tx.write {
		return Summary{}, ErrReadOnly
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return Summary{}, err
	}
	if q.Explain {
		return Summary{}, ErrExplain
	}
	if q.Schema != nil {
		// A schema command runs its own write transaction (execSchema), which would
		// deadlock against the write lock this transaction already holds, so it is
		// not part of a managed transaction. Run it through the database-level Exec.
		return Summary{}, ErrSchemaCommand
	}
	if err := internWriteNames(tx.etx, q); err != nil {
		return Summary{}, err
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(tx.etx), false)
	if err != nil {
		return Summary{}, err
	}
	eff := &exec.SideEffects{}
	ctx := tx.readCtx(b, params)
	ctx.Effects = eff
	if err := drain(plan.Plan(b), ctx); err != nil {
		return Summary{}, err
	}
	return summaryOf(eff), nil
}

// readCtx builds the execution context for a statement run against this
// transaction: the transaction itself, the parameter map, the resolver from the
// bound query, and the reverse token namers the entity functions need.
func (tx *Tx) readCtx(b *bind.Bound, params map[string]value.Value) *exec.Ctx {
	return &exec.Ctx{
		Tx:          tx.etx,
		Params:      params,
		Resolve:     exec.ResolverFromBound(b),
		LabelName:   tx.db.tokenNamer(catalog.KindLabel),
		RelTypeName: tx.db.tokenNamer(catalog.KindRelType),
		PropKeyName: tx.db.tokenNamer(catalog.KindPropKey),
	}
}
