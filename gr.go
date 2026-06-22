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
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// ErrNotFound is returned by NodeByElementId and RelationshipByElementId when no
// element with the given id is visible under the transaction's snapshot, and for an
// id string that is not a well-formed element id (doc 16 §10.7). A program tells "no
// such element" from a real read failure by comparing against this sentinel.
var ErrNotFound = errors.New("gr: no element with that id")

// ErrReadQuery is returned by Query when the statement contains a write clause,
// and by Exec when, conversely, a read-only statement is run where a write is
// expected (a read-only Exec is allowed, but Query rejects writes outright).
var ErrReadQuery = errors.New("gr: Query is read-only; use Exec for a write statement")

// ErrSchemaCommand is returned by Query when the statement is a schema command
// (CREATE CONSTRAINT, DROP CONSTRAINT); those run through Exec, which carries the
// write transaction and reports the schema mutation in its summary.
var ErrSchemaCommand = errors.New("gr: schema commands run through Exec, not Query")

// ErrAdminCommand is returned by Query when the statement is an administrative statement
// (CREATE USER, GRANT ROLE, SHOW USERS); those run through Run, which routes them to the
// credential API (doc 18 §10, §12.3).
var ErrAdminCommand = errors.New("gr: administrative statements run through Run, not Query")

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
	path  string
	eng   *engine.DiskEngine
	cache *plan.Cache

	// cfgMu guards the live-settable session knobs below (maxRetries, driftFactor,
	// memBudget, lazyProps), which a PRAGMA set form changes on an open connection (doc
	// 24 §3.4). A statement reads them through the accessor methods in pragma.go, so a
	// concurrent PRAGMA and a concurrent query do not race on the field. readOnly is fixed
	// at Open and never set live, so it sits outside the lock.
	cfgMu       sync.RWMutex
	maxRetries  int
	driftFactor float64

	// readOnly records whether the database was opened read-only (doc 16 §3.4); it backs
	// the read-only read_only pragma and never changes after Open.
	readOnly bool

	// fsys is the filesystem the database was opened through, kept so a query that
	// outgrows its memory budget can open spill files in the same namespace as the
	// database file (doc 12 §9.2). memBudget is the per-operator byte ceiling those
	// queries run under; zero leaves every operator in memory, the default. tmpSeq
	// names spill files uniquely within this open database.
	fsys      vfs.VFS
	memBudget int64
	tmpSeq    atomic.Uint64

	// events is the structured operational-event log (doc 20 §11): open, close, and the
	// rest of the taxonomy. A nil log is disabled, the embedded default, so every emit
	// site calls it without a guard. It is set at Open and read for the close event.
	events *EventLog

	// metrics is the database's metric registry and the pre-resolved query-metric handles
	// (doc 20 §3.1). It is always built at Open, so db.Metrics never returns nil and the
	// always-on collection runs whether or not a query log or event log is configured.
	metrics *queryMetrics

	// lazyProps is the open-time default for property materialization (doc 16 §10.6):
	// false (the default) materializes a graph object's properties eagerly when its
	// record is produced, so the object outlives its transaction; true defers each
	// property read to first access from the transaction's snapshot. A per-run
	// WithLazyProperties overrides it for one statement.
	lazyProps bool
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
	// MemBudget bounds the bytes a single buffering operator (today a hash join's
	// build side) may hold before it spills to temp files alongside the database
	// (doc 12 §9). 0, the default, leaves every operator in memory and never spills,
	// so the answers and the in-memory fast path are unchanged; set it to cap the
	// memory a large join uses at the cost of disk I/O for the overflow.
	MemBudget int64
	// LazyProperties sets the database's default for graph-object property
	// materialization (doc 16 §10.6). The zero value, false, materializes a node's or
	// relationship's properties eagerly when its record is produced, so the object
	// keeps its properties after its transaction ends; true defers each property read
	// to first access from the transaction's snapshot, which is cheaper when most
	// returned objects are never inspected but ties their property reads to the
	// transaction's lifetime. A single statement can override this with
	// WithLazyProperties.
	LazyProperties bool
	// EventLog receives the structured operational-event stream (doc 20 §11): the open
	// and close events, and (as the subsystems gain hooks) recovery, checkpoint, and the
	// rest of the taxonomy. nil, the default, disables it, so an embedded database logs
	// nothing until the embedder points it at an slog handler. Build one with
	// [NewEventLog].
	EventLog *EventLog
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
	db := &DB{
		path:        path,
		eng:         eng,
		cache:       plan.NewCache(opt.PlanCacheSize),
		maxRetries:  maxRetries,
		driftFactor: driftFactor,
		fsys:        fsys,
		memBudget:   opt.MemBudget,
		lazyProps:   opt.LazyProperties,
		readOnly:    opt.ReadOnly,
		events:      opt.EventLog,
		metrics:     newQueryMetrics(),
	}
	// Wire the plan-cache metrics to the cache now both exist (doc 20 §3.2): the eviction hook
	// counts LRU drops, and a computed gauge reads the resident plan count at snapshot time.
	db.cache.OnEvict = func(reason string) { db.metrics.recordCacheEviction(reason) }
	db.metrics.reg.ComputedGauge("gr_plan_cache_entries",
		"Cached plans currently resident", "plans", nil, func() int64 { return int64(db.cache.Len()) })
	// The expand metrics label by relationship-type name, so the observer needs the catalog's
	// token-to-name resolver (doc 20 §6.1). Wire it now the engine exists; an expand before this
	// would fall back to the all-types bucket, but no query runs during Open.
	db.metrics.relTypeName = db.tokenNamer(catalog.KindRelType)
	// The index-lookup metric labels by index name, so the observer needs the (label, property)
	// to index-name resolver (doc 20 §6.4). Wire it now the engine exists.
	db.metrics.indexNameOf = db.indexNamer()
	// The commit path counts each constraint check it runs, so give the engine the observer that
	// feeds gr_constraint_checks_total (doc 20 §6.4). A database with no declared constraints never
	// calls it, so this costs nothing until a constraint exists.
	db.eng.SetConstraintObserver(constraintObserver{db.metrics})
	// Register the per-index entry gauge for any index this open already carries (doc 20 §6.4), so a
	// reopened database with indexes exposes their sizes from the first scrape. New indexes register
	// lazily on the next snapshot.
	db.syncIndexGauges()
	// The on-disk footprint is a single computed gauge reading the size the engine publishes under
	// its lock at open and each commit (doc 20 §4.2), so the read is lock-free and the value tracks
	// the file growing without the write path touching the registry.
	db.metrics.reg.ComputedGauge("gr_file_size_bytes",
		"Current size of the main .gr file", "bytes", nil, func() int64 { return db.eng.FileSizeBytes() })
	// Reusable space on the free list is a sibling computed gauge over the count the engine
	// publishes at commit, so a large free list after a delete shows the space compaction can
	// reclaim (doc 20 §4.2).
	db.metrics.reg.ComputedGauge("gr_freelist_pages",
		"Pages on the free list, reusable space", "pages", nil, func() int64 { return db.eng.FreelistPages() })
	// The version-store size is a computed gauge reading the overlay's retained pre-image count (doc
	// 20 §5.1). It reads the overlay's own lock, never the engine lock, so the snapshot stays off the
	// write lock; a value that climbs under a read-heavy-plus-write load is the long-reader pinning
	// history GC cannot reclaim (§16.4).
	db.metrics.reg.ComputedGauge("gr_mvcc_versions_resident",
		"Element versions held beyond the current committed version, the version-store size", "versions", nil,
		func() int64 { return db.eng.VersionsResident() })
	// The watermark lag is the reclaimable backlog: the commit versions GC could drop the moment the
	// oldest live snapshot releases (doc 20 §5.1). It reads the oracle's lock, never the engine lock,
	// so a lag that stays high while a reader holds a snapshot is the long-reader signal (§16.4).
	db.metrics.reg.ComputedGauge("gr_mvcc_watermark_lag_versions",
		"Commit versions between the newest commit and the GC watermark, the reclaimable backlog", "versions", nil,
		func() int64 { return db.eng.WatermarkLag() })
	// The oldest snapshot's age is the same long-reader signal in time: a reader pinning the watermark
	// shows here as an age that climbs while it stays open (doc 20 §5.1). It reads the oracle's lock, so
	// the metrics path is free to read it during a write, and it is truncated to whole seconds.
	db.metrics.reg.ComputedGauge("gr_mvcc_oldest_snapshot_age_seconds",
		"Wall-clock age of the oldest live snapshot, the long-reader signal in time", "seconds", nil,
		func() int64 { return db.eng.OldestSnapshotAgeSeconds() })
	// The open event reports the file's real geometry and whether this open recovered a
	// committed WAL prefix after a crash (doc 20 §11.3). StorageInfo reads the header the
	// engine just mounted, so the format version and page size are the file's own.
	if info, err := eng.StorageInfo(); err == nil {
		db.events.Open(path, info.FormatVersion, info.PageSize, eng.Recovered())
	} else {
		db.events.Open(path, 0, eng.PageSize(), eng.Recovered())
	}
	return db, nil
}

