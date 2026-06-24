package engine

import (
	"cmp"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/gr/adj"
	"github.com/tamnd/gr/blockcache"
	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/column"
	"github.com/tamnd/gr/idmap"
	"github.com/tamnd/gr/mvcc"
	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/stats"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// ErrDetachRequired is returned when deleting a node that still has relationships.
var ErrDetachRequired = errors.New("gr/engine: cannot delete node with relationships")

// ErrIDMapDesync guards the two-id invariant: the id-map's dense position and the
// record store's appended position must agree (they are both append-only from 0).
var ErrIDMapDesync = errors.New("gr/engine: id-map and record store out of sync")

// DiskEngine is the real, durable storage engine of M1: it composes the catalog,
// id-map, node and relationship record stores, columnar property stores, and the
// CSR adjacency over one pager, behind the frozen engine SPI ([engine.Engine]),
// with graph-element MVCC ([mvcc]) layered on top for snapshot isolation.
//
// It is the single-writer-first realization (doc 06 §5; doc 25 §4 deliverable 9):
// one write transaction at a time holds the engine's write lock and creates
// versions; read transactions take a snapshot of the commit sequence and resolve
// every read as of that point, so a reader never sees a writer's uncommitted work
// and a reader's view is stable for its whole life. The durable base stores hold
// the latest committed state; an in-memory retention overlay ([mvcc.Overlay])
// keeps the pre-images older snapshots still need, reclaimed by the watermark.
// Full concurrent writers (doc 06 §6) add only a commit-time disjointness check
// behind this same model and are post-M1.
//
// Catalog interning is a setup-time concern outside the per-transaction SPI,
// exposed as Intern and TokenName on the concrete type. SPI tokens are one-based:
// token 0 is the SPI's "all labels"/"all types" wildcard (see ScanLabel and
// Expand), so the engine offsets catalog tokens by one at the boundary.
type DiskEngine struct {
	mu     sync.RWMutex
	p      *pager.Pager
	secs   *store.Sections
	cat    *catalog.Catalog
	ids    *idmap.Map
	nodes  *node.Store
	rels   *rel.Store
	ncols  *column.Columns
	rcols  *column.Columns
	nseg   *colsegstore.Store
	rseg   *colsegstore.Store
	adj    *adj.Adj
	st     *stats.Stats
	oracle *mvcc.Oracle
	ov     *mvcc.Overlay
	idx    *propIndexSet
	closed bool

	// conObs receives one report per constraint check at commit, so a higher layer can count
	// enforcement without the engine importing a metric registry (doc 20 §6.4). It is nil until a
	// caller sets it, and every report goes through the nil-safe reportConstraint helper.
	conObs ConstraintObserver

	// idxCounts holds the per-index entry count, and idxBytes the per-index in-memory footprint
	// estimate, each published as a whole map under the engine lock whenever the indexes are rebuilt
	// and read lock-free for the gr_index_entries and gr_index_memory_bytes gauges (doc 20 §6.4). They
	// are pointer swaps, never mutated in place, so a reader sees one consistent map even while a
	// writer holds the engine lock, which is what keeps the metrics snapshot off the engine lock and
	// out of a deadlock with a long-held write transaction.
	idxCounts atomic.Pointer[map[string]uint64]
	idxBytes  atomic.Pointer[map[string]uint64]

	// fileSizeBytes holds the durable size of the main file, published under the engine lock at open
	// and after every write commit (the only points the size changes) and read lock-free for the
	// gr_file_size_bytes gauge (doc 20 §4.2). Publishing where the writer already holds the lock and
	// reading the atomic keeps the metrics snapshot off the engine lock, the same discipline the
	// index gauges use.
	fileSizeBytes atomic.Uint64

	// freelistPages holds the number of reusable pages on the free list, published the same way as
	// fileSizeBytes (under the engine lock at open and after a write commit) and read lock-free for
	// the gr_freelist_pages gauge (doc 20 §4.2). The free list changes only on the write path, so a
	// commit-time refresh keeps it current, and computing it there walks the trunk chain under the
	// write lock where no reader runs, which the metrics snapshot path must not do itself.
	freelistPages atomic.Uint64

	// ckptNodeSegmentsTotal and ckptRelSegmentsTotal hold the cumulative count of column segments the
	// node and rel folds have rewritten across every checkpoint, bumped under the engine lock at the
	// end of each checkpoint by the new store's directory count, since a fold rewrites the whole
	// segmented base (doc 20 §5.4). They are the authoritative cumulative counts; the metrics path
	// mirrors them into gr_checkpoint_segments_rewritten_total{store} delta-style, the same bridge the
	// cache hit counters use, so reading them is a lock-free atomic load off the snapshot path.
	ckptNodeSegmentsTotal atomic.Uint64
	ckptRelSegmentsTotal  atomic.Uint64

	// ckptDeltaFoldedTotal counts the adjacency delta entries every fold has merged into the base CSR,
	// bumped under the engine lock at the start of each checkpoint by the staged delta length (doc 20
	// §5.4). It is the authoritative cumulative count the metrics path mirrors into
	// gr_checkpoint_delta_folded_total delta-style, read off the snapshot path as a lock-free atomic.
	ckptDeltaFoldedTotal atomic.Uint64

	// ckptPagesWrittenTotal counts the page images checkpoints have written back to the database file,
	// bumped under the engine lock by the pager's write-back delta across each checkpoint's commits
	// (doc 20 §5.4). It is the authoritative cumulative count the metrics path mirrors into
	// gr_checkpoint_pages_written_total delta-style, read off the snapshot path as a lock-free atomic.
	ckptPagesWrittenTotal atomic.Uint64

	// gcRunsTotal counts version-GC passes, and gcReclaimedNode and gcReclaimedRel the pre-images
	// each pass dropped, split by element, all bumped under the engine lock when GC runs at
	// checkpoint (doc 20 §5.1). They are the authoritative cumulative counts the metrics path mirrors
	// into gr_mvcc_gc_runs_total and gr_mvcc_gc_reclaimed_total{element} delta-style, read lock-free.
	gcRunsTotal     atomic.Uint64
	gcReclaimedNode atomic.Uint64
	gcReclaimedRel  atomic.Uint64

	// commitsTotal counts write transactions that reached their durability point, bumped under the
	// engine lock when a write transaction's commit makes the pager durable (doc 20 §5.3). It is the
	// amortization numerator: against gr_wal_fsync_total, rate(commits)/rate(fsync) is the commits-per-
	// fsync ratio that shows the group-commit payoff, near one in the inline-commit design and rising
	// once batching lands. It is the authoritative cumulative count the metrics path mirrors into
	// gr_commits_total delta-style, read lock-free off the snapshot path.
	commitsTotal atomic.Uint64

	// gcDurMu guards gcDurations, the per-pass GC durations in seconds buffered since the metrics path
	// last drained them (doc 20 §5.1). A histogram cannot be delta-mirrored from a cumulative pair the
	// way the GC counts are, so each pass appends its duration here under the engine lock the checkpoint
	// holds, and DrainGCDurations hands the buffer to the metrics path under gcDurMu alone, never the
	// engine lock, so the snapshot path observes each pass's duration exactly once.
	gcDurMu     sync.Mutex
	gcDurations []float64

	// bc fronts the segmented-base read with decoded segments, so a repeated point
	// read in a segment does not re-decode it (doc 14 §4). It is an in-memory cache,
	// never a source of truth: every cached segment carries the checkpoint epoch it
	// was built at, and epoch invalidates them all when a fold rebuilds the base.
	bc *blockcache.Cache
	// epoch is the checkpoint generation of the segmented base. A fold rebuilds the
	// base with a new segment layout and bumps it, so a read at the new epoch misses
	// every entry cached against the old base.
	epoch uint64

	// decodeObs receives the wall-clock time of each segment decode the colcache miss path runs,
	// labeled by the segment's codec, so a higher layer can chart decode cost without the engine
	// importing a metric registry (doc 20 §4.4). It is nil until a caller sets it, and the segGet
	// timing is skipped when it is nil. The observe is a lock-free atomic bucket add, so timing a
	// decode does not serialize the read path.
	decodeObs SegmentDecodeObserver

	// degreeStats is the per-(type, direction) degree distribution published at
	// open and after each checkpoint, the substrate for the supernode and skew
	// metrics (doc 20 §6.2). It is computed under the engine lock, where the adj
	// degree summaries are consistent, and stored here so db.Metrics() can read it
	// lock-free without taking the engine lock (which a live writer holds for its
	// whole transaction, so the metrics path must never wait on it).
	degreeStats atomic.Pointer[[]DegreeStat]
}

// DegreeStat is one (relationship type, direction) slot's degree distribution, the
// engine-facing shape of an adj.DegreeSummary with the slot already split into a
// typed, directed pair (doc 20 §6.2).
type DegreeStat struct {
	Type                        Token
	Dir                         Direction
	Nodes, Edges, Max, P50, P99 uint64
}

// Open opens or creates a graph database at path. A fresh file gets empty stores,
// committed so the structure is durable; an existing file reopens its stores,
// recovering to the committed prefix via the pager. The MVCC clock starts at the
// pager's durable change counter, so commit sequences continue monotonically
// across reopens; the retention overlay starts empty (a fresh open has no live
// old snapshots, so the base is the whole truth).
func Open(fsys vfs.VFS, path string, opt pager.Options) (*DiskEngine, error) {
	p, err := pager.Open(fsys, path, opt)
	if err != nil {
		return nil, err
	}
	e := &DiskEngine{p: p, ov: mvcc.NewOverlay(), bc: blockcache.New(defaultBlockCacheBytes)}
	fresh := p.SectionDir() == 0
	if err := e.load(fresh); err != nil {
		_ = p.Close()
		return nil, err
	}
	if fresh {
		if err := p.Commit(); err != nil {
			_ = p.Close()
			return nil, err
		}
	}
	e.oracle = mvcc.NewOracle(p.Header().ChangeCounter)
	e.publishFileSize()
	e.publishFreelistPages()
	return e, nil
}

// publishFileSize stores the durable file size into the lock-free atomic the gr_file_size_bytes
// gauge reads (doc 20 §4.2). The size is the page count times the page size, the value Commit
// truncates the file to, and it changes only at open and at a write commit, so publishing at those
// points keeps the gauge current. It is called while the caller holds the engine lock (or at open,
// before the engine is shared), so reading the header here does not race a writer.
func (e *DiskEngine) publishFileSize() {
	e.fileSizeBytes.Store(uint64(e.p.Header().PageCount) * uint64(e.p.PageSize()))
}

// FileSizeBytes returns the durable size of the main file in bytes, read lock-free from the atomic
// publishFileSize maintains (doc 20 §4.2). It never takes the engine lock, so the metrics snapshot
// path calls it freely even while a write transaction holds the lock.
func (e *DiskEngine) FileSizeBytes() int64 { return int64(e.fileSizeBytes.Load()) }

// publishFreelistPages refreshes the lock-free free-page count the gr_freelist_pages gauge reads
// (doc 20 §4.2). It walks the free-list trunk chain, which reads pages, so it runs only where the
// caller holds the engine lock (open before the engine is shared, or a write commit) and no reader
// is faulting pages. A walk error leaves the previous value rather than failing the commit, since
// the data is already durable and a stale metric is better than a lost commit.
func (e *DiskEngine) publishFreelistPages() {
	if n, err := e.p.FreeCount(); err == nil {
		e.freelistPages.Store(n)
	}
}

// FreelistPages returns the number of reusable pages on the free list, read lock-free from the
// atomic publishFreelistPages maintains (doc 20 §4.2). It never takes the engine lock, so the
// metrics snapshot path calls it freely even while a write transaction holds the lock.
func (e *DiskEngine) FreelistPages() int64 { return int64(e.freelistPages.Load()) }

// CheckpointSegmentsTotal returns the cumulative count of column segments the node and rel folds
// have rewritten across every checkpoint (doc 20 §5.4), read lock-free from the atomics each
// checkpoint bumps. The metrics path mirrors these into gr_checkpoint_segments_rewritten_total
// delta-style, so this never takes the engine lock and is safe off the snapshot path.
func (e *DiskEngine) CheckpointSegmentsTotal() (node, rel uint64) {
	return e.ckptNodeSegmentsTotal.Load(), e.ckptRelSegmentsTotal.Load()
}

// CheckpointDeltaFoldedTotal returns the cumulative count of adjacency delta entries every fold has
// merged into the base CSR (doc 20 §5.4), read lock-free from the atomic each checkpoint bumps. The
// metrics path mirrors it into gr_checkpoint_delta_folded_total delta-style, so this never takes the
// engine lock and is safe off the snapshot path.
func (e *DiskEngine) CheckpointDeltaFoldedTotal() uint64 { return e.ckptDeltaFoldedTotal.Load() }

// CheckpointPagesWrittenTotal returns the cumulative count of page images checkpoints have written back
// to the database file (doc 20 §5.4), read lock-free from the atomic each checkpoint bumps. The metrics
// path mirrors it into gr_checkpoint_pages_written_total delta-style, so this never takes the engine
// lock and is safe off the snapshot path.
func (e *DiskEngine) CheckpointPagesWrittenTotal() uint64 { return e.ckptPagesWrittenTotal.Load() }

// VersionsResident returns the number of element versions the MVCC overlay still holds beyond the
// current committed version (doc 20 §5.1), for the gr_mvcc_versions_resident gauge. It reads the
// overlay's own lock, never the engine lock, so the metrics snapshot path calls it freely even while
// a write transaction holds the engine lock; the write path takes the engine lock then the overlay
// lock, so there is no inversion.
func (e *DiskEngine) VersionsResident() int64 { return int64(e.ov.Len()) }

// VersionChainLengths returns the length of every retained version chain split by element (doc 20
// §5.1), for the gr_mvcc_version_chain_length histogram. Like VersionsResident it reads the overlay's
// own lock, never the engine lock, so the metrics path computes the point-in-time distribution
// without waiting on a write transaction.
func (e *DiskEngine) VersionChainLengths() (node, rel []float64) { return e.ov.ChainLengths() }

// GCStats returns the cumulative version-GC totals (doc 20 §5.1): the GC passes run and the
// pre-images reclaimed split by element, read lock-free from the atomics each checkpoint's GC bumps.
// The metrics path mirrors these into gr_mvcc_gc_runs_total and gr_mvcc_gc_reclaimed_total{element}
// delta-style, so this never takes the engine lock and is safe off the snapshot path.
func (e *DiskEngine) GCStats() (runs, reclaimedNode, reclaimedRel uint64) {
	return e.gcRunsTotal.Load(), e.gcReclaimedNode.Load(), e.gcReclaimedRel.Load()
}

// DrainGCDurations returns and clears the per-pass GC durations in seconds buffered since the last
// drain (doc 20 §5.1), for the gr_mvcc_gc_duration_seconds histogram. A histogram cannot be
// delta-mirrored from a cumulative pair the way the GC counts are, so the engine buffers each pass's
// duration and the metrics path drains it here. It takes only gcDurMu, never the engine lock, so the
// snapshot path stays clear of a long-held write transaction; returning nil when empty avoids an alloc.
func (e *DiskEngine) DrainGCDurations() []float64 {
	e.gcDurMu.Lock()
	defer e.gcDurMu.Unlock()
	if len(e.gcDurations) == 0 {
		return nil
	}
	out := e.gcDurations
	e.gcDurations = nil
	return out
}

// CommitsTotal returns the cumulative count of write transactions that reached their durability point
// (doc 20 §5.3), the amortization numerator the metrics path mirrors into gr_commits_total. It is a
// lock-free atomic load, never the engine lock, so the metrics snapshot path reads it freely even while
// a write transaction holds the engine lock.
func (e *DiskEngine) CommitsTotal() uint64 { return e.commitsTotal.Load() }

// WatermarkLag returns the commit versions between the newest commit and the GC watermark (doc 20
// §5.1), for the gr_mvcc_watermark_lag_versions gauge. It reads the oracle's own lock, never the
// engine lock, so the metrics snapshot path calls it freely even while a write transaction holds the
// engine lock; the write path takes the engine lock then the oracle lock, so there is no inversion.
func (e *DiskEngine) WatermarkLag() int64 { return int64(e.oracle.WatermarkLag()) }

// OldestSnapshotAgeSeconds returns the whole-second age of the oldest live snapshot, or zero when none
// is live (doc 20 §5.1), for the gr_mvcc_oldest_snapshot_age_seconds gauge. Like WatermarkLag it reads
// only the oracle's own lock, so the metrics path is safe to call it while a writer holds the engine
// lock; the result is truncated to seconds since the gauge reports a coarse age for long readers.
func (e *DiskEngine) OldestSnapshotAgeSeconds() int64 {
	return int64(e.oracle.OldestSnapshotAge().Seconds())
}

// load (re)builds the store handles over the current pager state, creating the
// stores when fresh and opening them otherwise. It is also used to rebuild state
// after a rollback. It does not touch the oracle or overlay, which outlive a
// transaction abort.
func (e *DiskEngine) load(create bool) error {
	var err error
	if create {
		if e.secs, err = store.CreateSections(e.p); err != nil {
			return err
		}
		if e.cat, err = catalog.Create(e.p, e.secs); err != nil {
			return err
		}
		if e.ids, err = idmap.Create(e.p, e.secs); err != nil {
			return err
		}
		if e.nodes, err = node.Create(e.p, e.secs); err != nil {
			return err
		}
		if e.rels, err = rel.Create(e.p, e.secs); err != nil {
			return err
		}
		if e.ncols, err = column.Create(e.p, e.secs, store.SecNodeCols); err != nil {
			return err
		}
		if e.rcols, err = column.Create(e.p, e.secs, store.SecRelCols); err != nil {
			return err
		}
		if e.nseg, err = createSegStore(e.p, e.secs, store.SecNodeColSeg); err != nil {
			return err
		}
		if e.rseg, err = createSegStore(e.p, e.secs, store.SecRelColSeg); err != nil {
			return err
		}
		if e.adj, err = adj.Create(e.p, e.secs, e.rels); err != nil {
			return err
		}
		if e.st, err = stats.Create(e.p, e.secs); err != nil {
			return err
		}
		e.publishDegreeStats()
		return e.rebuildIndexes()
	}
	if e.secs, err = store.OpenSections(e.p); err != nil {
		return err
	}
	if e.cat, err = catalog.Open(e.p, e.secs); err != nil {
		return err
	}
	if e.ids, err = idmap.Open(e.p, e.secs); err != nil {
		return err
	}
	if e.nodes, err = node.Open(e.p, e.secs); err != nil {
		return err
	}
	if e.rels, err = rel.Open(e.p, e.secs); err != nil {
		return err
	}
	if e.ncols, err = column.Open(e.p, e.secs, store.SecNodeCols); err != nil {
		return err
	}
	if e.rcols, err = column.Open(e.p, e.secs, store.SecRelCols); err != nil {
		return err
	}
	if e.nseg, err = openSegStore(e.p, e.secs, store.SecNodeColSeg); err != nil {
		return err
	}
	if e.rseg, err = openSegStore(e.p, e.secs, store.SecRelColSeg); err != nil {
		return err
	}
	if e.adj, err = adj.Open(e.p, e.secs, e.rels); err != nil {
		return err
	}
	if e.st, err = stats.Open(e.p, e.secs); err != nil {
		return err
	}
	e.publishDegreeStats()
	return e.rebuildIndexes()
}

// commitPager makes the pager durable and advances the MVCC clock to the new
// durable change counter, the single source of commit sequences.
func (e *DiskEngine) commitPager() (mvcc.Seq, error) {
	if err := e.p.Commit(); err != nil {
		return 0, err
	}
	seq := e.p.Header().ChangeCounter
	e.oracle.SetSeq(seq)
	return seq, nil
}

// Intern maps a label/relationship-type/property-key name to its token, assigning
// one if new. It is its own durable transaction (it takes the write lock and
// commits), so call it for schema setup between graph transactions. The returned
// token is one-based to keep token 0 as the SPI wildcard.
func (e *DiskEngine) Intern(kind catalog.Kind, name string) (Token, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, _, err := e.cat.Intern(kind, name)
	if err != nil {
		return 0, err
	}
	if _, err := e.commitPager(); err != nil {
		return 0, err
	}
	return Token(t + 1), nil
}

// Lookup returns the one-based token for an already-interned name, or false. It
// takes a read lock and does not commit.
func (e *DiskEngine) Lookup(kind catalog.Kind, name string) (Token, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	c, ok := e.cat.Lookup(kind, name)
	if !ok {
		return 0, false
	}
	return Token(c + 1), true
}

// TokenName returns the name a token maps to.
func (e *DiskEngine) TokenName(kind catalog.Kind, t Token) (string, bool) {
	if t == 0 {
		return "", false
	}
	return e.cat.Name(kind, uint32(t-1))
}

// IndexInfo describes a schema index with its label and property names resolved from
// the catalog dictionaries, so a caller listing the schema does not need the raw
// tokens (doc 07; doc 17 §6.5).
type IndexInfo struct {
	Name  string
	Label string
	Props []string
}

// tokenNames returns every interned name in a dictionary, in token order. The catalog
// is append-only, so this is the full set of labels, relationship types, or property
// keys the database has ever seen. The caller holds no lock; this takes the read lock.
func (e *DiskEngine) tokenNames(kind catalog.Kind) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n := e.cat.Count(kind)
	out := make([]string, 0, n)
	for t := range n {
		if name, ok := e.cat.Name(kind, uint32(t)); ok {
			out = append(out, name)
		}
	}
	return out
}

