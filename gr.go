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

	"github.com/tamnd/gr/bind"
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
	tx, err := db.eng.Begin(false)
	if err != nil {
		return nil, err
	}
	ctx := &exec.Ctx{
		Tx:      tx,
		Params:  params,
		Resolve: exec.ResolverFromBound(entry.Bound),
	}
	cur, err := exec.Open(entry.Op, ctx)
	if err != nil {
		_ = tx.Abort()
		return nil, err
	}
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx}, nil
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
