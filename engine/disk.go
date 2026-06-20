package engine

import (
	"errors"
	"slices"
	"sync"

	"github.com/tamnd/gr/adj"
	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/column"
	"github.com/tamnd/gr/idmap"
	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
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
// CSR adjacency over one pager, behind the frozen engine SPI ([engine.Engine]).
//
// It is the single-writer-first realization (doc 06 §5, doc 25 §4 deliverable 9
// in its first form): a write transaction holds the engine's write lock for its
// duration and is made durable by the pager's commit; read transactions share a
// read lock. Full MVCC snapshot isolation — readers that never block during a
// write, version tags, the watermark oracle, and version GC — is the next PR; it
// slots in behind this same interface without the query stack noticing, which is
// the whole point of the SPI seam.
//
// Catalog interning (mapping label/type/property-key strings to tokens) is a
// setup-time concern outside the per-transaction SPI, exposed as Intern and
// TokenName on the concrete type. SPI tokens are one-based: token 0 is the
// SPI's "all labels"/"all types" wildcard (see ScanLabel and Expand), so the
// engine offsets catalog tokens by one at the boundary.
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
	adj    *adj.Adj
	closed bool
}

// Open opens or creates a graph database at path. A fresh file gets empty stores,
// committed so the structure is durable; an existing file reopens its stores,
// recovering to the committed prefix via the pager.
func Open(fsys vfs.VFS, path string, opt pager.Options) (*DiskEngine, error) {
	p, err := pager.Open(fsys, path, opt)
	if err != nil {
		return nil, err
	}
	e := &DiskEngine{p: p}
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
	return e, nil
}

// load (re)builds the store handles over the current pager state, creating the
// stores when fresh and opening them otherwise. It is also used to rebuild state
// after a rollback.
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
		e.adj, err = adj.Create(e.p, e.secs, e.rels)
		return err
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
	e.adj, err = adj.Open(e.p, e.secs, e.rels)
	return err
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
	if err := e.p.Commit(); err != nil {
		return 0, err
	}
	return Token(t + 1), nil
}

// Lookup returns the one-based token for an already-interned name, or false. It
// takes no lock beyond a read lock and does not commit.
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

// Begin opens a transaction, taking the write lock for a write transaction or the
// read lock otherwise, held until Commit or Abort.
func (e *DiskEngine) Begin(write bool) (Tx, error) {
	if write {
		e.mu.Lock()
	} else {
		e.mu.RLock()
	}
	return &diskTx{e: e, write: write}, nil
}

// Checkpoint folds the adjacency delta into the base CSR and makes everything
// durable. It takes the write lock, so it does not run concurrently with a write
// transaction.
func (e *DiskEngine) Checkpoint() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.adj.Checkpoint(uint64(e.nodes.Count())); err != nil {
		return err
	}
	return e.p.Commit()
}

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