// RunOption tunes one statement run without changing the database's open-time
// defaults (doc 16 §10.6). It is the variadic-option seam Run and a transaction's Run
// take, so a caller threads a per-statement setting through the same call it already
// makes.
type RunOption func(*runConfig)

// runConfig holds the resolved per-run settings: the open-time defaults with any
// RunOption applied on top.
type runConfig struct {
	lazy bool
}

// WithLazyProperties overrides property materialization for one statement (doc 16
// §10.6): true defers each graph object's property reads to first access from the
// transaction's snapshot, false materializes them eagerly when the record is
// produced so the object outlives the transaction. It overrides the database's
// Options.LazyProperties for this run only.
func WithLazyProperties(lazy bool) RunOption {
	return func(c *runConfig) { c.lazy = lazy }
}

// resolveRun folds a statement's RunOptions onto the database's open-time defaults.
func (db *DB) resolveRun(opts []RunOption) runConfig {
	c := runConfig{lazy: db.lazyDefault()}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// materializer builds the graph-object materializer for a result: the snapshot to
// read structural attributes and (eagerly) properties from, plus the three reverse
// token namers that turn catalog tokens into names (doc 16 §10.2). lazy carries the
// resolved per-run materialization mode.
func (db *DB) materializer(tx engine.Tx, lazy bool) *objectMaterializer {
	return &objectMaterializer{
		tx:          tx,
		labelName:   db.tokenNamer(catalog.KindLabel),
		relTypeName: db.tokenNamer(catalog.KindRelType),
		propKeyName: db.tokenNamer(catalog.KindPropKey),
		lazy:        lazy,
	}
}

// tempFile opens a fresh, empty spill file in the database's filesystem and
// returns it with a discard closure that closes and removes it (the seam
// [exec.Ctx.TempFile] expects, doc 12 §9.2). The name is unique within this open
// database and sits beside the database file, so the spill area inherits the same
// filesystem and permissions. The file is truncated on open so a name left behind
// by an earlier process start cannot hand back stale bytes.
func (db *DB) tempFile() (vfs.File, func() error, error) {
	name := fmt.Sprintf("%s-spill-%d.tmp", db.path, db.tmpSeq.Add(1))
	f, err := db.fsys.Open(name, true)
	if err != nil {
		return nil, nil, err
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		_ = db.fsys.Remove(name)
		return nil, nil, err
	}
	discard := func() error {
		cerr := f.Close()
		rerr := db.fsys.Remove(name)
		if cerr != nil {
			return cerr
		}
		return rerr
	}
	return f, discard, nil
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// PageSize returns the file's page size in bytes.
func (db *DB) PageSize() uint32 { return db.eng.PageSize() }

// IndexInfo describes a schema index with its label and property names resolved (doc
// 16 §11, doc 17 §6.5). It is the engine's IndexInfo re-exported so a caller listing
// the schema does not import the engine package.
type IndexInfo = engine.IndexInfo

// Labels returns every node label the catalog holds, in interning order (doc 16 §11).
// These are the names the database has ever seen, the schema-introspection surface the
// CLI's .labels command and a program's schema browser read.
func (db *DB) Labels() ([]string, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	return db.eng.Labels(), nil
}

// RelationshipTypes returns every relationship type the catalog holds (doc 16 §11).
func (db *DB) RelationshipTypes() ([]string, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	return db.eng.RelationshipTypes(), nil
}

// PropertyKeys returns every property key the catalog holds (doc 16 §11).
func (db *DB) PropertyKeys() ([]string, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	return db.eng.PropertyKeys(), nil
}

// Indexes returns the schema indexes with their label and property names resolved
// (doc 16 §11).
func (db *DB) Indexes() ([]IndexInfo, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	return db.eng.IndexInfos(), nil
}

// ConstraintInfo describes a schema constraint with its names resolved (doc 08 §4).
type ConstraintInfo = engine.ConstraintInfo

// Constraints returns the schema constraints with their label, property names, and
// kind resolved, the read behind a dump's DDL section and the schema browser (doc 08
// §4, doc 17 §13.2).
func (db *DB) Constraints() ([]ConstraintInfo, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	return db.eng.ConstraintInfos(), nil
}

// DBInfo is the database's static and structural nameplate behind `.info` /
// `gr info` (doc 17 §6.15): the format version and page geometry, the size and
// free-page count, the catalog token counts, the live element counts, and the
// index and constraint counts.
type DBInfo struct {
	Path          string
	FormatVersion uint32
	PageSize      uint32
	PageCount     uint64
	FreePages     uint64
	SizeBytes     int64
	Labels        int
	RelTypes      int
	PropertyKeys  int
	Nodes         int
	Relationships int
	Indexes       int
	Constraints   int
	UniqueCons    int
}

// Backup writes a consistent physical image of the database to w and returns the
// number of bytes written (doc 16, doc 17 §6.13). The image is a standalone .gr
// file that opens directly, the fast same-version copy that complements the
// portable logical dump. It is safe to call while the database is open; the
// engine pins a single committed snapshot for the copy.
func (db *DB) Backup(w io.Writer) (int64, error) {
	if db.eng == nil {
		return 0, ErrClosed
	}
	return db.eng.Backup(w)
}

// Info gathers the database's static and structural facts (doc 17 §6.15). The
// geometry and free-page count come from the engine's storage info, the catalog
// counts and index and constraint counts from the introspection surface, and the
// live element counts from a snapshot count query, so a deleted element's slot is
// not counted.
func (db *DB) Info() (DBInfo, error) {
	if db.eng == nil {
		return DBInfo{}, ErrClosed
	}
	si, err := db.eng.StorageInfo()
	if err != nil {
		return DBInfo{}, err
	}
	info := DBInfo{
		Path:          db.path,
		FormatVersion: si.FormatVersion,
		PageSize:      si.PageSize,
		PageCount:     si.PageCount,
		FreePages:     si.FreePages,
		SizeBytes:     si.SizeBytes,
		Labels:        len(db.eng.Labels()),
		RelTypes:      len(db.eng.RelationshipTypes()),
		PropertyKeys:  len(db.eng.PropertyKeys()),
		Indexes:       len(db.eng.IndexInfos()),
	}
	for _, c := range db.eng.ConstraintInfos() {
		info.Constraints++
		if c.Kind == "UNIQUE" {
			info.UniqueCons++
		}
	}
	err = db.View(func(tx *Tx) error {
		n, err := countRows(tx, "MATCH (n) RETURN count(n)")
		if err != nil {
			return err
		}
		r, err := countRows(tx, "MATCH ()-[r]->() RETURN count(r)")
		if err != nil {
			return err
		}
		info.Nodes, info.Relationships = n, r
		return nil
	})
	if err != nil {
		return DBInfo{}, err
	}
	return info, nil
}

// countRows runs a single-column count query on a transaction and returns the count.
func countRows(tx *Tx, cypher string) (int, error) {
	r, err := tx.Run(context.Background(), cypher, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	if !r.Next() {
		return 0, nil
	}
	n, err := r.Record().GetInt(r.Keys()[0])
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

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
	// Query is the read-only entry, so its kind is always read (a write fails with
	// ErrReadQuery and counts as a read error). It does not route through Run, so it
	// records its own query metrics here (doc 20 §3.1).
	start := time.Now()
	db.metrics.begin("read")
	res, err := db.query(cypher, params, db.lazyDefault())
	return db.measureQuery("read", start, res, err)
}

// query is the body of Query with the resolved property-materialization mode threaded
// in, so the auto-commit read path and the per-run Run path (which may override the
// mode with WithLazyProperties) share one implementation.
func (db *DB) query(cypher string, params map[string]value.Value, lazy bool) (*Result, error) {
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
	// The executor span for the streaming read starts here, once the plan is ready and the
	// snapshot is open; Close records the time since it (doc 20 §3.1).
	execStart := time.Now()
	cur, err := exec.Open(entry.Op, db.execCtx(tx, entry.Bound, params))
	if err != nil {
		_ = tx.Abort()
		return nil, err
	}
	return &Result{cols: cur.Cols(), cursor: cur, tx: tx, ownTx: true, mat: db.materializer(tx, lazy), mscan: cur.ScanCount(), mexec: execStart}, nil
}

// execCtx builds the execution context shared by every run of a statement: the
// transaction it runs against, the parameter map, the resolver from the bound
// query, and the reverse token namers the entity functions need. A write run sets
// Effects on the returned context to collect its mutation counts.
func (db *DB) execCtx(etx engine.Tx, b *bind.Bound, params map[string]value.Value) *exec.Ctx {
	ctx := &exec.Ctx{
		Tx:          etx,
		Params:      params,
		Resolve:     exec.ResolverFromBound(b),
		LabelName:   db.tokenNamer(catalog.KindLabel),
		RelTypeName: db.tokenNamer(catalog.KindRelType),
		PropKeyName: db.tokenNamer(catalog.KindPropKey),
		// Arm the scanned-rows counter so the scan and expand operators record their work
		// for gr_query_rows_scanned (doc 20 §3.1). It is a cheap atomic the library reads
		// after the cursor drains, the amplification numerator paired with rows returned.
		Scanned: new(atomic.Int64),
		// Wire the graph-operator metrics (doc 20 §6): the shortest-path, WCOJ, and binary-join
		// operators report through this as they open.
		Graph: graphObserver{db.metrics},
	}
	// Arm spilling only when a budget is configured. With the default zero budget
	// the executor never spills, so leaving TempFile unset keeps the in-memory path.
	// The budget is read once here so a concurrent PRAGMA set does not change it
	// mid-statement (doc 24 §3.4).
	if mb := db.memBudgetVal(); mb > 0 {
		ctx.MemBudget = mb
		ctx.TempFile = db.tempFile
	}
	return ctx
}

// execWriteBuffered interns, binds, plans, and runs a write statement against an
// open transaction to exhaustion, materializing its RETURN rows and collecting its
// mutation counts. It is the shared body of every eagerly executed write: the
// database-level auto-commit Run, and the managed transaction's Run. Running to
// exhaustion before returning is what keeps a write all-or-nothing: every mutation
// lands before the caller sees a row, so a half-consumed result cannot leave the
// statement partly applied at commit. It does not begin, commit, or abort the
// transaction; the caller owns that.
func (db *DB) execWriteBuffered(etx engine.Tx, q *ast.Query, params map[string]value.Value) ([]string, []eval.Row, Summary, *atomic.Int64, error) {
	// A write is never served from the plan cache, so the bind and plan below are a full
	// compile: time them as a plan-cache miss for gr_query_plan_duration_seconds (doc 20 §3.1).
	pstart := time.Now()
	if err := internWriteNames(etx, q); err != nil {
		return nil, nil, Summary{}, nil, err
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(etx), false)
	if err != nil {
		return nil, nil, Summary{}, nil, err
	}
	op := plan.Plan(b)
	db.metrics.recordPlan("miss", time.Since(pstart))
	eff := &exec.SideEffects{}
	ctx := db.execCtx(etx, b, params)
	ctx.Effects = eff
	// The executor span starts here, once the plan is ready, and ends when the cursor drains and
	// closes: it is gr_query_execute_duration_seconds for the write, the executor work alone with
	// parse and plan excluded (doc 20 §3.1).
	estart := time.Now()
	cur, err := exec.Open(op, ctx)
	if err != nil {
		db.metrics.recordExecute("write", time.Since(estart))
		return nil, nil, Summary{}, nil, err
	}
	cols := cur.Cols()
	var buf []eval.Row
	for {
		row, ok, err := cur.Next()
		if err != nil {
			_ = cur.Close()
			db.metrics.recordExecute("write", time.Since(estart))
			return nil, nil, Summary{}, nil, err
		}
		if !ok {
			break
		}
		buf = append(buf, cloneRow(row))
	}
	if err := cur.Close(); err != nil {
		db.metrics.recordExecute("write", time.Since(estart))
		return nil, nil, Summary{}, nil, err
	}
	db.metrics.recordExecute("write", time.Since(estart))
	// The scan counter is final now the cursor is drained and closed, so the caller stores it
	// on the eager result and measureQuery reads it for gr_query_rows_scanned (doc 20 §3.1).
	return cols, buf, summaryOf(eff), ctx.Scanned, nil
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
// params supplies the values for the statement's $-parameters as plain Go values
// (doc 16 §9); a nil map is fine for a parameterless statement, and a value the model
// cannot represent fails with ErrParam before the statement runs. The ctx is honoured
// at entry: a context already cancelled when Run is called returns its error without
// touching the engine. The caller must Close the result; for a write or schema result
// Close has nothing to release, since the implicit transaction has already committed.
func (db *DB) Run(ctx context.Context, cypher string, params Params, opts ...RunOption) (*Result, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := db.resolveRun(opts)
	vals, err := toValues(params)
	if err != nil {
		return nil, err
	}
	q, err := parse.Parse(cypher)
	if err != nil {
		// A parse failure never classifies into a kind, so it is not in gr_queries_total, but
		// it is still a query error and counts in gr_query_errors_total{class="syntax"}.
		db.metrics.recordError(err)
		return nil, err
	}
	// The query metrics (doc 20 §3.1) wrap the dispatch: begin raises the in-flight gauge,
	// runDispatch executes, and measureQuery records the outcome and latency, recording an
	// eager result now and deferring a streaming read to its Close.
	kind := metricQueryKind(q)
	start := time.Now()
	db.metrics.begin(kind)
	res, err := db.runDispatch(q, cypher, vals, cfg)
	return db.measureQuery(kind, start, res, err)
}

// runDispatch routes a parsed statement to its execution path, the body of Run with the
// metric instrumentation lifted out so the throughput, latency, and in-flight metrics record
// once around the whole dispatch (doc 20 §3.1).
func (db *DB) runDispatch(q *ast.Query, cypher string, vals map[string]value.Value, cfg runConfig) (*Result, error) {
	if q.Explain {
		return db.explain(q, db.eng, indexLookup{db.eng}, engineStats{db.eng})
	}
	if q.Admin != nil {
		return db.execAdmin(q.Admin)
	}
	if q.Pragma != nil {
		return db.execPragma(q.Pragma)
	}
	if q.Schema != nil {
		s, err := db.execSchema(q.Schema)
		if err != nil {
			return nil, err
		}
		return &Result{summary: s}, nil
	}
	if queryHasWrites(q) {
		return db.runAutoWrite(q, vals, cfg.lazy)
	}
	return db.query(cypher, vals, cfg.lazy)
}

// runAutoWrite executes a write statement in an implicit write transaction and
// commits it before returning, so the auto-commit Run keeps a single write statement
// all-or-nothing at the statement boundary. The statement runs eagerly to exhaustion
// (execWriteBuffered), so every mutation lands and the RETURN rows are materialized
// before the commit; any error aborts the transaction and leaves the database
// unchanged. The returned result iterates the buffered rows and reports the mutations
// through Summary; it owns no open transaction, so Close has nothing to release.
func (db *DB) runAutoWrite(q *ast.Query, params map[string]value.Value, lazy bool) (*Result, error) {
	tx, err := db.eng.Begin(true)
	if err != nil {
		return nil, err
	}
	cols, buf, summary, scanned, err := db.execWriteBuffered(tx, q, params)
	if err != nil {
		_ = tx.Abort()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	res := &Result{cols: cols, buf: buf, summary: summary, mscan: scanned}
	// A write commits before returning, so its buffered rows have no live snapshot to
	// materialize their graph objects against. When the result carries rows, open a
	// fresh read transaction over the just-committed state and let the result own it,
	// so a returned node or relationship resolves its labels, type, endpoints, and
	// properties; the result aborts this snapshot on Close (doc 16 §10.6). A result
	// with no rows needs no snapshot.
	if len(buf) > 0 {
		if rtx, err := db.eng.Begin(false); err == nil {
			res.tx, res.ownTx = rtx, true
			res.mat = db.materializer(rtx, lazy)
		}
	}
	return res, nil
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
	if q.Admin != nil {
		// A mutation administrative statement reports an empty summary, since it changes
		// no graph data; SHOW USERS yields rows, which the row-less Exec cannot carry.
		if _, isShow := q.Admin.(*ast.ShowUsers); isShow {
			return Summary{}, ErrAdminRows
		}
		res, err := db.execAdmin(q.Admin)
		if err != nil {
			return Summary{}, err
		}
		return res.summary, nil
	}
	if q.Pragma != nil {
		// A PRAGMA reads or sets configuration and yields its value as rows in the query
		// form, neither of which the row-less, write-oriented Exec carries; run it through
		// Run.
		return Summary{}, ErrPragmaCommand
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

// Kind classifies a statement by what it does (doc 18 §5.7, §10.6): a read, a write,
// or a schema change. It is computed from the parsed statement without executing it, so
// a caller (an authorization layer, a router) can decide what to allow before any side
// effect happens.
type Kind int

const (
	// ReadStatement reads the graph and changes nothing. An EXPLAIN of any statement
	// is a read, since EXPLAIN never executes the underlying statement.
	ReadStatement Kind = iota
	// WriteStatement changes graph data (CREATE, MERGE, SET, REMOVE, DELETE, FOREACH).
	WriteStatement
	// SchemaStatement changes the schema (CREATE/DROP INDEX or CONSTRAINT).
	SchemaStatement
	// AdminStatement manages users and roles (CREATE/ALTER/DROP USER, SHOW USERS,
	// GRANT/REVOKE ROLE). It requires the admin role (doc 18 §12.3).
	AdminStatement
	// PragmaStatement reads or sets a configuration knob (PRAGMA, doc 24 §3). It touches
	// no graph data; the query form is an introspection read and the set form changes
	// connection or file configuration.
	PragmaStatement
)

// String names the kind for diagnostics.
func (k Kind) String() string {
	switch k {
	case WriteStatement:
		return "write"
	case SchemaStatement:
		return "schema"
	case AdminStatement:
		return "admin"
	case PragmaStatement:
		return "pragma"
	default:
		return "read"
	}
}

// StatementKind parses a statement and reports whether it reads, writes, or changes
// schema, without executing it (doc 18 §10.6). It is the classifier an authorization
// layer checks a principal's roles against before execution, so a forbidden write is
// refused before it can have any effect. An unparseable statement returns the parse
// error, so a caller can let the normal execution path surface it as a syntax error.
// An EXPLAIN classifies as a read regardless of the underlying statement, because
// EXPLAIN produces only the plan and never executes.
func (db *DB) StatementKind(cypher string) (Kind, error) {
	q, err := parse.Parse(cypher)
	if err != nil {
		return ReadStatement, err
	}
	switch {
	case q.Explain:
		return ReadStatement, nil
	case q.Admin != nil:
		return AdminStatement, nil
	case q.Pragma != nil:
		return PragmaStatement, nil
	case q.Schema != nil:
		return SchemaStatement, nil
	case queryHasWrites(q):
		return WriteStatement, nil
	default:
		return ReadStatement, nil
	}
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
	start := time.Now()
	key := plan.Key{Text: plan.NormalizeText(cypher), Catalog: db.eng.CatalogVersion()}
	st := engineStats{db.eng}
	cached, ok := db.cache.Get(key)
	if ok && !plan.Drifted(cached.Stats, st, db.drift()) {
		db.metrics.recordCacheLookup("hit")
		db.metrics.recordPlan("hit", time.Since(start))
		return cached, nil
	}
	// Either the cache had no plan for this text or the plan it had has drifted off its
	// cardinality basis: both recompile, so both count as a lookup miss, and a present-but-drifted
	// entry also counts as a coherence invalidation that the recompile resolves (doc 20 §3.2).
	db.metrics.recordCacheLookup("miss")
	if ok {
		db.metrics.recordCacheInvalidation("stats_refresh")
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
	if q.Admin != nil {
		return nil, ErrAdminCommand
	}
	if q.Pragma != nil {
		return nil, ErrPragmaCommand
	}
	b, err := bind.Bind(q, bind.NewEngineCatalog(db.eng), false)
	if err != nil {
		return nil, err
	}
	op := plan.SeekRewrite(plan.PlanWithStats(b, st), b, indexLookup{db.eng}, st)
	entry := &plan.Entry{Bound: b, Op: op, Stats: plan.Snapshot(op, st)}
	db.cache.Put(key, entry)
	db.metrics.recordPlan("miss", time.Since(start))
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
	if q.Admin != nil {
		// An administrative statement changes users outside the operator pipeline, so it
		// has no plan to render, the same as a schema command.
		return nil, ErrExplainSchema
	}
	if q.Pragma != nil {
		// A PRAGMA reads or sets configuration outside the operator pipeline, so it has no
		// plan to render either.
		return nil, ErrExplainPragma
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

// indexNamer returns a resolver from an indexed (label, property) token pair to the index name,
// for the index-lookup metric's index label (doc 20 §6.4). It resolves the tokens to their names
// and finds the index over that label whose first property matches, returning its declared name; an
// unresolved pair (no such index, or an unnamed token) returns the empty string, which the metric
// renders as a derived name so a count is never lost.
func (db *DB) indexNamer() func(label, prop engine.Token) string {
	labelName := db.tokenNamer(catalog.KindLabel)
	propName := db.tokenNamer(catalog.KindPropKey)
	return func(label, prop engine.Token) string {
		ln, ok := labelName(label)
		if !ok {
			return ""
		}
		pn, ok := propName(prop)
		if !ok {
			return ""
		}
		for _, ix := range db.eng.IndexInfos() {
			if ix.Label == ln && len(ix.Props) > 0 && ix.Props[0] == pn {
				return ix.Name
			}
		}
		return ""
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
	// mat materializes the graph handles in a row into self-describing Node,
	// Relationship, and Path objects, reading from the result's snapshot (doc 16
	// §10.2). It is nil for a result with no graph objects to materialize (a schema or
	// EXPLAIN result), where the nil-safe materializer yields bare handles.
	mat *objectMaterializer
	// curRow holds the row the most recent Next advanced to, the row Record wraps;
	// err holds the first error that stopped streaming, surfaced through Err. Both are
	// the database/sql.Rows-style streaming state (doc 16 §8.2).
	curRow eval.Row
	err    error

	// mdb, mkind, and mstart carry the deferred query-metric recording for a streaming
	// read (doc 20 §3.1): a read's end-to-end latency ends when the caller drains and
	// closes the result, so measureQuery stamps these and Close records the outcome and
	// the duration since mstart. mdb is nil for an eager result, which records at dispatch.
	// mdone guards the one-shot recording so a second Close does not double-count.
	mdb    *DB
	mkind  string
	mstart time.Time
	mdone  bool
	// mexec is the start of the executor span for a streaming read (doc 20 §3.1): query stamps
	// it once the plan is ready and the cursor is open, and Close records the time since it as
	// gr_query_execute_duration_seconds, the executor work alone with parse and plan excluded.
	mexec time.Time
	// mscan is the shared executor scan counter (doc 20 §3.1), the rows the query's scans
	// and expands touched, read at the recording point for gr_query_rows_scanned. It is nil
	// for a result with no execution (a schema or EXPLAIN result). rowsReturned counts the
	// rows a streaming read yielded, the output cardinality for gr_query_rows_returned; an
	// eager result reads its returned count from the buffer length instead.
	mscan        *atomic.Int64
	rowsReturned int64
}

// Columns returns the result's output column names in order. It is the same column
// list as Keys; Columns is the lower-level spelling, Keys the doc 16 §8.1 name.
func (r *Result) Columns() []string { return r.cols }

// Keys returns the result's column names in the order the RETURN or WITH clause
// produced them (doc 16 §8.1).
func (r *Result) Keys() []string { return r.cols }

// Next advances to the next record and reports whether there is one (doc 16 §8.2).
// It returns false at the end of the stream or on a runtime error; the caller then
// checks Err to tell a normal end (Err is nil) from an error that stopped the stream.
// This is the database/sql.Rows iteration shape: for res.Next() { ... }; res.Err().
func (r *Result) Next() bool {
	row, ok, err := r.next()
	if err != nil {
		r.err = err
		r.curRow = nil
		return false
	}
	if !ok {
		r.curRow = nil
		return false
	}
	r.curRow = row
	return true
}

// Record returns the record the most recent Next advanced to (doc 16 §8.3). It is
// valid only until the next Next call, and nil before the first Next or after a Next
// that returned false.
func (r *Result) Record() *Record {
	if r.curRow == nil {
		return nil
	}
	return newRecord(r.cols, r.curRow, r.mat)
}

// Err returns the first error that stopped streaming, or nil if the stream ended
// normally (doc 16 §8.2, §8.4). It is checked once after the iteration loop, the
// database/sql.Rows.Err idiom.
func (r *Result) Err() error { return r.err }

// Row pulls the next result row as a slice of column values aligned to Columns,
// returning ok false at the end of the stream (the positional, lower-level form of
// the Next/Record streaming API). A column absent from the row binds to the null
// value, the schema-optional reading rule (doc 08 §5.3).
func (r *Result) Row() ([]value.Value, bool, error) {
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

// next pulls the next row from whichever backing the result has: the live cursor
// for a read, or the materialized buffer for an eagerly executed write.
func (r *Result) next() (eval.Row, bool, error) {
	if r.cursor != nil {
		row, ok, err := r.cursor.Next()
		if ok {
			// Count the rows a streaming read yields for gr_query_rows_returned (doc 20
			// §3.1); Close reads the total when the stream ends. An eager result reads its
			// returned count from the buffer length, so it does not count here.
			r.rowsReturned++
		}
		return row, ok, err
	}
	if r.bufIdx >= len(r.buf) {
		return nil, false, nil
	}
	row := r.buf[r.bufIdx]
	r.bufIdx++
	return row, true, nil
}

// NextRow pulls the next result row as a name-keyed map, for callers that prefer
// lookup by column name over positional access. It shares the backing with Next.
func (r *Result) NextRow() (eval.Row, bool, error) { return r.next() }

// Summary reports the graph mutations a write statement run through Run performed
// (doc 13 §3). It is the zero summary for a read result, which mutates nothing, and
// it is complete as soon as Run returns, since a write executes eagerly.
func (r *Result) Summary() Summary { return r.summary }

// Close releases the result's cursor and any read transaction it owns. For an
// auto-commit Query result it aborts the read snapshot the stream runs against; for
// an auto-commit write result it aborts the read snapshot opened after commit to
// materialize the buffered rows' graph objects (doc 16 §10.6); for a managed
// transaction's Run result it leaves the borrowed transaction untouched, since the
// caller commits or rolls it back. It is safe to call more than once.
func (r *Result) Close() error {
	var cerr error
	if r.cursor != nil {
		cerr = r.cursor.Close()
	}
	var terr error
	if r.ownTx && r.tx != nil {
		terr = r.tx.Abort()
	}
	r.cursor, r.tx = nil, nil
	// A streaming read records its query metrics here, where its parse-through-last-row
	// latency actually ends (doc 20 §3.1). The status follows whether the stream errored.
	// mdone makes this fire once, so Close is still safe to call more than once.
	if r.mdb != nil && !r.mdone {
		r.mdone = true
		if r.err != nil {
			r.mdb.metrics.recordError(r.err)
		}
		r.mdb.metrics.recordRows(r.rowsReturned, scanLoad(r.mscan))
		if !r.mexec.IsZero() {
			r.mdb.metrics.recordExecute(r.mkind, time.Since(r.mexec))
		}
		r.mdb.metrics.finish(r.mkind, metricStatusOf(r.err), time.Since(r.mstart))
	}
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
	// The close is clean when the engine closed without error; a failed close left work
	// undone, which the clean flag records for an operator reading the event stream (doc
	// 20 §11.3).
	db.events.Close(db.path, err == nil)
	return err
}