// Labels returns every node label the catalog holds (doc 17 §6.5).
func (e *DiskEngine) Labels() []string { return e.tokenNames(catalog.KindLabel) }

// RelationshipTypes returns every relationship type the catalog holds (doc 17 §6.5).
func (e *DiskEngine) RelationshipTypes() []string { return e.tokenNames(catalog.KindRelType) }

// PropertyKeys returns every property key the catalog holds (doc 17 §6.5).
func (e *DiskEngine) PropertyKeys() []string { return e.tokenNames(catalog.KindPropKey) }

// IndexInfos returns the schema indexes with their label and property names resolved
// (doc 17 §6.5). An index whose label or property token is missing from the catalog
// (which should not happen) is reported with an empty name in that position rather
// than dropped.
func (e *DiskEngine) IndexInfos() []IndexInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ixs := e.cat.Indexes()
	out := make([]IndexInfo, 0, len(ixs))
	for _, ix := range ixs {
		info := IndexInfo{Name: ix.Name}
		if name, ok := e.cat.Name(catalog.KindLabel, ix.Label); ok {
			info.Label = name
		}
		for _, p := range ix.Props {
			name, _ := e.cat.Name(catalog.KindPropKey, p)
			info.Props = append(info.Props, name)
		}
		out = append(out, info)
	}
	return out
}

