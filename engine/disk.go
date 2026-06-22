package engine

import (
	"errors"
	"io"
	"slices"
	"sync"

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

	// bc fronts the segmented-base read with decoded segments, so a repeated point
	// read in a segment does not re-decode it (doc 14 §4). It is an in-memory cache,
	// never a source of truth: every cached segment carries the checkpoint epoch it
	// was built at, and epoch invalidates them all when a fold rebuilds the base.
	bc *blockcache.Cache
	// epoch is the checkpoint generation of the segmented base. A fold rebuilds the
	// base with a new segment layout and bumps it, so a read at the new epoch misses
	// every entry cached against the old base.
	epoch uint64
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
	return e, nil
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
	if err := e.adj.Checkpoint(uint64(e.nodes.Count())); err != nil {
		return err
	}
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
	e.ov.GC(e.oracle.Watermark())
	return nil
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
