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

// DB is an open gr database. It owns the storage engine, which owns the pager
// over the underlying file; queries run against snapshots the engine hands out.
// It also owns the plan cache, so a repeated query shape reuses its compiled plan.
type DB struct {
	path  string
	eng   *engine.DiskEngine
	cache *plan.Cache
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
	return &DB{path: path, eng: eng, cache: plan.NewCache(opt.PlanCacheSize)}, nil
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
	ctx := &exec.Ctx{
		Tx:          tx,
		Params:      params,
		Resolve:     exec.ResolverFromBound(entry.Bound),
		LabelName:   db.tokenNamer(catalog.KindLabel),
		RelTypeName: db.tokenNamer(catalog.KindRelType),
		PropKeyName: db.tokenNamer(catalog.KindPropKey),
	}
	cur, err := exec.Open(entry.Op, ctx)
	if err != nil {
		_ = tx.Abort()
		return nil, err
	}
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx}, nil
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
// The names a CREATE introduces (labels, relationship types, property keys) are
// interned before the transaction begins, because interning is its own durable
// transaction and the engine's write lock is not reentrant (doc 13 §9). A
// statement that turns out to write nothing still runs; a read-only statement runs
// too and reports an empty summary, so Exec is a superset of Query for callers that
// do not need streamed rows.
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
	if err := db.internWriteNames(q); err != nil {
		return Summary{}, err
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(db.eng), false)
	if err != nil {
		return Summary{}, err
	}
	op := plan.Plan(b)
	tx, err := db.eng.Begin(true)
	if err != nil {
		return Summary{}, err
	}
	eff := &exec.SideEffects{}
	ctx := &exec.Ctx{
		Tx:          tx,
		Params:      params,
		Resolve:     exec.ResolverFromBound(b),
		LabelName:   db.tokenNamer(catalog.KindLabel),
		RelTypeName: db.tokenNamer(catalog.KindRelType),
		PropKeyName: db.tokenNamer(catalog.KindPropKey),
		Effects:     eff,
	}
	if err := drain(op, ctx); err != nil {
		_ = tx.Abort()
		return Summary{}, err
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, err
	}
	return summaryOf(eff), nil
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
// known tokens. It interns nothing for a name already in the catalog (interning is
// idempotent but commits a durable transaction, so the Lookup avoids needless
// commits) and touches only write clauses (a name read by MATCH must stay unknown
// when absent, matching nothing rather than being created).
func (db *DB) internWriteNames(q *ast.Query) error {
	for _, sq := range singleQueries(q) {
		for _, c := range sq.Clauses {
			switch cl := c.(type) {
			case *ast.Create:
				for _, pp := range cl.Patterns {
					if err := db.internPatternNames(pp); err != nil {
						return err
					}
				}
			case *ast.Set:
				if err := db.internSetNames(cl); err != nil {
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

// internSetNames interns the names a SET clause introduces: the property key of
// each single-property assignment and every label of each label addition. The
// map forms (SET n = m, SET n += m) carry no static key, so the switch skips
// them; their keys come from the value at run time and the executor interns them
// inside the write transaction through Tx.InternPropKey (doc 13 §6.4).
func (db *DB) internSetNames(s *ast.Set) error {
	for _, it := range s.Items {
		switch it.Op {
		case ast.SetProperty:
			if err := db.internName(catalog.KindPropKey, it.Key); err != nil {
				return err
			}
		case ast.SetLabels:
			for _, l := range it.Labels {
				if err := db.internName(catalog.KindLabel, l); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// internPatternNames interns the names of one CREATE path pattern: each node's
// labels and property keys, and each relationship's type and property keys.
func (db *DB) internPatternNames(pp *ast.PathPattern) error {
	if err := db.internNode(pp.Start); err != nil {
		return err
	}
	for _, step := range pp.Chain {
		for _, ty := range step.Rel.Types {
			if err := db.internName(catalog.KindRelType, ty); err != nil {
				return err
			}
		}
		if err := db.internProps(step.Rel.Properties); err != nil {
			return err
		}
		if err := db.internNode(step.Node); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) internNode(np *ast.NodePattern) error {
	for _, l := range np.Labels {
		if err := db.internName(catalog.KindLabel, l); err != nil {
			return err
		}
	}
	return db.internProps(np.Properties)
}

func (db *DB) internProps(props []ast.PropEntry) error {
	for _, pe := range props {
		if err := db.internName(catalog.KindPropKey, pe.Key); err != nil {
			return err
		}
	}
	return nil
}

// internName ensures a name is in the catalog, interning it only when absent.
func (db *DB) internName(kind catalog.Kind, name string) error {
	if _, ok := db.eng.Lookup(kind, name); ok {
		return nil
	}
	_, err := db.eng.Intern(kind, name)
	return err
}

// queryHasWrites reports whether a statement contains any write clause, so Query
// can reject one and run it where it belongs (Exec, over a write transaction).
func queryHasWrites(q *ast.Query) bool {
	for _, sq := range singleQueries(q) {
		for _, c := range sq.Clauses {
			switch c.(type) {
			case *ast.Create, *ast.Set, *ast.Remove, *ast.Delete:
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
func (db *DB) compile(cypher string) (*plan.Entry, error) {
	key := plan.Key{Text: plan.NormalizeText(cypher), Catalog: db.eng.CatalogVersion()}
	if entry, ok := db.cache.Get(key); ok {
		return entry, nil
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		return nil, err
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(db.eng), false)
	if err != nil {
		return nil, err
	}
	entry := &plan.Entry{Bound: b, Op: plan.Plan(b)}
	db.cache.Put(key, entry)
	return entry, nil
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
// a Next/Close pair that pulls one row of column values at a time. It owns the
// read transaction the query runs against, released by Close.
type Result struct {
	cols   []string
	cursor *exec.Cursor
	tx     engine.Tx
}

// Columns returns the result's output column names in order.
func (r *Result) Columns() []string { return r.cols }

// Next pulls the next result row as a slice of column values aligned to Columns,
// returning ok false at the end of the stream. A column absent from the row binds
// to the null value, the schema-optional reading rule (doc 08 §5.3).
func (r *Result) Next() ([]value.Value, bool, error) {
	row, ok, err := r.cursor.Next()
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
// lookup by column name over positional access. It shares the cursor with Next.
func (r *Result) NextRow() (eval.Row, bool, error) { return r.cursor.Next() }

// Close releases the result's cursor and aborts its read transaction. It is safe
// to call more than once.
func (r *Result) Close() error {
	if r.cursor == nil {
		return nil
	}
	cerr := r.cursor.Close()
	terr := r.tx.Abort()
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
