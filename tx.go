package gr

import (
	"errors"

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
	return tx.etx.Abort()
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
// common write path is correct by construction. The closure must be re-runnable:
// on the concurrent-writer path Update retries it against a fresh snapshot after a
// conflict (doc 16 §6.4), and although the single-writer path never conflicts, so
// the retry is dormant, the closure must hold no side effect outside the
// transaction that a re-run would double (doc 16 §6.2).
func (db *DB) Update(fn func(tx *Tx) error) error {
	if db.eng == nil {
		return ErrClosed
	}
	tx, err := db.Begin(Write)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Run executes a Cypher read statement against the transaction's snapshot and
// returns a streaming result (doc 16 §7.1). The result borrows the transaction, so
// it does not commit or abort anything on Close, and it is valid only within the
// transaction (doc 16 §8.5): close it before the transaction finishes.
//
// A read inside a write transaction sees the transaction's own uncommitted writes
// (read-your-writes, doc 06 §2.3) and binds against the transaction's catalog view,
// so it resolves names the transaction has interned but not yet committed.
//
// A write statement is rejected with ErrReadQuery, the same split Query and Exec
// make at the database level: this Run streams read rows, and Exec applies a write.
func (tx *Tx) Run(cypher string, params map[string]value.Value) (*Result, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	b, op, err := tx.compileRead(cypher)
	if err != nil {
		return nil, err
	}
	cur, err := exec.Open(op, tx.readCtx(b, params))
	if err != nil {
		return nil, err
	}
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx.etx, ownTx: false}, nil
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

// compileRead parses, binds, and plans a read statement for this transaction,
// rejecting a write with ErrReadQuery and a schema command with ErrSchemaCommand.
// A read transaction reuses the database plan cache, which binds against the engine
// catalog (safe, since a read transaction holds no write lock). A write transaction
// cannot: it holds the engine lock, so an engine catalog lookup would deadlock, and
// it must see its own uncommitted interned names, so it binds against its own
// catalog view and skips the cache.
func (tx *Tx) compileRead(cypher string) (*bind.Bound, plan.Op, error) {
	if !tx.write {
		entry, err := tx.db.compile(cypher)
		if err != nil {
			return nil, nil, err
		}
		if queryHasWrites(entry.Bound.Query) {
			return nil, nil, ErrReadQuery
		}
		return entry.Bound, entry.Op, nil
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return nil, nil, err
	}
	if q.Schema != nil {
		return nil, nil, ErrSchemaCommand
	}
	if queryHasWrites(q) {
		return nil, nil, ErrReadQuery
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(tx.etx), false)
	if err != nil {
		return nil, nil, err
	}
	return b, plan.Plan(b), nil
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