// diskTx is a transaction over the disk engine. It holds the lock acquired in
// Begin; the read methods consult the committed stores, and the write methods
// mutate them, made durable at Commit.
type diskTx struct {
	e     *DiskEngine
	write bool
	done  bool
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

// nodePos resolves a node id to its dense position, requiring it to be live.
func (t *diskTx) nodePos(id NodeID) (uint64, error) {
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok || !t.e.nodes.Exists(pos) {
		return 0, ErrNoSuchNode
	}
	return pos, nil
}

// relPos resolves a relationship id to its dense position, requiring it live.
func (t *diskTx) relPos(id RelID) (uint64, error) {
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok || !t.e.rels.Exists(pos) {
		return 0, ErrNoSuchRel
	}
	return pos, nil
}

// --- reads ---

func (t *diskTx) NodeExists(id NodeID) (bool, error) {
	pos, ok := t.e.ids.Pos(uint64(id))
	if !ok {
		return false, nil
	}
	return t.e.nodes.Exists(pos), nil
}

func (t *diskTx) NodeLabels(id NodeID) ([]Token, error) {
	pos, err := t.nodePos(id)
	if err != nil {
		return nil, err
	}
	cats, err := t.e.nodes.Labels(pos)
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
	pos, err := t.nodePos(id)
	if err != nil {
		return false, err
	}
	return t.e.nodes.HasLabel(pos, toCat(label))
}

func (t *diskTx) NodeProperty(id NodeID, key Token) (value.Value, error) {
	pos, err := t.nodePos(id)
	if err != nil {
		return value.Null, err
	}
	v, ok, err := t.e.ncols.Get(toCat(key), pos)
	if err != nil || !ok {
		return value.Null, err
	}
	return v, nil
}

func (t *diskTx) RelProperty(id RelID, key Token) (value.Value, error) {
	pos, err := t.relPos(id)
	if err != nil {
		return value.Null, err
	}
	v, ok, err := t.e.rcols.Get(toCat(key), pos)
	if err != nil || !ok {
		return value.Null, err
	}
	return v, nil
}

func (t *diskTx) ScanLabel(label Token, fn func(NodeID) error) error {
	for pos := range uint64(t.e.nodes.Count()) {
		if !t.e.nodes.Exists(pos) {
			continue
		}
		if label != 0 {
			has, err := t.e.nodes.HasLabel(pos, toCat(label))
			if err != nil {
				return err
			}
			if !has {
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
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	dirs := dirSlice(dir)
	types := t.typeSlice(relType)
	for _, ty := range types {
		for _, d := range dirs {
			nbrs, err := t.e.adj.Expand(pos, ty, d)
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

func (t *diskTx) Degree(id NodeID, relType Token, dir Direction) (int64, error) {
	var c int64
	err := t.Expand(id, relType, dir, func(Neighbor) error { c++; return nil })
	return c, err
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
	npos, err := t.e.nodes.Create(labelsToCat(labels))
	if err != nil {
		return 0, err
	}
	if npos != pos {
		return 0, ErrIDMapDesync
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
	if err := t.e.nodes.Delete(pos); err != nil {
		return err
	}
	return t.e.ids.Delete(uint64(id))
}

// hasAnyRel reports whether a node position has any live relationship in either
// direction across all known types.
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
	rpos, err := t.e.rels.Create(ty, spos, dpos)
	if err != nil {
		return 0, err
	}
	if rpos != pos {
		return 0, ErrIDMapDesync
	}
	t.e.adj.Insert(ty, spos, dpos, rpos)
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
	if err := t.e.rels.Delete(pos); err != nil {
		return err
	}
	return t.e.ids.Delete(uint64(id))
}

func (t *diskTx) SetNodeProperty(id NodeID, key Token, v value.Value) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.nodePos(id)
	if err != nil {
		return err
	}
	if v.IsNull() {
		return t.e.ncols.Remove(toCat(key), pos)
	}
	return t.e.ncols.Set(toCat(key), pos, v)
}

func (t *diskTx) SetRelProperty(id RelID, key Token, v value.Value) error {
	if !t.write {
		return ErrReadOnlyTx
	}
	pos, err := t.relPos(id)
	if err != nil {
		return err
	}
	if v.IsNull() {
		return t.e.rcols.Remove(toCat(key), pos)
	}
	return t.e.rcols.Set(toCat(key), pos, v)
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
	cats = append(cats, c)
	slices.Sort(cats)
	return t.e.nodes.SetLabels(pos, cats)
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
	cats = slices.Delete(cats, idx, idx+1)
	return t.e.nodes.SetLabels(pos, cats)
}

// --- lifecycle ---

func (t *diskTx) Commit() error {
	if t.done {
		return nil
	}
	t.done = true
	if !t.write {
		t.e.mu.RUnlock()
		return nil
	}
	defer t.e.mu.Unlock()
	return t.e.p.Commit()
}

func (t *diskTx) Abort() error {
	if t.done {
		return nil
	}
	t.done = true
	if !t.write {
		t.e.mu.RUnlock()
		return nil
	}
	defer t.e.mu.Unlock()
	if err := t.e.p.Rollback(); err != nil {
		return err
	}
	// In-memory store state (id-map maps, record counts, the adjacency delta)
	// was mutated during the transaction; rebuild it from the rolled-back pager.
	return t.e.load(false)
}