// ConstraintInfo describes a schema constraint with its label, property names, and
// kind resolved from the catalog, so a caller listing the schema (a dump's DDL
// section, doc 17 §13.2, or .info) does not need the raw tokens or kind codes. Kind
// is the constraint's flavour ("UNIQUE", "EXISTS", or "TYPE"); PropType names the
// required value type for a TYPE constraint and is empty otherwise.
type ConstraintInfo struct {
	Name     string
	Label    string
	Props    []string
	Kind     string
	PropType string
}

// ConstraintInfos returns the schema constraints with their names resolved (doc 08
// §4, doc 17 §13.2). The order follows the catalog's declaration order, so a dump
// re-creates constraints in the order they were added.
func (e *DiskEngine) ConstraintInfos() []ConstraintInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cons := e.cat.Constraints()
	out := make([]ConstraintInfo, 0, len(cons))
	for _, c := range cons {
		info := ConstraintInfo{Name: c.Name}
		if name, ok := e.cat.Name(catalog.KindLabel, c.Label); ok {
			info.Label = name
		}
		for _, p := range c.Props {
			name, _ := e.cat.Name(catalog.KindPropKey, p)
			info.Props = append(info.Props, name)
		}
		switch c.Kind {
		case catalog.UniqueNode:
			info.Kind = "UNIQUE"
		case catalog.ExistsNode:
			info.Kind = "EXISTS"
		case catalog.TypedNode:
			info.Kind = "TYPE"
			info.PropType = value.Type(c.ValueType).String()
		}
		out = append(out, info)
	}
	return out
}

