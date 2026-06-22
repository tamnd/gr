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
func (db *DB) Begin(ctx context.Context, mode AccessMode) (*Tx, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	return db.ViewContext(context.Background(), fn)
}

// ViewContext is View with an explicit context (doc 16 §6.2). The context is
// honoured at begin: a context already cancelled when ViewContext is called returns
// its error without opening a transaction or running the closure.
func (db *DB) ViewContext(ctx context.Context, fn func(tx *Tx) error) error {
	if db.eng == nil {
		return ErrClosed
	}
	tx, err := db.Begin(ctx, Read)
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
	return db.UpdateContext(context.Background(), fn)
}

// UpdateContext is Update with an explicit context (doc 16 §6.2, §6.4). The context
// bounds the retry loop: [Retry] checks it before each attempt, so a cancelled
// context stops the re-run rather than spinning against a conflict that will not
// clear, and a context already cancelled at the call returns without running the
// closure.
func (db *DB) UpdateContext(ctx context.Context, fn func(tx *Tx) error) error {
	if db.eng == nil {
		return ErrClosed
	}
	return Retry(ctx, db.retries(), func() error {
		tx, err := db.Begin(ctx, Write)
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
func (tx *Tx) Run(ctx context.Context, cypher string, params Params, opts ...RunOption) (*Result, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := tx.db.resolveRun(opts)
	vals, err := toValues(params)
	if err != nil {
		return nil, err
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
	if q.Admin != nil {
		// An administrative statement manages users through the credential API, which
		// runs its own durable transaction (gr_admin.go); it would deadlock against the
		// write lock this transaction holds, so it is not part of a managed transaction.
		// Run it through the database-level Run.
		return nil, ErrAdminCommand
	}
	if q.Pragma != nil {
		// A PRAGMA reads or sets connection and file configuration, not transactional
		// graph data (doc 24 §3), so it is not part of a managed transaction; run it
		// through the database-level Run.
		return nil, ErrPragmaCommand
	}
	if queryHasWrites(q) {
		if !tx.write {
			return nil, ErrReadOnly
		}
		return tx.runWrite(q, vals, cfg.lazy)
	}
	return tx.runRead(cypher, q, vals, cfg.lazy)
}

// runRead opens a read statement over the transaction's snapshot and returns a
// result that streams lazily from the cursor. A read transaction reuses the
// database plan cache (which binds against the engine catalog, safe because a read
// transaction holds no write lock); a write transaction binds against its own
// catalog view, since it holds the engine lock (an engine lookup would deadlock)
// and must see its own uncommitted interned names.
func (tx *Tx) runRead(cypher string, q *ast.Query, params map[string]value.Value, lazy bool) (*Result, error) {
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
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx.etx, ownTx: false, mat: tx.db.materializer(tx.etx, lazy)}, nil
}

// runWrite executes a write statement eagerly and returns a result over its
// materialized RETURN rows. The statement runs to exhaustion before Run returns, so
// every mutation lands and the summary is complete; a lazily streamed write would
// leave the statement half-applied if the caller stopped iterating before commit.
// Names are interned inside the transaction and the bind resolves against its
// catalog view (doc 13 §9), so the write takes no lock it does not already hold and
// leaves no orphan token on rollback (doc 13 §16).
func (tx *Tx) runWrite(q *ast.Query, params map[string]value.Value, lazy bool) (*Result, error) {
	cols, buf, summary, err := tx.db.execWriteBuffered(tx.etx, q, params)
	if err != nil {
		return nil, err
	}
	return &Result{cols: cols, buf: buf, summary: summary, tx: tx.etx, ownTx: false, mat: tx.db.materializer(tx.etx, lazy)}, nil
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
	if q.Admin != nil {
		// Likewise an administrative statement runs its own durable transaction through
		// the credential API, so it is not part of a managed transaction.
		return Summary{}, ErrAdminCommand
	}
	if q.Pragma != nil {
		// A PRAGMA is connection and file configuration, not a graph write (doc 24 §3);
		// run it through the database-level Run.
		return Summary{}, ErrPragmaCommand
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

// NodeByElementId fetches a single node by its element id under the transaction's
// snapshot (doc 16 §10.7). It is the lookup a program uses to turn an id it stored
// earlier back into a node, the round trip behind a node's ElementId. It returns
// ErrNotFound when no such node is visible at the snapshot, and the same for an id
// that is not a node element id (a malformed string, or a relationship id), so a
// caller need not distinguish a deleted node from a wrong id. The returned node reads
// from this transaction, so fetch it within the transaction; under eager
// materialization, the default, its properties stay valid after the transaction ends.
func (tx *Tx) NodeByElementId(id string) (Node, error) {
	if tx.done {
		return Node{}, ErrTxnDone
	}
	kind, raw, err := decodeElementID(id)
	if err != nil || kind != elemNode {
		return Node{}, ErrNotFound
	}
	ok, err := tx.etx.NodeExists(engine.NodeID(raw))
	if err != nil {
		return Node{}, err
	}
	if !ok {
		return Node{}, ErrNotFound
	}
	return tx.db.materializer(tx.etx, tx.db.lazyDefault()).node(raw), nil
}

// RelationshipByElementId fetches a single relationship by its element id under the
// transaction's snapshot (doc 16 §10.7). Like NodeByElementId it returns ErrNotFound
// for an absent relationship and for an id that is not a relationship element id. The
// returned relationship reads from this transaction, so fetch it within the
// transaction.
func (tx *Tx) RelationshipByElementId(id string) (Relationship, error) {
	if tx.done {
		return Relationship{}, ErrTxnDone
	}
	kind, raw, err := decodeElementID(id)
	if err != nil || kind != elemRel {
		return Relationship{}, ErrNotFound
	}
	ok, err := tx.etx.RelExists(engine.RelID(raw))
	if err != nil {
		return Relationship{}, err
	}
	if !ok {
		return Relationship{}, ErrNotFound
	}
	return tx.db.materializer(tx.etx, tx.db.lazyDefault()).rel(raw), nil
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
