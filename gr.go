// Package gr is an embedded, single-file, labeled-property-graph database for Go
// that speaks Cypher (spec 2060). It is pure Go (CGO_ENABLED=0), stores a whole
// graph in one self-describing .gr file with -wal/-shm sidecars, and gives the
// SQLite "open a file, get a database" feel for graphs.
//
// Open creates or opens a .gr file and mounts the storage engine over it; Query
// runs a Cypher read query against a snapshot and streams the result rows; Close
// leaves a quiescent, checksum-valid file. The read query path is M2 (doc 25
// §5); the write path is M3 (doc 25 §1.5). The library API in full is doc 16.
package gr

import (
	"errors"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/exec"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// ErrClosed is returned by operations on a database that has been closed.
var ErrClosed = errors.New("gr: database is closed")

// ErrReadQuery is returned by Query when the statement contains a write clause,
// and by Exec when, conversely, a read-only statement is run where a write is
// expected (a read-only Exec is allowed, but Query rejects writes outright).
var ErrReadQuery = errors.New("gr: Query is read-only; use Exec for a write statement")

// ErrSchemaCommand is returned by Query when the statement is a schema command
// (CREATE CONSTRAINT, DROP CONSTRAINT); those run through Exec, which carries the
// write transaction and reports the schema mutation in its summary.
var ErrSchemaCommand = errors.New("gr: schema commands run through Exec, not Query")

// ErrExplain is returned when an EXPLAIN statement reaches an entry point that
// cannot carry a plan listing. EXPLAIN yields rows (the operator tree), so it runs
// through Run or a transaction's Run, not through the row-less Exec or the
// write-rejecting, cache-backed Query.
var ErrExplain = errors.New("gr: EXPLAIN runs through Run, not Query or Exec")

// ErrExplainSchema is returned by EXPLAIN of a schema command. A schema command
// changes the catalog outside the operator pipeline (execSchema), so it has no plan
// to render.
var ErrExplainSchema = errors.New("gr: cannot EXPLAIN a schema command")

// DB is an open gr database. It owns the storage engine, which owns the pager
// over the underlying file; queries run against snapshots the engine hands out.
// It also owns the plan cache, so a repeated query shape reuses its compiled plan.
type DB struct {
	path        string
	eng         *engine.DiskEngine
	cache       *plan.Cache
	maxRetries  int
	driftFactor float64
}

// Options configure how a database is opened. The zero value is the default:
// the OS filesystem, the default page size, and full synchronous durability.
type Options struct {
	// VFS is the virtual filesystem to open the file through; nil uses the real
	// OS filesystem. Tests pass an in-memory, fault-injecting VFS here.
	VFS vfs.VFS
	// PageSize is used only when creating a new file; 0 means the default.
	PageSize uint32
	// Sync is the durability level; 0 means full synchronous (the safe default).
	Sync wal.SyncLevel
	// ReadOnly opens the database for queries only.
	ReadOnly bool
	// SaltSeed makes WAL salt generation deterministic for tests; 0 is fine in
	// production where determinism is not required.
	SaltSeed uint64
	// PlanCacheSize bounds the plan cache in distinct query shapes; 0 uses the
	// default ([plan.DefaultCacheSize]). A negative value is treated as the
	// default, not as a disabled cache.
	PlanCacheSize int
	// MaxRetries bounds how many times Update re-runs its closure after a conflict;
	// 0 uses [DefaultMaxRetries]. It is dormant on the single-writer path, where a
	// write transaction never conflicts.
	MaxRetries int
	// ReplanDriftFactor sets how far the data may drift under a fixed schema before a
	// cached plan is recompiled: a label or type whose share of the graph changes by
	// more than this multiple since the plan was costed triggers a re-plan (doc 11
	// §7). 0 uses [plan.DefaultDriftFactor]; a value of one or less disables adaptive
	// re-planning, leaving a cached plan in place until the schema changes.
	ReplanDriftFactor float64
}

// Open opens the database at path, creating it with a fresh graph structure if it
// does not exist, and recovering the WAL if it does, so a crash before this call
// leaves the database at its last committed state.
func Open(path string, opt Options) (*DB, error) {
	fsys := opt.VFS
	if fsys == nil {
		fsys = vfs.NewOS()
	}
	eng, err := engine.Open(fsys, path, pager.Options{
		PageSize: opt.PageSize,
		Sync:     opt.Sync,
		ReadOnly: opt.ReadOnly,
		SaltSeed: opt.SaltSeed,
	})
	if err != nil {
		return nil, err
	}
	maxRetries := opt.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	driftFactor := opt.ReplanDriftFactor
	if driftFactor == 0 {
		driftFactor = plan.DefaultDriftFactor
	}
	return &DB{
		path:        path,
		eng:         eng,
		cache:       plan.NewCache(opt.PlanCacheSize),
		maxRetries:  maxRetries,
		driftFactor: driftFactor,
	}, nil
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// PageSize returns the file's page size in bytes.
func (db *DB) PageSize() uint32 { return db.eng.PageSize() }

// Query runs a Cypher read query against a snapshot of the database and returns a
// streaming result. It threads the whole read pipeline: the text is parsed to an
// AST ([parse]), bound against the catalog ([bind]), planned into a logical
// operator tree ([plan]), and opened over a read transaction by the executor
// ([exec]). The returned result holds that transaction open until it is closed,
// so the caller must Close it.
//
// The compiled query (the bound query and its plan) is cached keyed by the query
// text and the catalog version, so a repeated shape skips parse, bind, and plan
// and only binds parameters and runs (doc 14 §8). Because parameters are
// placeholders in the text, one cached plan serves every parameter value.
//
// params supplies the values for the query's $-parameters; a nil map is fine for
// a parameterless query. Names the catalog never interned bind as unknown, which
// the read path treats as the schema-optional empty result rather than an error
// (doc 08 §5.3).
func (db *DB) Query(cypher string, params map[string]value.Value) (*Result, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	entry, err := db.compile(cypher)
	if err != nil {
		return nil, err
	}
	if queryHasWrites(entry.Bound.Query) {
		return nil, ErrReadQuery
	}
	tx, err := db.eng.Begin(false)
	if err != nil {
		return nil, err
	}
	cur, err := exec.Open(entry.Op, db.execCtx(tx, entry.Bound, params))
	if err != nil {
		_ = tx.Abort()
		return nil, err
	}
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx, ownTx: true}, nil
}

// execCtx builds the execution context shared by every run of a statement: the
// transaction it runs against, the parameter map, the resolver from the bound
// query, and the reverse token namers the entity functions need. A write run sets
// Effects on the returned context to collect its mutation counts.
func (db *DB) execCtx(etx engine.Tx, b *bind.Bound, params map[string]value.Value) *exec.Ctx {
	return &exec.Ctx{
		Tx:          etx,
		Params:      params,
		Resolve:     exec.ResolverFromBound(b),
		LabelName:   db.tokenNamer(catalog.KindLabel),
		RelTypeName: db.tokenNamer(catalog.KindRelType),
		PropKeyName: db.tokenNamer(catalog.KindPropKey),
	}
}

// execWriteBuffered interns, binds, plans, and runs a write statement against an
// open transaction to exhaustion, materializing its RETURN rows and collecting its
// mutation counts. It is the shared body of every eagerly executed write: the
// database-level auto-commit Run, and the managed transaction's Run. Running to
// exhaustion before returning is what keeps a write all-or-nothing: every mutation
// lands before the caller sees a row, so a half-consumed result cannot leave the
// statement partly applied at commit. It does not begin, commit, or abort the
// transaction; the caller owns that.
func (db *DB) execWriteBuffered(etx engine.Tx, q *ast.Query, params map[string]value.Value) ([]string, []eval.Row, Summary, error) {
	if err := internWriteNames(etx, q); err != nil {
		return nil, nil, Summary{}, err
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(etx), false)
	if err != nil {
		return nil, nil, Summary{}, err
	}
	eff := &exec.SideEffects{}
	ctx := db.execCtx(etx, b, params)
	ctx.Effects = eff
	cur, err := exec.Open(plan.Plan(b), ctx)
	if err != nil {
		return nil, nil, Summary{}, err
	}
	cols := cur.Cols()
	var buf []eval.Row
	for {
		row, ok, err := cur.Next()
		if err != nil {
			_ = cur.Close()
			return nil, nil, Summary{}, err
		}
		if !ok {
			break
		}
		buf = append(buf, cloneRow(row))
	}
	if err := cur.Close(); err != nil {
		return nil, nil, Summary{}, err
	}
	return cols, buf, summaryOf(eff), nil
}

// cloneRow copies a row map so a buffered write result does not alias a row the
// executor may reuse for the next Next call.
func cloneRow(row eval.Row) eval.Row {
	out := make(eval.Row, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

// Run executes a Cypher statement in an implicit transaction and returns a result
// (doc 16 §6.7, §7.1). It is the database-level single entry point that infers the
// access mode from the statement so a caller need not pick Query versus Exec: a read
// runs against a snapshot and streams lazily, committing nothing; a write runs in an
// implicit write transaction, executes eagerly, and commits before Run returns, with
// its RETURN rows buffered and its mutations reported through the result's Summary; a
// schema command runs through execSchema and reports its change through Summary. This
// is the seam the CLI and server marshal one statement at a time onto (doc 16 §6.7).
//
// params supplies the values for the statement's $-parameters; a nil map is fine for
// a parameterless statement. The caller must Close the result; for a write or schema
// result Close has nothing to release, since the implicit transaction has already
// committed.
func (db *DB) Run(cypher string, params map[string]value.Value) (*Result, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return nil, err
	}
	if q.Explain {
		return db.explain(q, db.eng, indexLookup{db.eng}, engineStats{db.eng})
	}
	if q.Schema != nil {
		s, err := db.execSchema(q.Schema)
		if err != nil {
			return nil, err
		}
		return &Result{summary: s}, nil
	}
	if queryHasWrites(q) {
		return db.runAutoWrite(q, params)
	}
	return db.Query(cypher, params)
}

// runAutoWrite executes a write statement in an implicit write transaction and
// commits it before returning, so the auto-commit Run keeps a single write statement
// all-or-nothing at the statement boundary. The statement runs eagerly to exhaustion
// (execWriteBuffered), so every mutation lands and the RETURN rows are materialized
// before the commit; any error aborts the transaction and leaves the database
// unchanged. The returned result iterates the buffered rows and reports the mutations
// through Summary; it owns no open transaction, so Close has nothing to release.
func (db *DB) runAutoWrite(q *ast.Query, params map[string]value.Value) (*Result, error) {
	tx, err := db.eng.Begin(true)
	if err != nil {
		return nil, err
	}
	cols, buf, summary, err := db.execWriteBuffered(tx, q, params)
	if err != nil {
		_ = tx.Abort()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{cols: cols, buf: buf, summary: summary}, nil
}

// Summary reports the graph mutations a write statement performed, the normative
// openCypher statistics (doc 13 §3.4). All counts are zero for a statement that
// changed nothing.
type Summary struct {
	NodesCreated         int
	NodesDeleted         int
	RelationshipsCreated int
	RelationshipsDeleted int
	PropertiesSet        int
	LabelsAdded          int
	LabelsRemoved        int
	IndexesAdded         int
	IndexesRemoved       int
	ConstraintsAdded     int
	ConstraintsRemoved   int
}

// Exec runs a Cypher write statement in its own write transaction and returns a
// summary of the mutations it performed (doc 13 §3, the write path). It threads
// the same pipeline as Query — parse, bind, plan, execute — over a write
// transaction, and commits once the whole statement has run; any error aborts the
// transaction, leaving the database unchanged.
//
// The transaction is begun first, then the names a write clause introduces
// (labels, relationship types, property keys) are interned inside it through the
// write SPI (doc 13 §9): interning under the held write lock keeps the new tokens
// part of this transaction, so an abort rolls them back and leaves no orphan token
// (doc 13 §16). A statement that turns out to write nothing still runs; a
// read-only statement runs too and reports an empty summary, so Exec is a superset
// of Query for callers that do not need streamed rows.
//
// params supplies the values for the statement's $-parameters; a nil map is fine
// for a parameterless statement.
func (db *DB) Exec(cypher string, params map[string]value.Value) (Summary, error) {
	if db.eng == nil {
		return Summary{}, ErrClosed
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return Summary{}, err
	}
	if q.Explain {
		return Summary{}, ErrExplain
	}
	if q.Schema != nil {
		return db.execSchema(q.Schema)
	}
	tx, err := db.eng.Begin(true)
	if err != nil {
		return Summary{}, err
	}
	if err := internWriteNames(tx, q); err != nil {
		_ = tx.Abort()
		return Summary{}, err
	}
	// Bind against the transaction's own catalog view, not the engine's: the write
	// tx holds the engine lock, so an engine lookup would deadlock, and binding
	// through the tx lets the statement resolve the names it just interned.
	b, err := bind.Bind(q, bind.NewEngineCatalog(tx), false)
	if err != nil {
		_ = tx.Abort()
		return Summary{}, err
	}
	op := plan.Plan(b)
	eff := &exec.SideEffects{}
	ctx := db.execCtx(tx, b, params)
	ctx.Effects = eff
	if err := drain(op, ctx); err != nil {
		_ = tx.Abort()
		return Summary{}, err
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, err
	}
	return summaryOf(eff), nil
}

// execSchema applies a data-definition statement (doc 08 §6). Each runs in its
// own write transaction inside the engine, which interns the names, validates the
// existing data, records the change durably, and commits; this layer only maps the
// outcome onto the mutation summary. A schema change touches no graph rows, so it
// runs outside the read/write operator pipeline.
func (db *DB) execSchema(cmd ast.SchemaCommand) (Summary, error) {
	switch c := cmd.(type) {
	case *ast.CreateConstraint:
		var added bool
		var err error
		switch c.Type {
		case ast.ConstraintExists:
			added, err = db.eng.CreateExistenceConstraint(c.Name, c.Label, c.Props[0], c.IfNotExists)
		case ast.ConstraintPropertyType:
			added, err = db.eng.CreateTypeConstraint(c.Name, c.Label, c.Props[0], c.PropType, c.IfNotExists)
		default:
			added, err = db.eng.CreateUniqueConstraint(c.Name, c.Label, c.Props[0], c.IfNotExists)
		}
		if err != nil {
			return Summary{}, err
		}
		if added {
			return Summary{ConstraintsAdded: 1}, nil
		}
		return Summary{}, nil
	case *ast.DropConstraint:
		removed, err := db.eng.DropConstraint(c.Name, c.IfExists)
		if err != nil {
			return Summary{}, err
		}
		if removed {
			return Summary{ConstraintsRemoved: 1}, nil
		}
		return Summary{}, nil
	case *ast.CreateIndex:
		added, err := db.eng.CreateIndex(c.Name, c.Label, c.Props[0], c.IfNotExists)
		if err != nil {
			return Summary{}, err
		}
		if added {
			return Summary{IndexesAdded: 1}, nil
		}
		return Summary{}, nil
	case *ast.DropIndex:
		removed, err := db.eng.DropIndex(c.Name, c.IfExists)
		if err != nil {
			return Summary{}, err
		}
		if removed {
			return Summary{IndexesRemoved: 1}, nil
		}
		return Summary{}, nil
	default:
		return Summary{}, errors.New("gr: unsupported schema command")
	}
}

// drain compiles and runs a plan to exhaustion, discarding the rows. A write
// statement's effects accrue on the context as its operators run, so the caller
// reads them from the context after draining.
func drain(op plan.Op, ctx *exec.Ctx) error {
	cur, err := exec.Open(op, ctx)
	if err != nil {
		return err
	}
	for {
		_, ok, err := cur.Next()
		if err != nil {
			_ = cur.Close()
			return err
		}
		if !ok {
			break
		}
	}
	return cur.Close()
}

// internWriteNames interns every label, relationship type, and property key a
// statement's write clauses introduce, so the subsequent bind resolves them to
// known tokens. It interns inside the open write transaction (tx.Intern), so the
// new tokens are part of this transaction and roll back with it on abort (doc 13
// §16); interning is idempotent, so a name already in the catalog is a no-op. It
// touches only write clauses: a name read by MATCH must stay unknown when absent,
// matching nothing rather than being created.
func internWriteNames(tx engine.Tx, q *ast.Query) error {
	for _, sq := range singleQueries(q) {
		for _, c := range sq.Clauses {
			switch cl := c.(type) {
			case *ast.Create:
				for _, pp := range cl.Patterns {
					if err := internPatternNames(tx, pp); err != nil {
						return err
					}
				}
			case *ast.Merge:
				if err := internMergeNames(tx, cl); err != nil {
					return err
				}
			case *ast.Set:
				if err := internSetItemNames(tx, cl.Items); err != nil {
					return err
				}
			case *ast.Foreach:
				if err := internForeachNames(tx, cl); err != nil {
					return err
				}
			}
			// REMOVE interns nothing: an unknown label or key names no stored
			// element, so it stays unresolved and removes nothing rather than
			// being created (doc 13 §7).
		}
	}
	return nil
}

// internSetItemNames interns the static names a list of SET items introduces, the
// body of a SET clause and of MERGE's ON CREATE / ON MATCH parts. The map forms
// (SET n = m, SET n += m) carry no static key, so the switch skips them; their
// keys come from the value at run time and the executor interns them inside the
// write transaction through Tx.Intern (doc 13 §6.4).
func internSetItemNames(tx engine.Tx, items []ast.SetItem) error {
	for _, it := range items {
		switch it.Op {
		case ast.SetProperty:
			if err := internName(tx, catalog.KindPropKey, it.Key); err != nil {
				return err
			}
		case ast.SetLabels:
			for _, l := range it.Labels {
				if err := internName(tx, catalog.KindLabel, l); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// internMergeNames interns the names a MERGE clause introduces: the labels, types,
// and property keys of its pattern (it may create the whole pattern, so its names
// must resolve like CREATE's, doc 13 §11.2) and the static names of its ON CREATE
// and ON MATCH set items.
func internMergeNames(tx engine.Tx, m *ast.Merge) error {
	if err := internPatternNames(tx, m.Pattern); err != nil {
		return err
	}
	if err := internSetItemNames(tx, m.OnCreate); err != nil {
		return err
	}
	return internSetItemNames(tx, m.OnMatch)
}

// internForeachNames interns the static names a FOREACH body introduces. The body
// holds only write clauses (doc 13 §10.2); each is interned like its top-level
// form, and a nested FOREACH recurses. REMOVE and DELETE introduce no names.
func internForeachNames(tx engine.Tx, f *ast.Foreach) error {
	for _, c := range f.Body {
		switch cl := c.(type) {
		case *ast.Create:
			for _, pp := range cl.Patterns {
				if err := internPatternNames(tx, pp); err != nil {
					return err
				}
			}
		case *ast.Merge:
			if err := internMergeNames(tx, cl); err != nil {
				return err
			}
		case *ast.Set:
			if err := internSetItemNames(tx, cl.Items); err != nil {
				return err
			}
		case *ast.Foreach:
			if err := internForeachNames(tx, cl); err != nil {
				return err
			}
		}
	}
	return nil
}

// internPatternNames interns the names of one CREATE path pattern: each node's
// labels and property keys, and each relationship's type and property keys.
func internPatternNames(tx engine.Tx, pp *ast.PathPattern) error {
	if err := internNode(tx, pp.Start); err != nil {
		return err
	}
	for _, step := range pp.Chain {
		for _, ty := range step.Rel.Types {
			if err := internName(tx, catalog.KindRelType, ty); err != nil {
				return err
			}
		}
		if err := internProps(tx, step.Rel.Properties); err != nil {
			return err
		}
		if err := internNode(tx, step.Node); err != nil {
			return err
		}
	}
	return nil
}

func internNode(tx engine.Tx, np *ast.NodePattern) error {
	for _, l := range np.Labels {
		if err := internName(tx, catalog.KindLabel, l); err != nil {
			return err
		}
	}
	return internProps(tx, np.Properties)
}

func internProps(tx engine.Tx, props []ast.PropEntry) error {
	for _, pe := range props {
		if err := internName(tx, catalog.KindPropKey, pe.Key); err != nil {
			return err
		}
	}
	return nil
}

// internName ensures a name is in the catalog as part of this write transaction.
// tx.Intern is idempotent, returning the existing token for a name already present
// and appending one only for a new name, so this needs no Lookup guard.
func internName(tx engine.Tx, kind catalog.Kind, name string) error {
	_, err := tx.Intern(kind, name)
	return err
}

// queryHasWrites reports whether a statement contains any write clause, so Query
// can reject one and run it where it belongs (Exec, over a write transaction).
func queryHasWrites(q *ast.Query) bool {
	for _, sq := range singleQueries(q) {
		for _, c := range sq.Clauses {
			switch c.(type) {
			case *ast.Create, *ast.Merge, *ast.Set, *ast.Remove, *ast.Delete, *ast.Foreach:
				return true
			}
		}
	}
	return false
}

// singleQueries returns a statement's UNION arms in order, the leading query then
// each tail, so a walk over every clause needs no special-casing of the head.
func singleQueries(q *ast.Query) []*ast.SingleQuery {
	out := []*ast.SingleQuery{q.First}
	for _, tail := range q.Rest {
		out = append(out, tail.Query)
	}
	return out
}

// summaryOf projects the executor's side-effect counters onto the public summary.
func summaryOf(e *exec.SideEffects) Summary {
	return Summary{
		NodesCreated:         e.NodesCreated,
		NodesDeleted:         e.NodesDeleted,
		RelationshipsCreated: e.RelsCreated,
		RelationshipsDeleted: e.RelsDeleted,
		PropertiesSet:        e.PropertiesSet,
		LabelsAdded:          e.LabelsAdded,
		LabelsRemoved:        e.LabelsRemoved,
		IndexesAdded:         e.IndexesAdded,
		IndexesRemoved:       e.IndexesRemoved,
		ConstraintsAdded:     e.ConstraintsAdded,
		ConstraintsRemoved:   e.ConstraintsRemoved,
	}
}

// compile returns the compiled query for the given text, from the plan cache on a
// hit or by parsing, binding, and planning on a miss (then caching the result).
// The cache key pairs the normalized text with the engine's catalog version, so a
// schema change misses the entries bound against the old catalog (doc 14 §8.4).
//
// A schema change is not the only thing that can stale a plan: a cost decision baked
// into a cached plan can go stale as the data drifts under a fixed schema (doc 11
// §7). So a cache hit also checks whether the live statistics have drifted far enough
// from the plan's basis to re-plan; if they have, the entry is recompiled and
// replaced, which resets the basis so the check does not re-fire until the data
// drifts again. Drift is a relative-fraction test, so uniform growth does not trigger
// it, and a re-plan on a false positive only costs a compile, never correctness.
func (db *DB) compile(cypher string) (*plan.Entry, error) {
	key := plan.Key{Text: plan.NormalizeText(cypher), Catalog: db.eng.CatalogVersion()}
	st := engineStats{db.eng}
	if entry, ok := db.cache.Get(key); ok && !plan.Drifted(entry.Stats, st, db.driftFactor) {
		return entry, nil
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return nil, err
	}
	if q.Explain {
		// EXPLAIN returns a plan listing, which the cache-backed read path is not
		// shaped to carry; it runs through Run (doc 25 §7.2).
		return nil, ErrExplain
	}
	if q.Schema != nil {
		return nil, ErrSchemaCommand
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(db.eng), false)
	if err != nil {
		return nil, err
	}
	op := plan.SeekRewrite(plan.PlanWithStats(b, st), b, indexLookup{db.eng}, st)
	entry := &plan.Entry{Bound: b, Op: op, Stats: plan.Snapshot(op, st)}
	db.cache.Put(key, entry)
	return entry, nil
}

// explain binds and plans a statement without interning, executing, or otherwise
// touching the database, then returns the operator tree as a result whose single
// "plan" column lists one operator per row (doc 25 §7.2). It is side-effect free
// for a write statement as much as a read: the write's plan is rendered, never run,
// so EXPLAIN CREATE shows the plan and creates nothing. A schema command has no
// operator plan, so EXPLAIN of one is rejected.
//
// cat is the catalog the bind resolves names against, ix the index oracle the seek
// rewrite consults, and st the statistics the cost model estimates cardinalities
// from. The auto-commit path passes the engine, its index lookup, and its
// statistics; a write transaction, which already holds the engine lock, passes its
// own catalog view and a nil ix and nil st, to skip the seek rewrite and the
// estimates, exactly as its execution path skips the index oracle, so EXPLAIN inside
// a write transaction cannot deadlock against the lock the transaction holds. When
// st is nil the listing shows the plan without per-operator row estimates.
func (db *DB) explain(q *ast.Query, cat bind.TokenResolver, ix plan.IndexLookup, st plan.Statistics) (*Result, error) {
	if q.Schema != nil {
		return nil, ErrExplainSchema
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(cat), false)
	if err != nil {
		return nil, err
	}
	op := plan.PlanWithStats(b, st)
	if ix != nil {
		op = plan.SeekRewrite(op, b, ix, st)
	}
	return explainResult(op, st), nil
}

// explainResult renders an operator tree into a streaming result: one column named
// "plan" and one row per line of the tree, so a caller iterates the listing the way
// it iterates any other result. With statistics it annotates each operator with the
// rows the cost model estimates; without them it renders the bare tree. The trailing
// newline is trimmed so the listing has no blank final row.
func explainResult(op plan.Op, st plan.Statistics) *Result {
	var text string
	if st != nil {
		text = plan.StringWithRows(op, st)
	} else {
		text = plan.String(op)
	}
	text = strings.TrimRight(text, "\n")
	lines := strings.Split(text, "\n")
	buf := make([]eval.Row, len(lines))
	for i, ln := range lines {
		buf[i] = eval.Row{"plan": value.String(ln)}
	}
	return &Result{cols: []string{"plan"}, buf: buf}
}

// indexLookup adapts the engine to the planner's IndexLookup seam, mapping the
// planner's raw token integers to the engine's Token type. The plan cache keys on
// the catalog version, which bumps on an index add or drop, so a plan built from
// these answers is invalidated when an index appears or disappears.
type indexLookup struct{ eng *engine.DiskEngine }

func (ix indexLookup) HasNodeIndex(label, prop uint32) bool {
	return ix.eng.HasNodeIndex(engine.Token(label), engine.Token(prop))
}

// engineStats adapts the engine to the planner's Statistics seam, mapping the
// planner's raw token integers to the engine's Token type and its uint64 counts to
// the float64 the cost model works in. Like indexLookup it reads the engine's
// committed catalog counters, so the plan cache, keyed on the catalog version,
// invalidates a plan costed against counts that have since moved across a schema
// change. It must not be used while a write transaction holds the engine lock, since
// the count methods take the read lock and would deadlock against the held write
// lock, the same restriction the index oracle has on that path.
type engineStats struct{ eng *engine.DiskEngine }

func (s engineStats) NodeCount() float64 { return float64(s.eng.NodeCount()) }

func (s engineStats) RelCount() float64 { return float64(s.eng.RelCount()) }

func (s engineStats) LabelCount(label uint32) float64 {
	n, err := s.eng.NodeCountByLabel(engine.Token(label))
	if err != nil {
		return 0
	}
	return float64(n)
}

func (s engineStats) RelTypeCount(relType uint32) float64 {
	n, err := s.eng.RelCountByType(engine.Token(relType))
	if err != nil {
		return 0
	}
	return float64(n)
}

// tokenNamer returns a reverse resolver for one catalog kind: a token to the name
// it interns. It backs eval's entity functions (labels, type, keys, properties),
// which return names rather than tokens (doc 09 §7).
func (db *DB) tokenNamer(kind catalog.Kind) func(t engine.Token) (string, bool) {
	return func(t engine.Token) (string, bool) {
		return db.eng.TokenName(kind, t)
	}
}

// Result is a streaming Cypher query result: the output column names in order and
// a Next/Close pair that pulls one row of column values at a time. An auto-commit
// Query owns its read transaction and releases it on Close; a Result from a
// managed transaction's Run borrows that transaction and leaves it for the caller
// to commit or roll back, so Close only releases the cursor (ownTx is false).
//
// A read streams lazily from a cursor. A write statement run through a managed
// transaction's Run executes eagerly and materializes its RETURN rows into buf, so
// every mutation lands before Run returns (a half-consumed write would otherwise
// leave the statement partly applied at commit); the result then iterates the
// buffer and reports the mutation counts through Summary.
type Result struct {
	cols    []string
	cursor  *exec.Cursor
	buf     []eval.Row
	bufIdx  int
	summary Summary
	tx      engine.Tx
	ownTx   bool
}

// Columns returns the result's output column names in order.
func (r *Result) Columns() []string { return r.cols }

// next pulls the next row from whichever backing the result has: the live cursor
// for a read, or the materialized buffer for an eagerly executed write.
func (r *Result) next() (eval.Row, bool, error) {
	if r.cursor != nil {
		return r.cursor.Next()
	}
	if r.bufIdx >= len(r.buf) {
		return nil, false, nil
	}
	row := r.buf[r.bufIdx]
	r.bufIdx++
	return row, true, nil
}

// Next pulls the next result row as a slice of column values aligned to Columns,
// returning ok false at the end of the stream. A column absent from the row binds
// to the null value, the schema-optional reading rule (doc 08 §5.3).
func (r *Result) Next() ([]value.Value, bool, error) {
	row, ok, err := r.next()
	if err != nil || !ok {
		return nil, ok, err
	}
	out := make([]value.Value, len(r.cols))
	for i, c := range r.cols {
		out[i] = row[c]
	}
	return out, true, nil
}

// NextRow pulls the next result row as a name-keyed map, for callers that prefer
// lookup by column name over positional access. It shares the backing with Next.
func (r *Result) NextRow() (eval.Row, bool, error) { return r.next() }

// Summary reports the graph mutations a write statement run through Run performed
// (doc 13 §3). It is the zero summary for a read result, which mutates nothing, and
// it is complete as soon as Run returns, since a write executes eagerly.
func (r *Result) Summary() Summary { return r.summary }

// Close releases the result's cursor. For an auto-commit Query result it also
// aborts the read transaction it owns; for a managed-transaction Run result it
// leaves the borrowed transaction untouched, since the caller commits or rolls it
// back. It is safe to call more than once.
func (r *Result) Close() error {
	if r.cursor == nil {
		return nil
	}
	cerr := r.cursor.Close()
	var terr error
	if r.ownTx && r.tx != nil {
		terr = r.tx.Abort()
	}
	r.cursor, r.tx = nil, nil
	if cerr != nil {
		return cerr
	}
	return terr
}

// Close flushes and closes the database. Callers that want a sidecar-free file
// should ensure all work is committed first; a clean checkpoint runs on the last
// commit so a quiescent database has no live WAL frames.
func (db *DB) Close() error {
	if db.eng == nil {
		return nil
	}
	err := db.eng.Close()
	db.eng = nil
	return err
}