// StorageInfo is the static, structural nameplate of the database file: the
// format version, the page geometry, and the free-page count (doc 17 §6.15).
type StorageInfo struct {
	FormatVersion uint32
	PageSize      uint32
	PageCount     uint64
	FreePages     uint64
	SizeBytes     int64
}

// StorageInfo reports the file's structural facts behind `.info` / `gr info`
// (doc 17 §6.15). The size is the allocated page count times the page size, the
// logical database size, which is the on-disk main-file size after a checkpoint.
func (e *DiskEngine) StorageInfo() (StorageInfo, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	h := e.p.Header()
	free, err := e.p.FreeCount()
	if err != nil {
		return StorageInfo{}, err
	}
	return StorageInfo{
		FormatVersion: h.FormatVersion,
		PageSize:      h.PageSize,
		PageCount:     h.PageCount,
		FreePages:     free,
		SizeBytes:     int64(h.PageCount) * int64(h.PageSize),
	}, nil
}

// Recovered reports whether opening the database redid a committed WAL prefix
// after a crash, feeding the open event's recovered flag (doc 20 §11.3).
func (e *DiskEngine) Recovered() bool { return e.p.Recovered() }

// RecoveryStats returns what the crash recovery on open redid (doc 20 §11.3): the committed
// transactions replayed, the durable commit sequence the header records, and how long recovery
// took. It feeds the recovery_complete event and is zero when the open did not recover. It reads
// values the pager set during Open before any concurrent access, so it needs no engine lock.
func (e *DiskEngine) RecoveryStats() (txReplayed int, lastSeq uint64, dur time.Duration) {
	return e.p.RecoveryStats()
}

// RecoveryStartStats returns what the recovery_start event reports before the replay runs (doc 20
// §11.3): the WAL byte size found at open and the durable change counter the replay starts from. It
// feeds the recovery_start event and is zero when the open did not recover. It reads values the
// pager set during Open before any concurrent access, so it needs no engine lock.
func (e *DiskEngine) RecoveryStartStats() (walSizeBytes int64, lastCheckpointLSN uint64) {
	return e.p.RecoveryStartStats()
}

// CatalogVersion returns a monotonic version of the catalog: the total number of
// interned names across the label, type, and property-key dictionaries. The
// catalog is append-only (names are interned, never removed), so any schema
// addition strictly increases this value, which is exactly what the plan cache
// needs to key a compiled plan to the catalog it was bound against (doc 14 §8.4).
func (e *DiskEngine) CatalogVersion() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return uint64(e.cat.Count(catalog.KindLabel)+
		e.cat.Count(catalog.KindRelType)+
		e.cat.Count(catalog.KindPropKey)) + e.cat.SchemaOps()
}

// NodeCountByLabel returns the number of live nodes carrying a label, the
// per-label cardinality the planner uses (doc 04 §19.1; doc 25 deliverable 11).
// It is the latest committed count, maintained on the write path, not a snapshot
// read. A zero label is not the wildcard here; pass a real label token.
func (e *DiskEngine) NodeCountByLabel(label Token) (uint64, error) {
	if label == 0 {
		return 0, nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st.LabelCount(toCat(label))
}

// RelCountByType returns the number of live relationships of a type, the per-type
// cardinality the planner uses.
func (e *DiskEngine) RelCountByType(relType Token) (uint64, error) {
	if relType == 0 {
		return 0, nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st.RelTypeCount(toCat(relType))
}

// NodeCount returns the total number of node records, the all-nodes cardinality the
// cost model uses for an unlabeled scan and as the denominator for average degree
// (doc 11 §2.2). It is the record high-water mark, so it counts a deleted node's
// slot until it is reused; that makes it an upper bound, which keeps a cardinality
// estimate conservative rather than too low.
func (e *DiskEngine) NodeCount() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return uint64(e.nodes.Count())
}

// RelCount returns the total number of relationship records, the denominator-side
// total the cost model uses for typeless average degree (doc 11 §2.2). Like
// NodeCount it is the record high-water mark, an upper bound.
func (e *DiskEngine) RelCount() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return uint64(e.rels.Count())
}

// Begin opens a transaction. A write transaction takes the engine's write lock
// for its whole duration (single-writer-first) and reads its own writes through
// the live base. A read transaction takes a snapshot of the commit sequence and
// holds no lock between operations, so it never blocks a writer for its lifetime;
// each read briefly shares the lock only to read the base coherently.
func (e *DiskEngine) Begin(write bool) (Tx, error) {
	if write {
		e.mu.Lock()
	}
	snap, read := e.oracle.Begin()
	return &diskTx{e: e, write: write, snap: snap, readSeq: read}, nil
}

// Checkpoint folds the adjacency delta into the base CSR, makes everything
// durable, and reclaims overlay pre-images no live snapshot can see (the
// watermark bound). It takes the write lock, so it does not run concurrently
// with a write transaction.
func (e *DiskEngine) Checkpoint() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Capture the pager's cumulative write-back count before the fold, so the difference after the
	// checkpoint's commits is exactly the pages this checkpoint wrote back to the file (doc 20 §5.4).
	pagesBefore := e.p.PagesWritten()
	// Capture the staged adjacency delta before the fold drains it, so the metrics path can mirror the
	// edge work this checkpoint absorbed (doc 20 §5.4). The fold clears the delta to empty, so reading
	// it after would always be zero; we hold the engine lock, so no writer is appending concurrently.
	folded := uint64(e.adj.DeltaLen())
	if err := e.adj.Checkpoint(uint64(e.nodes.Count())); err != nil {
		return err
	}
	e.ckptDeltaFoldedTotal.Add(folded)
	// Merge the naive delta over the current base into a fresh segmented base, then
	// drain the delta to empty so the next checkpoint window starts clean. The
	// merge reads the old base and the old delta, so hold on to both, compute the
	// fresh stores, swap them in, and only then free the old pages once nothing
	// reads them.
	oldNseg, oldRseg := e.nseg, e.rseg
	oldNcols, oldRcols := e.ncols, e.rcols
	var err error
	newNseg, err := e.foldSegmented(e.ncols, e.nseg, uint64(e.nodes.Count()), store.SecNodeColSeg, nodeColID)
	if err != nil {
		return err
	}
	newRseg, err := e.foldSegmented(e.rcols, e.rseg, uint64(e.rels.Count()), store.SecRelColSeg, relColID)
	if err != nil {
		return err
	}
	e.nseg, e.rseg = newNseg, newRseg
	// A fold rewrites the whole segmented base, so the new store's directory count is the
	// segments this checkpoint rewrote; add them to the cumulative totals the metrics path
	// mirrors (doc 20 §5.4). Done under the engine lock the checkpoint already holds.
	e.ckptNodeSegmentsTotal.Add(uint64(newNseg.DirCount()))
	e.ckptRelSegmentsTotal.Add(uint64(newRseg.DirCount()))
	// The new base has a fresh segment layout, so every entry cached against the old
	// base is now stale; bumping the epoch makes a read at it miss them all (doc 14
	// §4.7). The folds above ran against the old base at the old epoch, so their
	// cached segments are correctly invalidated here too.
	e.epoch++
	// Return the old base and old delta pages to the free list before creating the
	// fresh empty delta columns, so the fresh columns reuse those pages instead of
	// growing the file. The folds above already read both, the new base is swapped
	// in, and no live read reaches a delta or base by stored pointer, so the pages
	// are unreferenced (doc 60 §6; doc 64 §6).
	if err := oldNseg.Free(); err != nil {
		return err
	}
	if err := oldRseg.Free(); err != nil {
		return err
	}
	if err := oldNcols.Free(); err != nil {
		return err
	}
	if err := oldRcols.Free(); err != nil {
		return err
	}
	if e.ncols, err = column.Create(e.p, e.secs, store.SecNodeCols); err != nil {
		return err
	}
	if e.rcols, err = column.Create(e.p, e.secs, store.SecRelCols); err != nil {
		return err
	}
	if _, err := e.commitPager(); err != nil {
		return err
	}
	// The checkpoint's commits are done, so the pager's write-back count has advanced by exactly the
	// pages this checkpoint wrote back to the file; record the delta for the metrics path (doc 20 §5.4).
	e.ckptPagesWrittenTotal.Add(e.p.PagesWritten() - pagesBefore)
	gcStart := time.Now()
	gc := e.ov.GC(e.oracle.Watermark())
	gcDur := time.Since(gcStart).Seconds()
	// Record the GC pass, what it reclaimed, and how long it took for the MVCC metrics (doc 20 §5.1),
	// under the engine lock the checkpoint holds; the metrics path mirrors the cumulative totals and
	// drains the buffered durations.
	e.gcRunsTotal.Add(1)
	e.gcReclaimedNode.Add(gc.Node)
	e.gcReclaimedRel.Add(gc.Rel)
	e.gcDurMu.Lock()
	e.gcDurations = append(e.gcDurations, gcDur)
	e.gcDurMu.Unlock()
	// The fold rebuilt the adjacency base, so its degree distribution changed;
	// republish it for the metrics path while we still hold the engine lock.
	e.publishDegreeStats()
	return nil
}

// publishDegreeStats converts the adjacency's per-slot degree summaries into the
// engine's typed, directed DegreeStat list and stores it for the lock-free
// metrics path (doc 20 §6.2). The caller holds the engine lock, where adj's
// degStats is consistent; the published slice is the metrics path's only view, so
// it never reaches into adj. A slot maps to a type token (slot/2, shifted to the
// 1-based SPI space) and a direction (slot%2).
func (e *DiskEngine) publishDegreeStats() {
	summaries := e.adj.DegreeStats()
	out := make([]DegreeStat, 0, len(summaries))
	for s, d := range summaries {
		dir := Outgoing
		if adj.SlotDir(s) == adj.In {
			dir = Incoming
		}
		out = append(out, DegreeStat{
			Type:  toTok(adj.SlotType(s)),
			Dir:   dir,
			Nodes: d.Nodes,
			Edges: d.Edges,
			Max:   d.Max,
			P50:   d.P50,
			P99:   d.P99,
		})
	}
	e.degreeStats.Store(&out)
}

// DegreeStats returns the per-(type, direction) degree distribution published at
// the last open or checkpoint, read lock-free so the metrics path never waits on
// the engine lock a live writer holds (doc 20 §6.2). It returns nil before the
// first publish.
func (e *DiskEngine) DegreeStats() []DegreeStat {
	p := e.degreeStats.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Backup writes a consistent physical image of the database to w and returns the
// number of bytes written (doc 17 §6.13, doc 19 §15). It takes the engine's
// exclusive lock for the copy, so no write transaction lands mid-image and the
// page image is the single committed snapshot the lock pins; the result is a
// standalone .gr file that opens directly.
func (e *DiskEngine) Backup(w io.Writer) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.p.CopyImage(w)
}

// PageSize returns the file's page size in bytes, the value the library surface
// reports without reaching past the engine into the pager.
func (e *DiskEngine) PageSize() uint32 { return e.p.PageSize() }

// BufferPoolStats returns the pager's buffer-pool lookup outcomes and resident population (doc 20
// §4.1), the numbers the buffer-pool metrics expose. It reads only the pager's pool lock, never the
// engine lock, so the metrics snapshot path calls it freely even while a write transaction holds
// the engine lock.
func (e *DiskEngine) BufferPoolStats() pager.PoolStats { return e.p.PoolStats() }

// PagesByStore returns the pager's cumulative per-store page read and write counts since open (doc 20
// §4.2), the per-store breakdown the gr_pages_read_total{store} and gr_pages_written_total{store}
// counters expose. It reads the pager's lock-free per-store atomics, never the engine lock, so the
// metrics snapshot path calls it freely even while a write transaction holds the engine lock.
func (e *DiskEngine) PagesByStore() []pager.StorePageIO { return e.p.PagesByStore() }

// SetSegmentDecodeObserver installs the observer the colcache miss path reports segment decode times
// to (doc 20 §4.4). It is set once after Open, before the database is shared, since segment decodes
// run only on later query read paths and not during open itself.
func (e *DiskEngine) SetSegmentDecodeObserver(o SegmentDecodeObserver) { e.decodeObs = o }

// WALStats returns the write-ahead log's cumulative write counters and current size (doc 20 §5.2), the
// numbers the WAL metrics expose. It reads the WAL's own lock-free atomics through the pager, never the
// engine lock, so the metrics snapshot path calls it freely even while a write transaction holds the
// engine lock.
func (e *DiskEngine) WALStats() wal.Stats { return e.p.WALStats() }

// CheckPager returns the pager for the integrity checker. The checker holds the engine
// read lock for its whole run, so it is safe to access the pager directly.
func (e *DiskEngine) CheckPager() *pager.Pager { return e.p }

// CheckAdj returns the adjacency index for the integrity checker.
func (e *DiskEngine) CheckAdj() *adj.Adj { return e.adj }

// CheckCatalog returns the catalog for the integrity checker.
func (e *DiskEngine) CheckCatalog() *catalog.Catalog { return e.cat }

// CheckNodeStore returns the node store for the integrity checker.
func (e *DiskEngine) CheckNodeStore() *node.Store { return e.nodes }

// CheckRelStore returns the relationship store for the integrity checker.
func (e *DiskEngine) CheckRelStore() *rel.Store { return e.rels }

// CheckConstraints returns the declared constraints for the integrity checker.
func (e *DiskEngine) CheckConstraints() []ConstraintInfo { return e.ConstraintInfos() }

// DrainWALFsyncDurations returns and clears the WAL's per-fsync durations in seconds buffered since the
// last drain (doc 20 §5.2), the samples the metrics path observes into the fsync-latency histogram. It
// forwards to the pager and on to the WAL, which takes only its own small buffer lock, never the engine
// lock, so the metrics snapshot path drains it freely even while a write transaction holds the engine lock.
func (e *DiskEngine) DrainWALFsyncDurations() []float64 { return e.p.DrainWALFsyncDurations() }

// Close releases the engine and its pager.
func (e *DiskEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return e.p.Close()
}

// diskTx is a transaction over the disk engine. A read transaction resolves every
// read as of readSeq through the overlay, falling back to the base; a write
// transaction mutates the base in place under the held write lock, records each
// datum's pre-image, and publishes them to the overlay at commit so older
// snapshots keep resolving the values they saw.
type diskTx struct {
	e       *DiskEngine
	write   bool
	done    bool
	snap    uint64
	readSeq mvcc.Seq
	pending []pendingPre
}

// pendingPre is a captured pre-image awaiting publication at commit.
type pendingPre struct {
	key mvcc.Key
	pre mvcc.Pre
}

// rguard takes the engine read lock for a read transaction's physical base
// access and returns the matching unlock; under a write transaction the
// exclusive lock is already held, so it is a no-op.
func (t *diskTx) rguard() func() {
	if t.write {
		return func() {}
	}
	t.e.mu.RLock()
	return t.e.mu.RUnlock
}

// --- token and id helpers ---

func toCat(t Token) uint32 { return uint32(t - 1) } // SPI (1-based) -> catalog (0-based)
func toTok(c uint32) Token { return Token(c + 1) }

func labelsToCat(ts []Token) []uint32 {
	out := make([]uint32, len(ts))
	for i, t := range ts {
		out[i] = toCat(t)
	}
	slices.Sort(out)
	return out
}

// --- snapshot-scoped resolution: overlay first, then base ---

// nodeLive reports whether a node position is visible to this snapshot.
func (t *diskTx) nodeLive(pos uint64) bool {
	if pre, ok := t.e.ov.Resolve(mvcc.Key{Kind: mvcc.NodeExist, Pos: pos}, t.readSeq); ok {
		return pre.Present
	}
	return t.e.nodes.Exists(pos)
}

// relLive reports whether a relationship position is visible to this snapshot.
func (t *diskTx) relLive(pos uint64) bool {
	if pre, ok := t.e.ov.Resolve(mvcc.Key{Kind: mvcc.RelExist, Pos: pos}, t.readSeq); ok {
		return pre.Present
	}
	return t.e.rels.Exists(pos)
}

// snapLabels returns the node's label set as of the snapshot.
func (t *diskTx) snapLabels(pos uint64) ([]uint32, error) {
	if pre, ok := t.e.ov.Resolve(mvcc.Key{Kind: mvcc.NodeLabels, Pos: pos}, t.readSeq); ok {
		return pre.Labels, nil
	}
	// LabelsRaw, not Labels: a snapshot may still see a node a later transaction
	// deleted from the base, and its retained label bytes are correct here (the
	// overlay above already covered any label change before that delete).
	return t.e.nodes.LabelsRaw(pos)
}

// snapNodeProp returns a node property value (and presence) as of the snapshot.
func (t *diskTx) snapNodeProp(pos uint64, key uint32) (value.Value, bool, error) {
	if pre, ok := t.e.ov.Resolve(mvcc.Key{Kind: mvcc.NodeProp, Pos: pos, Sub: key}, t.readSeq); ok {
		return pre.Val, pre.Present, nil
	}
	return t.e.baseNodeProp(key, pos)
}

// snapRelProp returns a relationship property value (and presence) as of the snapshot.
func (t *diskTx) snapRelProp(pos uint64, key uint32) (value.Value, bool, error) {
	if pre, ok := t.e.ov.Resolve(mvcc.Key{Kind: mvcc.RelProp, Pos: pos, Sub: key}, t.readSeq); ok {
		return pre.Val, pre.Present, nil
	}
	return t.e.baseRelProp(key, pos)
}

// nodePos resolves a node id to its dense position, requiring it visible.
func (t *diskTx) nodePos(id NodeID) (uint64, error) {
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok || !t.nodeLive(pos) {
		return 0, ErrNoSuchNode
	}
	return pos, nil
}

// relPos resolves a relationship id to its dense position, requiring it visible.
func (t *diskTx) relPos(id RelID) (uint64, error) {
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok || !t.relLive(pos) {
		return 0, ErrNoSuchRel
	}
	return pos, nil
}

// --- reads ---

func (t *diskTx) NodeExists(id NodeID) (bool, error) {
	defer t.rguard()()
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok {
		return false, nil
	}
	return t.nodeLive(pos), nil
}

func (t *diskTx) RelExists(id RelID) (bool, error) {
	defer t.rguard()()
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok {
		return false, nil
	}
	return t.relLive(pos), nil
}

func (t *diskTx) NodeLabels(id NodeID) ([]Token, error) {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return nil, err
	}
	cats, err := t.snapLabels(pos)
	if err != nil {
		return nil, err
	}
	out := make([]Token, len(cats))
	for i, c := range cats {
		out[i] = toTok(c)
	}
	return out, nil
}

func (t *diskTx) HasLabel(id NodeID, label Token) (bool, error) {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return false, err
	}
	cats, err := t.snapLabels(pos)
	if err != nil {
		return false, err
	}
	return slices.Contains(cats, toCat(label)), nil
}

func (t *diskTx) NodeProperty(id NodeID, key Token) (value.Value, error) {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return value.Null, err
	}
	v, ok, err := t.snapNodeProp(pos, toCat(key))
	if err != nil || !ok {
		return value.Null, err
	}
	return v, nil
}

func (t *diskTx) RelProperty(id RelID, key Token) (value.Value, error) {
	defer t.rguard()()
	pos, err := t.relPos(id)
	if err != nil {
		return value.Null, err
	}
	v, ok, err := t.snapRelProp(pos, toCat(key))
	if err != nil || !ok {
		return value.Null, err
	}
	return v, nil
}

func (t *diskTx) RelType(id RelID) (Token, error) {
	defer t.rguard()()
	pos, err := t.relPos(id)
	if err != nil {
		return 0, err
	}
	r, err := t.e.rels.Get(pos)
	if err != nil {
		return 0, err
	}
	return toTok(r.Type), nil
}

func (t *diskTx) RelEndpoints(id RelID) (NodeID, NodeID, error) {
	defer t.rguard()()
	pos, err := t.relPos(id)
	if err != nil {
		return 0, 0, err
	}
	r, err := t.e.rels.Get(pos)
	if err != nil {
		return 0, 0, err
	}
	// r.Src and r.Dst are dense node positions; map them back to the stable node
	// ids the API hands out (the reverse of nodePos), so the endpoints come out as
	// external ids, never the internal positions (doc 02 §5.1).
	src, ok := t.e.ids.Eid(idmap.KindNode, r.Src)
	if !ok {
		return 0, 0, ErrNoSuchNode
	}
	dst, ok := t.e.ids.Eid(idmap.KindNode, r.Dst)
	if !ok {
		return 0, 0, ErrNoSuchNode
	}
	return NodeID(src), NodeID(dst), nil
}

// NodePropertyKeys probes every property column for a snapshot-visible value at
// the node's position; a key with a present value is one the node carries. The
// candidate set is the columns that exist (a key never written has no column and
// so can be on no node), which keeps this proportional to the schema, not the
// catalog.
func (t *diskTx) NodePropertyKeys(id NodeID) ([]Token, error) {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return nil, err
	}
	var out []Token
	for _, k := range basePropKeys(t.e.ncols, t.e.nseg) {
		_, ok, err := t.snapNodeProp(pos, k)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, toTok(k))
		}
	}
	return out, nil
}

func (t *diskTx) RelPropertyKeys(id RelID) ([]Token, error) {
	defer t.rguard()()
	pos, err := t.relPos(id)
	if err != nil {
		return nil, err
	}
	var out []Token
	for _, k := range basePropKeys(t.e.rcols, t.e.rseg) {
		_, ok, err := t.snapRelProp(pos, k)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, toTok(k))
		}
	}
	return out, nil
}

func (t *diskTx) ScanLabel(label Token, fn func(NodeID) error) error {
	defer t.rguard()()
	for pos := range uint64(t.e.nodes.Count()) {
		if !t.nodeLive(pos) {
			continue
		}
		if label != 0 {
			cats, err := t.snapLabels(pos)
			if err != nil {
				return err
			}
			if !slices.Contains(cats, toCat(label)) {
				continue
			}
		}
		eid, ok := t.e.ids.Eid(idmap.KindNode, pos)
		if !ok {
			continue
		}
		if err := fn(NodeID(eid)); err != nil {
			return err
		}
	}
	return nil
}

func (t *diskTx) Expand(id NodeID, relType Token, dir Direction, fn func(Neighbor) error) error {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	visible := func(edge uint64) bool { return t.relLive(edge) }
	dirs := dirSlice(dir)
	types := t.typeSlice(relType)
	for _, ty := range types {
		for _, d := range dirs {
			nbrs, err := t.e.adj.ExpandWith(pos, ty, d, visible)
			if err != nil {
				return err
			}
			for _, nb := range nbrs {
				neid, ok := t.e.ids.Eid(idmap.KindNode, nb.Node)
				if !ok {
					continue
				}
				reid, ok := t.e.ids.Eid(idmap.KindRel, nb.Edge)
				if !ok {
					continue
				}
				if err := fn(Neighbor{Rel: RelID(reid), Node: NodeID(neid), Type: toTok(ty)}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// NeighborsByPos implements engine.Adjacency: it returns the snapshot-visible
// neighbors of a node along a type and direction as a slice sorted by dense
// position, reusing the supplied buffer. adj.ExpandWith already returns each base
// and delta run sorted by dense position, so when a single (type, direction) run
// is requested the result is sorted with no extra work; the multi-run wildcard
// case (zero type or both directions) merges into one slice and sorts once. The
// dense position is the neighbor's adjacency node id before id translation, which
// is the same key adjacency lists are packed on, so two lists are mergeable on it.
func (t *diskTx) NeighborsByPos(id NodeID, relType Token, dir Direction, buf []PosNeighbor) ([]PosNeighbor, error) {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return nil, err
	}
	visible := func(edge uint64) bool { return t.relLive(edge) }
	dirs := dirSlice(dir)
	types := t.typeSlice(relType)
	out := buf[:0]
	multi := len(dirs) > 1 || len(types) > 1
	for _, ty := range types {
		for _, d := range dirs {
			nbrs, err := t.e.adj.ExpandWith(pos, ty, d, visible)
			if err != nil {
				return nil, err
			}
			for _, nb := range nbrs {
				neid, ok := t.e.ids.Eid(idmap.KindNode, nb.Node)
				if !ok {
					continue
				}
				reid, ok := t.e.ids.Eid(idmap.KindRel, nb.Edge)
				if !ok {
					continue
				}
				out = append(out, PosNeighbor{Pos: nb.Node, Rel: RelID(reid), Node: NodeID(neid), Type: toTok(ty)})
			}
		}
	}
	if multi {
		slices.SortFunc(out, func(a, b PosNeighbor) int { return cmp.Compare(a.Pos, b.Pos) })
	}
	return out, nil
}

// dirSlice maps an SPI direction to the adjacency directions to walk.
func dirSlice(dir Direction) []adj.Dir {
	switch dir {
	case Outgoing:
		return []adj.Dir{adj.Out}
	case Incoming:
		return []adj.Dir{adj.In}
	default:
		return []adj.Dir{adj.Out, adj.In}
	}
}

// typeSlice returns the catalog type tokens to expand: the one requested, or all
// known types when relType is the zero wildcard.
func (t *diskTx) typeSlice(relType Token) []uint32 {
	if relType != 0 {
		return []uint32{toCat(relType)}
	}
	n := t.e.cat.Count(catalog.KindRelType)
	out := make([]uint32, n)
	for i := range out {
		out[i] = uint32(i)
	}
	return out
}

// Degree returns a node's relationship count along a type and direction from the
// adjacency's maintained degree statistic, in O(1) per (type, direction) slot
// rather than by materializing the neighbor run (doc 04 §12.5). It is the planner
// statistic: it reflects the latest committed state and the writer's own writes,
// not a reader's snapshot. A zero relType sums all types; Both sums both directions.
func (t *diskTx) Degree(id NodeID, relType Token, dir Direction) (int64, error) {
	defer t.rguard()()
	pos, err := t.nodePos(id)
	if err != nil {
		return 0, err
	}
	var c int64
	for _, ty := range t.typeSlice(relType) {
		for _, d := range dirSlice(dir) {
			n, err := t.e.adj.Degree(pos, ty, d)
			if err != nil {
				return 0, err
			}
			c += n
		}
	}
	return c, nil
}

// --- writes ---

func (t *diskTx) CreateNode(labels []Token) (NodeID, error) {
	if !t.write {
		return 0, ErrReadOnlyTx
	}
	eid, pos, err := t.e.ids.Alloc(idmap.KindNode)
	if err != nil {
		return 0, err
	}
	// Before this node existed, an older snapshot saw it as absent.
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.NodeExist, Pos: pos},
		pre: mvcc.Pre{Present: false},
	})
	cats := labelsToCat(labels)
	npos, err := t.e.nodes.Create(cats)
	if err != nil {
		return 0, err
	}
	if npos != pos {
		return 0, ErrIDMapDesync
	}
	for _, c := range cats {
		if err := t.e.st.AddLabel(c, +1); err != nil {
			return 0, err
		}
	}
	return NodeID(eid), nil
}

func (t *diskTx) DeleteNode(id NodeID) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	attached, err := t.hasAnyRel(pos)
	if err != nil {
		return err
	}
	if attached {
		return ErrDetachRequired
	}
	// An older snapshot still sees the node, so retain its existence pre-image.
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.NodeExist, Pos: pos},
		pre: mvcc.Pre{Present: true},
	})
	// Decrement the per-label counts for the labels this node carried.
	cats, err := t.e.nodes.Labels(pos)
	if err != nil {
		return err
	}
	for _, c := range cats {
		if err := t.e.st.AddLabel(c, -1); err != nil {
			return err
		}
	}
	// The id-map mapping is kept (not removed) so older snapshots can still
	// resolve the position from the element id; id reclamation is deferred.
	return t.e.nodes.Delete(pos)
}

// hasAnyRel reports whether a node position has any live relationship in either
// direction across all known types, from the writer's latest view.
func (t *diskTx) hasAnyRel(pos uint64) (bool, error) {
	for ty := range uint32(t.e.cat.Count(catalog.KindRelType)) {
		for _, d := range []adj.Dir{adj.Out, adj.In} {
			nbrs, err := t.e.adj.Expand(pos, ty, d)
			if err != nil {
				return false, err
			}
			if len(nbrs) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func (t *diskTx) CreateRel(src, dst NodeID, relType Token) (RelID, error) {
	if !t.write {
		return 0, ErrReadOnlyTx
	}
	spos, err := t.nodePos(src)
	if err != nil {
		return 0, err
	}
	dpos, err := t.nodePos(dst)
	if err != nil {
		return 0, err
	}
	ty := toCat(relType)
	eid, pos, err := t.e.ids.Alloc(idmap.KindRel)
	if err != nil {
		return 0, err
	}
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.RelExist, Pos: pos},
		pre: mvcc.Pre{Present: false},
	})
	rpos, err := t.e.rels.Create(ty, spos, dpos)
	if err != nil {
		return 0, err
	}
	if rpos != pos {
		return 0, ErrIDMapDesync
	}
	t.e.adj.Insert(ty, spos, dpos, rpos)
	if err := t.e.st.AddRelType(ty, +1); err != nil {
		return 0, err
	}
	return RelID(eid), nil
}

func (t *diskTx) DeleteRel(id RelID) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.relPos(id)
	if err != nil {
		return err
	}
	// Read the edge before tombstoning it so the adjacency can correct the degree
	// statistic for its endpoints.
	r, err := t.e.rels.Get(pos)
	if err != nil {
		return err
	}
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.RelExist, Pos: pos},
		pre: mvcc.Pre{Present: true},
	})
	t.e.adj.Remove(r.Type, r.Src, r.Dst)
	if err := t.e.st.AddRelType(r.Type, -1); err != nil {
		return err
	}
	return t.e.rels.Delete(pos)
}

func (t *diskTx) SetNodeProperty(id NodeID, key Token, v value.Value) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	c := toCat(key)
	old, present, err := t.e.baseNodeProp(c, pos)
	if err != nil {
		return err
	}
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.NodeProp, Pos: pos, Sub: c},
		pre: mvcc.Pre{Present: present, Val: old},
	})
	if v.IsNull() {
		// Tombstone, not Remove: the naive store is a delta over the segmented
		// base, so a removal must hide any folded base value rather than clear a
		// flag a base read cannot tell apart from a never written position.
		return t.e.ncols.Tombstone(c, pos)
	}
	return t.e.ncols.Set(c, pos, v)
}

func (t *diskTx) SetRelProperty(id RelID, key Token, v value.Value) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.relPos(id)
	if err != nil {
		return err
	}
	c := toCat(key)
	old, present, err := t.e.baseRelProp(c, pos)
	if err != nil {
		return err
	}
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.RelProp, Pos: pos, Sub: c},
		pre: mvcc.Pre{Present: present, Val: old},
	})
	if v.IsNull() {
		// See SetNodeProperty: a delta removal tombstones the base value.
		return t.e.rcols.Tombstone(c, pos)
	}
	return t.e.rcols.Set(c, pos, v)
}

func (t *diskTx) AddLabel(id NodeID, label Token) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	cats, err := t.e.nodes.Labels(pos)
	if err != nil {
		return err
	}
	c := toCat(label)
	if slices.Contains(cats, c) {
		return nil
	}
	t.recordLabels(pos, cats)
	next := append(slices.Clone(cats), c)
	slices.Sort(next)
	if err := t.e.nodes.SetLabels(pos, next); err != nil {
		return err
	}
	return t.e.st.AddLabel(c, +1)
}

func (t *diskTx) RemoveLabel(id NodeID, label Token) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	cats, err := t.e.nodes.Labels(pos)
	if err != nil {
		return err
	}
	c := toCat(label)
	idx := slices.Index(cats, c)
	if idx < 0 {
		return nil
	}
	t.recordLabels(pos, cats)
	next := slices.Delete(slices.Clone(cats), idx, idx+1)
	if err := t.e.nodes.SetLabels(pos, next); err != nil {
		return err
	}
	return t.e.st.AddLabel(c, -1)
}

// Intern interns a name within this write transaction. The catalog appends the
// new name to its pager-backed log, which becomes durable when this transaction
// commits and is rolled back if it aborts, so an aborted write leaves no orphan
// token. It takes no lock: a write transaction already holds the engine's
// exclusive lock, so it must not re-take it (the lock is not reentrant). The
// engine's own Intern method is the standalone equivalent for between-transaction
// schema setup.
func (t *diskTx) Intern(kind catalog.Kind, name string) (Token, error) {
	if !t.write {
		return 0, ErrReadOnlyTx
	}
	c, _, err := t.e.cat.Intern(kind, name)
	if err != nil {
		return 0, err
	}
	return toTok(c), nil
}

// Lookup resolves an interned name to its token from this transaction's catalog
// view. It reads the catalog directly without locking: a write transaction holds
// the engine lock for its whole life, so it must not re-take it, and the catalog
// it reads includes the names this transaction has just interned (doc 13 §9).
func (t *diskTx) Lookup(kind catalog.Kind, name string) (Token, bool) {
	c, ok := t.e.cat.Lookup(kind, name)
	if !ok {
		return 0, false
	}
	return toTok(c), true
}

// recordLabels retains the current label set as the pre-image for older snapshots.
func (t *diskTx) recordLabels(pos uint64, cats []uint32) {
	t.pending = append(t.pending, pendingPre{
		key: mvcc.Key{Kind: mvcc.NodeLabels, Pos: pos},
		pre: mvcc.Pre{Present: true, Labels: slices.Clone(cats)},
	})
}

// --- lifecycle ---

func (t *diskTx) Commit() error {
	if t.done {
		return nil
	}
	// Enforce constraints before the durability point: a violation aborts the
	// whole transaction (rolling back its writes) and surfaces a typed error,
	// rather than committing data the schema forbids (doc 13 §12, §16).
	if t.write {
		if err := t.validateUnique(); err != nil {
			_ = t.Abort()
			return err
		}
		if err := t.validateExistence(); err != nil {
			_ = t.Abort()
			return err
		}
		if err := t.validateType(); err != nil {
			_ = t.Abort()
			return err
		}
	}
	t.done = true
	t.e.oracle.End(t.snap)
	if !t.write {
		return nil
	}
	defer t.e.mu.Unlock()
	seq, err := t.e.commitPager()
	if err != nil {
		return err
	}
	// This write transaction reached its durability point, so count it for the commit total, the
	// amortization numerator against the WAL fsync count (doc 20 §5.3). It is bumped under the engine
	// write lock this commit already holds; the metrics path reads it lock-free.
	t.e.commitsTotal.Add(1)
	// The pager commit may have grown or truncated the file and changed the free list, so refresh
	// the published size and free-page count for their gauges while the write lock is still held
	// (doc 20 §4.2).
	t.e.publishFileSize()
	t.e.publishFreelistPages()
	// Publish pre-images at the commit sequence (durable-before-visible: the
	// pager commit above is the durability point, this publication the visibility
	// point). Older snapshots now resolve the retained values; newer ones read
	// the freshly committed base.
	for _, pp := range t.pending {
		t.e.ov.Record(seq, pp.key, pp.pre)
	}
	// Refresh the property indexes from the freshly committed base so a later read
	// seek sees this transaction's writes. The rebuild is the correctness-first
	// maintenance form (doc 07 §9); a write pays it only when an index is declared.
	if t.e.hasIndexes() {
		if err := t.e.rebuildIndexes(); err != nil {
			return err
		}
	}
	return nil
}

func (t *diskTx) Abort() error {
	if t.done {
		return nil
	}
	t.done = true
	t.e.oracle.End(t.snap)
	if !t.write {
		return nil
	}
	defer t.e.mu.Unlock()
	// Nothing was published to the overlay, so the abort only rewinds the pager
	// and rebuilds the in-memory store state (id-map maps, record counts, the
	// adjacency delta) from the rolled-back, last-committed prefix.
	if err := t.e.p.Rollback(); err != nil {
		return err
	}
	return t.e.load(false)
}
