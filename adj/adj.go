// Package adj is gr's CSR index-free adjacency: the expand primitive that, given
// a node and a relationship type and direction, yields its neighbors in
// O(1)-to-the-run plus O(degree) (spec 2060 doc 04 §3, §5, §8; doc 25 §4
// deliverables 3, 4, 7, 10).
//
// The design has three pieces that mirror the spec's base/delta/checkpoint:
//
//   - The relationship store (package rel) is the durable edge log: every edge,
//     live or tombstoned, is a record there. It is the source of truth.
//   - The base CSR is a derived, foldable index: per (type, direction) slot, an
//     offset array indexed by source node position plus parallel neighbor and
//     edge-id arrays, with each source's run sorted by neighbor. It holds the
//     edges folded so far, recorded by a folded relationship count.
//   - The delta is the in-memory adjacency over the un-folded, live tail of the
//     relationship store. It is rebuilt on open by scanning relationship
//     positions at and beyond the folded count; it needs no durable pages of its
//     own because the relationship store it derives from is already durable.
//
// expand merges base and delta: it reads the base run for the source and the
// delta inserts for the source, drops any whose edge is no longer live in the
// relationship store (which is how deletes take effect uniformly across base and
// delta), and returns the sorted neighbor list. checkpoint folds the delta into
// the base by rebuilding the CSR from every live relationship and advancing the
// folded count, after which the delta is empty.
//
// Dense nodes (supernodes) need no special record (doc 04 §12; ADR-20): a
// high-degree node's neighbors are a long contiguous CSR run, and a typed,
// directed expand touches only that one (type, direction) slot's run, never all
// of the node's edges, so a million-edge celebrity contributes only its
// KNOWS-forward slice to a KNOWS-forward expand. Degree is exposed without
// touching edges at all: a slot's base degree is the offset delta
// offset[src+1]-offset[src] read straight from the offset array, plus an
// in-memory tail adjustment for edges created or deleted since the last
// checkpoint. This is the engine half of degree-aware planning (doc 04 §12.5):
// the engine maintains and exposes accurate degree, the M4 planner uses it.
//
// This is the M1, single-writer, no-MVCC realization. A standalone durable delta
// with its own pages and per-entry version tags, segmented and incremental
// checkpointing, and dense-id reuse arrive in later PRs; the merge and checkpoint
// here are written so those refinements slot in without changing the expand
// contract.
package adj

import (
	"slices"
	"sync"

	"github.com/tamnd/gr/colcodec"
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/store"
)

// Dir is an expand direction.
type Dir uint8

const (
	// Out expands a node's outgoing edges (source -> neighbor).
	Out Dir = 0
	// In expands a node's incoming edges (neighbor -> source).
	In Dir = 1
)

// Neighbor is one entry of an expand result: the neighbor node's dense position
// and the relationship's dense position (its edge id), so reaching the edge's
// properties is O(1) after the neighbor (doc 04 §5.3).
type Neighbor struct {
	Node uint64
	Edge uint64
}

// slot folds a relationship type token and a direction into one dense key. Type
// tokens are dense, so slots are dense and the directory Vector packs tightly.
func slot(relType uint32, d Dir) uint32 { return relType*2 + uint32(d) }

// dirStride is one base-directory cell. The three CSR arrays of a slot are stored
// compressed as colcodec blobs packed into one append-only log (doc 15 §15), so a
// cell records the log's head page, the byte length of each of the three blobs in
// log order (offsets, neighbors, edges), and the logical offset count. The blob
// byte lengths slice the three apart; the offset count bounds the source range
// without decoding.
const dirStride = 40

// base holds the decoded base CSR arrays for one slot. openBase decodes the three
// compressed blobs once and caches the result here, so a run lookup indexes plain
// slices rather than re-reading pages (doc 15 §15.9).
type base struct {
	off []uint64 // offsets, length nodeCount+1
	nbr []uint64 // neighbor node positions
	edg []uint64 // edge (relationship) positions
}

// Adj is the adjacency index over a relationship store.
type Adj struct {
	p      *pager.Pager
	secs   *store.Sections
	rels   *rel.Store
	dir    *store.Vector
	folded uint64
	delta  map[uint32]map[uint64][]Neighbor // slot -> src -> sorted neighbors
	// cacheMu guards cache against concurrent readers. The base cache is filled
	// lazily on read (openBase opens a slot's vectors the first time it is
	// expanded), so morsel-parallel workers expanding different slots at once
	// would otherwise write the map concurrently, a fatal access. delta and
	// degTail need no lock: they are written only on the write path, which holds
	// the engine's exclusive lock, and read-only under a snapshot.
	cacheMu sync.Mutex
	cache   map[uint32]*base // opened base arrays, cleared on checkpoint
	// degTail is the net live-degree change since the last checkpoint, per slot
	// and source: edges inserted into the tail (counted +1) minus edges removed
	// (counted -1, whether they sat in the base or the tail). A slot's degree is
	// the base offset delta plus this. It is reset at checkpoint, when the base
	// offsets again reflect every live edge.
	degTail map[uint32]map[uint64]int64
	// degStats is the per-slot degree distribution computed from the base offset
	// array at the last open or checkpoint: it feeds the supernode and skew
	// metrics (doc 20 §6.2). It is rebuilt whenever the base does, so it reflects
	// only the folded edges, which is exactly the durable state a planner reasons
	// about. It is written on the write/checkpoint path and read by DegreeStats,
	// both under the engine's exclusive lock, so it needs no lock of its own.
	degStats map[uint32]DegreeSummary
}

// DegreeSummary is one slot's degree distribution, computed by a single pass over
// the base CSR offset array with no edge reads (doc 20 §6.2). It counts only
// participating sources, those with at least one edge in the slot, so a slot the
// schema knows but a node never uses does not drag the percentiles to zero.
type DegreeSummary struct {
	Nodes uint64 // sources with degree >= 1 in this slot
	Edges uint64 // sum of those sources' degrees
	Max   uint64 // largest single degree, the supernode tip
	P50   uint64 // median degree over participating sources
	P99   uint64 // 99th-percentile degree over participating sources
}

// Create initializes a fresh adjacency: an empty base directory and a zero
// folded count, recorded in the section directory (durable at the next commit).
func Create(p *pager.Pager, secs *store.Sections, rels *rel.Store) (*Adj, error) {
	dir, err := store.CreateVector(p, dirStride, format.PageTypeRelGroup)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecAdjDir, dir.Head(), 0); err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecAdjMeta, 0, 0); err != nil {
		return nil, err
	}
	return &Adj{
		p: p, secs: secs, rels: rels, dir: dir,
		delta:    map[uint32]map[uint64][]Neighbor{},
		cache:    map[uint32]*base{},
		degTail:  map[uint32]map[uint64]int64{},
		degStats: map[uint32]DegreeSummary{},
	}, nil
}

// Open reopens the adjacency from the section directory and rebuilds the
// in-memory delta by scanning the un-folded tail of the relationship store.
func Open(p *pager.Pager, secs *store.Sections, rels *rel.Store) (*Adj, error) {
	head, count, err := secs.Get(store.SecAdjDir)
	if err != nil {
		return nil, err
	}
	dir, err := store.OpenVector(p, head, dirStride, int(count))
	if err != nil {
		return nil, err
	}
	_, folded, err := secs.Get(store.SecAdjMeta)
	if err != nil {
		return nil, err
	}
	a := &Adj{
		p: p, secs: secs, rels: rels, dir: dir, folded: folded,
		delta:    map[uint32]map[uint64][]Neighbor{},
		cache:    map[uint32]*base{},
		degTail:  map[uint32]map[uint64]int64{},
		degStats: map[uint32]DegreeSummary{},
	}
	if err := a.rebuildDelta(); err != nil {
		return nil, err
	}
	if err := a.recomputeDegStats(); err != nil {
		return nil, err
	}
	return a, nil
}

// rebuildDelta scans relationship positions [folded, count) and indexes the live
// ones into the in-memory delta, both directions.
func (a *Adj) rebuildDelta() error {
	for pos := a.folded; pos < uint64(a.rels.Count()); pos++ {
		if !a.rels.Exists(pos) {
			continue
		}
		r, err := a.rels.Get(pos)
		if err != nil {
			return err
		}
		a.insertDelta(r.Type, r.Src, r.Dst, pos)
	}
	return nil
}

// Insert records a freshly created edge in the in-memory delta. The caller has
// already created the durable relationship record; this only indexes it, so a
// reopen rebuilds the same delta from that record.
func (a *Adj) Insert(relType uint32, src, dst, relPos uint64) {
	a.insertDelta(relType, src, dst, relPos)
}

func (a *Adj) insertDelta(relType uint32, src, dst, relPos uint64) {
	a.addDelta(slot(relType, Out), src, Neighbor{Node: dst, Edge: relPos})
	a.addDelta(slot(relType, In), dst, Neighbor{Node: src, Edge: relPos})
	a.adjustDeg(slot(relType, Out), src, +1)
	a.adjustDeg(slot(relType, In), dst, +1)
}

// Remove records the deletion of an edge for degree accounting. The caller has
// already (or is about to) tombstone the durable relationship record and supplies
// its type and endpoints; the delta neighbor list is left untouched, because a
// deleted edge is dropped by the expand merge's visibility test, not by removal
// from the list. Degree, which does not consult that list, is corrected here.
func (a *Adj) Remove(relType uint32, src, dst uint64) {
	a.adjustDeg(slot(relType, Out), src, -1)
	a.adjustDeg(slot(relType, In), dst, -1)
}

// adjustDeg adds delta to a slot's tail degree for a source.
func (a *Adj) adjustDeg(s uint32, src uint64, delta int64) {
	m := a.degTail[s]
	if m == nil {
		m = map[uint64]int64{}
		a.degTail[s] = m
	}
	m[src] += delta
}

// addDelta inserts a neighbor into the sorted per-source delta list.
func (a *Adj) addDelta(s uint32, src uint64, nb Neighbor) {
	m := a.delta[s]
	if m == nil {
		m = map[uint64][]Neighbor{}
		a.delta[s] = m
	}
	list := m[src]
	i, _ := slices.BinarySearchFunc(list, nb, cmpNeighbor)
	m[src] = slices.Insert(list, i, nb)
}

func cmpNeighbor(a, b Neighbor) int {
	if a.Node != b.Node {
		if a.Node < b.Node {
			return -1
		}
		return 1
	}
	switch {
	case a.Edge < b.Edge:
		return -1
	case a.Edge > b.Edge:
		return 1
	default:
		return 0
	}
}

// Expand returns the live neighbors of src along the given type and direction,
// base and delta merged, sorted by neighbor then edge. An edge that is no longer
// live in the relationship store is dropped, which is how a delete takes effect
// against both the base and the delta.
func (a *Adj) Expand(src uint64, relType uint32, d Dir) ([]Neighbor, error) {
	return a.ExpandWith(src, relType, d, a.rels.Exists)
}

// ExpandWith is Expand with a caller-supplied edge-visibility predicate in place
// of the default liveness test. The engine passes a snapshot-scoped predicate so
// an edge appears only if its relationship is visible to the reader's snapshot
// (doc 04 §11.3), folding MVCC visibility into the same merge that handles
// deletes — a deleted or not-yet-visible edge is simply one the predicate drops.
func (a *Adj) ExpandWith(src uint64, relType uint32, d Dir, visible func(edge uint64) bool) ([]Neighbor, error) {
	s := slot(relType, d)
	var out []Neighbor

	run, err := a.baseRun(s, src)
	if err != nil {
		return nil, err
	}
	for _, nb := range run {
		if visible(nb.Edge) {
			out = append(out, nb)
		}
	}
	for _, nb := range a.delta[s][src] {
		if visible(nb.Edge) {
			out = append(out, nb)
		}
	}
	slices.SortFunc(out, cmpNeighbor)
	return out, nil
}

// Degree returns the live degree of a source along a type and direction without
// touching its edges: the base offset delta plus the tail adjustment (doc 04
// §12.5). It is the engine-maintained degree statistic the planner reads, so it
// reflects the latest committed state (and the current writer's own writes), not
// a reader's snapshot; for a supernode this is O(1), where materializing the run
// to count would be O(degree).
//
// Between a reopen and the next checkpoint the count can over-report base edges
// that were deleted before the reopen, because the tail adjustment is in-memory
// and a reopen rebuilds only the un-folded tail; the next checkpoint folds those
// deletions out of the base and the count is exact again. This is acceptable for
// a planner statistic and self-healing.
func (a *Adj) Degree(src uint64, relType uint32, d Dir) (int64, error) {
	s := slot(relType, d)
	bd, err := a.baseDegree(s, src)
	if err != nil {
		return 0, err
	}
	return max(bd+a.degTail[s][src], 0), nil
}

// baseDegree reads a source's folded degree in a slot straight from the offset
// array (offset[src+1]-offset[src]), or zero when the slot has no base or the
// source is beyond the folded node range.
func (a *Adj) baseDegree(s uint32, src uint64) (int64, error) {
	if int(s) >= a.dir.Count() {
		return 0, nil
	}
	b, offLen, err := a.openBase(s)
	if err != nil {
		return 0, err
	}
	if b == nil || src+1 >= offLen {
		return 0, nil
	}
	return int64(b.off[src+1] - b.off[src]), nil
}

// baseRun returns the base CSR run for a source in a slot, or nil if the slot has
// no base or the source is beyond the folded node range.
func (a *Adj) baseRun(s uint32, src uint64) ([]Neighbor, error) {
	if int(s) >= a.dir.Count() {
		return nil, nil
	}
	b, offLen, err := a.openBase(s)
	if err != nil {
		return nil, err
	}
	if b == nil || src+1 >= offLen {
		return nil, nil
	}
	lo, hi := b.off[src], b.off[src+1]
	out := make([]Neighbor, 0, hi-lo)
	for k := lo; k < hi; k++ {
		out = append(out, Neighbor{Node: b.nbr[k], Edge: b.edg[k]})
	}
	return out, nil
}

// openBase decodes and caches a slot's base arrays from its directory cell. It
// returns the offset-array length so callers can bound the source range. The three
// arrays are colcodec blobs packed in one log; this decodes all three at once and
// caches them, so a later run lookup for the same slot reads from memory.
func (a *Adj) openBase(s uint32) (*base, uint64, error) {
	c, err := a.readDir(s)
	if err != nil {
		return nil, 0, err
	}
	if c.offLen == 0 {
		return nil, 0, nil
	}
	a.cacheMu.Lock()
	if b, ok := a.cache[s]; ok {
		a.cacheMu.Unlock()
		return b, c.offLen, nil
	}
	a.cacheMu.Unlock()

	// Decode the slot's compressed vectors without the lock: OpenLog walks the page
	// chain and the colcodec decode is pure CPU, so holding the lock across them would
	// serialize cold expands of different slots. The decoded arrays hold no pins, so a
	// duplicate built by a peer racing the same slot is cheap to discard.
	total := int(c.offBytes + c.nbrBytes + c.edgBytes)
	log, err := store.OpenLog(a.p, c.logHead, total)
	if err != nil {
		return nil, 0, err
	}
	blob := make([]byte, total)
	if err := log.Read(0, total, blob); err != nil {
		return nil, 0, err
	}
	off, err := decodeArray(blob[:c.offBytes])
	if err != nil {
		return nil, 0, err
	}
	nbr, err := decodeArray(blob[c.offBytes : c.offBytes+c.nbrBytes])
	if err != nil {
		return nil, 0, err
	}
	edg, err := decodeArray(blob[c.offBytes+c.nbrBytes:])
	if err != nil {
		return nil, 0, err
	}
	b := &base{off: off, nbr: nbr, edg: edg}

	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	// A peer may have opened the same slot while this call walked the chain; if so,
	// keep theirs so every reader shares one base per slot.
	if existing, ok := a.cache[s]; ok {
		return existing, c.offLen, nil
	}
	a.cache[s] = b
	return b, c.offLen, nil
}

// decodeArray decodes one colcodec integer blob into unsigned positions. The CSR
// arrays are all non-negative (offsets, dense node ids, dense edge ids), so the
// int64 the codec yields maps straight back to the uint64 it was encoded from.
func decodeArray(blob []byte) ([]uint64, error) {
	vals, err := colcodec.Decode(blob)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, len(vals))
	for i, v := range vals {
		out[i] = uint64(v)
	}
	return out, nil
}

// freeOldBase returns a slot's current base CSR vectors to the pager free list.
// A checkpoint calls it before rebuilding the slot, because the rebuild reads the
// live relationships rather than the old base, so the old offset, neighbor, and
// edge vectors are unreferenced once it starts. An empty slot (no offsets) owns no
// base pages, so there is nothing to free.
func (a *Adj) freeOldBase(s uint32) error {
	c, err := a.readDir(s)
	if err != nil {
		return err
	}
	if c.offLen == 0 || c.logHead == 0 {
		return nil
	}
	log, err := store.OpenLog(a.p, c.logHead, 0)
	if err != nil {
		return err
	}
	return log.Free()
}

// dirCell is one slot's decoded directory entry: the compressed-blob log's head
// page, the byte lengths of the offsets, neighbors, and edges blobs (in that log
// order), and the logical offset count (nodeCount+1). An empty slot has logHead
// zero and offLen zero.
type dirCell struct {
	logHead                      format.PageID
	offBytes, nbrBytes, edgBytes uint64
	offLen                       uint64
}

func (a *Adj) readDir(s uint32) (dirCell, error) {
	var buf [dirStride]byte
	if err := a.dir.Get(int(s), buf[:]); err != nil {
		return dirCell{}, err
	}
	return dirCell{
		logHead:  format.PageID(format.U64(buf[0:8])),
		offBytes: format.U64(buf[8:16]),
		nbrBytes: format.U64(buf[16:24]),
		edgBytes: format.U64(buf[24:32]),
		offLen:   format.U64(buf[32:40]),
	}, nil
}

func (a *Adj) writeDir(s uint32, c dirCell) error {
	var buf [dirStride]byte
	format.PutU64(buf[0:8], uint64(c.logHead))
	format.PutU64(buf[8:16], c.offBytes)
	format.PutU64(buf[16:24], c.nbrBytes)
	format.PutU64(buf[24:32], c.edgBytes)
	format.PutU64(buf[32:40], c.offLen)
	return a.dir.Set(int(s), buf[:])
}

// Checkpoint folds the delta into the base by rebuilding the CSR from every live
// relationship and advancing the folded count to the current relationship count,
// after which the delta is empty. nodeCount is the number of node positions, so
// every node gets an offset entry even with zero degree. Durable when the
// transaction commits; because the whole rebuild commits in one batch, a crash
// mid-checkpoint recovers either the old base or the new one, never a mix.
func (a *Adj) Checkpoint(nodeCount uint64) error {
	relCount := uint64(a.rels.Count())
	oldSlotCount := a.dir.Count()

	// Group live edges by slot and source, both directions.
	group := map[uint32]map[uint64][]Neighbor{}
	maxSlot := -1
	add := func(s uint32, src uint64, nb Neighbor) {
		m := group[s]
		if m == nil {
			m = map[uint64][]Neighbor{}
			group[s] = m
		}
		m[src] = append(m[src], nb)
		if int(s) > maxSlot {
			maxSlot = int(s)
		}
	}
	for pos := range relCount {
		if !a.rels.Exists(pos) {
			continue
		}
		r, err := a.rels.Get(pos)
		if err != nil {
			return err
		}
		add(slot(r.Type, Out), r.Src, Neighbor{Node: r.Dst, Edge: pos})
		add(slot(r.Type, In), r.Dst, Neighbor{Node: r.Src, Edge: pos})
	}

	// Ensure the directory covers every slot up to the max (dense fill).
	if a.dir.Count() <= maxSlot {
		empty := make([]byte, dirStride)
		for a.dir.Count() <= maxSlot {
			if _, err := a.dir.Append(empty); err != nil {
				return err
			}
		}
	}

	// Free the old base vectors of every slot that had one. The rebuild below
	// builds each slot from the live relationships, not from the old base, so the
	// old pages are unreferenced now; freeing them up front lets the fresh vectors
	// reuse them instead of growing the file (doc 65 §2; doc 64 §6).
	for s := range oldSlotCount {
		if err := a.freeOldBase(uint32(s)); err != nil {
			return err
		}
	}

	// Rebuild each slot's arrays.
	for s := range maxSlot + 1 {
		perSrc := group[uint32(s)]
		if len(perSrc) == 0 {
			// No edges for this slot: write an empty cell.
			if err := a.writeDir(uint32(s), dirCell{}); err != nil {
				return err
			}
			continue
		}
		// Build the three CSR arrays in memory: the offset array (cumulative degree),
		// the neighbor array (each source's run sorted), and the parallel edge array.
		offsets := make([]uint64, nodeCount+1)
		var nbrs, edges []uint64
		var running uint64
		for i := range nodeCount {
			list := perSrc[i]
			slices.SortFunc(list, cmpNeighbor)
			for _, nb := range list {
				nbrs = append(nbrs, nb.Node)
				edges = append(edges, nb.Edge)
			}
			running += uint64(len(list))
			offsets[i+1] = running
		}
		cell, err := a.writeSlotBlobs(offsets, nbrs, edges)
		if err != nil {
			return err
		}
		if err := a.writeDir(uint32(s), cell); err != nil {
			return err
		}
	}

	a.folded = relCount
	if err := a.secs.Set(store.SecAdjDir, a.dir.Head(), uint64(a.dir.Count())); err != nil {
		return err
	}
	if err := a.secs.Set(store.SecAdjMeta, 0, relCount); err != nil {
		return err
	}
	a.delta = map[uint32]map[uint64][]Neighbor{}
	a.cache = map[uint32]*base{}
	// The rebuilt base offsets now reflect every live edge, so the tail
	// adjustment starts fresh.
	a.degTail = map[uint32]map[uint64]int64{}
	// The base just changed, so the degree distribution did too: recompute it from
	// the fresh offset arrays while we are already on the write path.
	return a.recomputeDegStats()
}

// recomputeDegStats rebuilds the per-slot degree distribution from the base
// offset arrays. It runs at open and after each checkpoint, the two points the
// base is freshly consistent, and reads only each slot's offset blob, never its
// neighbor or edge arrays, so it stays cheap even for a graph full of supernodes.
func (a *Adj) recomputeDegStats() error {
	stats := make(map[uint32]DegreeSummary, a.dir.Count())
	for s := 0; s < a.dir.Count(); s++ {
		off, err := a.slotOffsets(uint32(s))
		if err != nil {
			return err
		}
		if len(off) < 2 {
			continue
		}
		degs := make([]uint64, 0, len(off)-1)
		var sum, mx uint64
		for i := 1; i < len(off); i++ {
			d := off[i] - off[i-1]
			if d == 0 {
				continue // a node with no edge in this slot does not participate
			}
			degs = append(degs, d)
			sum += d
			if d > mx {
				mx = d
			}
		}
		if len(degs) == 0 {
			continue
		}
		slices.Sort(degs)
		stats[uint32(s)] = DegreeSummary{
			Nodes: uint64(len(degs)),
			Edges: sum,
			Max:   mx,
			P50:   percentile(degs, 50),
			P99:   percentile(degs, 99),
		}
	}
	a.degStats = stats
	return nil
}

// slotOffsets decodes just a slot's offset array, the first of its three packed
// blobs, leaving the neighbor and edge blobs on disk. An empty slot has no base
// and returns nil.
func (a *Adj) slotOffsets(s uint32) ([]uint64, error) {
	c, err := a.readDir(s)
	if err != nil {
		return nil, err
	}
	if c.offLen == 0 {
		return nil, nil
	}
	log, err := store.OpenLog(a.p, c.logHead, int(c.offBytes))
	if err != nil {
		return nil, err
	}
	blob := make([]byte, c.offBytes)
	if err := log.Read(0, int(c.offBytes), blob); err != nil {
		return nil, err
	}
	return decodeArray(blob)
}

// percentile returns the nearest-rank pth percentile of a sorted, non-empty
// degree slice: the value at rank ceil(p*n/100), one-based and clamped into the
// slice. Nearest-rank needs no interpolation, so it returns an actual observed
// degree, which reads naturally as a metric ("the 99th-percentile node has this
// many edges").
func percentile(sorted []uint64, p int) uint64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p*n/100)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// DegreeStats returns a copy of the per-slot degree summaries computed at the
// last open or checkpoint, keyed by slot (relType*2 + direction). It is read on
// the write/checkpoint path under the engine's exclusive lock, so degStats itself
// needs no lock; the copy lets the caller publish the snapshot without aliasing
// the live map.
func (a *Adj) DegreeStats() map[uint32]DegreeSummary {
	out := make(map[uint32]DegreeSummary, len(a.degStats))
	for k, v := range a.degStats {
		out[k] = v
	}
	return out
}

// SlotType and SlotDir split a slot key back into its relationship type token and
// direction, the inverse of the packing slot() does, so a caller mapping per-slot
// stats onto typed, directed metric labels need not know the encoding.
func SlotType(s uint32) uint32 { return s / 2 }

// SlotDir returns the direction half of a slot key.
func SlotDir(s uint32) Dir { return Dir(s % 2) }

// SlotCount returns the number of (relType, direction) slots in the adjacency directory.
func (a *Adj) SlotCount() int { return a.dir.Count() }

// SlotOffsets returns the CSR offset array for slot s, or nil if the slot is empty.
// The offset array maps each node position to the [start, end) range in the neighbor
// array; it must be non-decreasing. Called by the integrity checker (doc 23 §8.5).
func (a *Adj) SlotOffsets(s uint32) ([]uint64, error) { return a.slotOffsets(s) }

// SlotNeighbors returns the neighbor and edge id arrays for slot s. These parallel
// the offset array: nbrs[offsets[i]..offsets[i+1]] are the destinations of edges
// from node i; edges[...] are the corresponding relationship ids. Used by the
// integrity checker for adjacency-symmetry verification (doc 23 §8.5).
func (a *Adj) SlotNeighbors(s uint32) (nbrs, edges []uint64, err error) {
	c, err := a.readDir(s)
	if err != nil {
		return nil, nil, err
	}
	if c.nbrBytes == 0 {
		return nil, nil, nil
	}
	log, err := store.OpenLog(a.p, c.logHead, int(c.offBytes+c.nbrBytes+c.edgBytes))
	if err != nil {
		return nil, nil, err
	}
	// neighbor array follows the offset array
	nbrBlob := make([]byte, c.nbrBytes)
	if err := log.Read(int(c.offBytes), int(c.nbrBytes), nbrBlob); err != nil {
		return nil, nil, err
	}
	edgeBlob := make([]byte, c.edgBytes)
	if err := log.Read(int(c.offBytes+c.nbrBytes), int(c.edgBytes), edgeBlob); err != nil {
		return nil, nil, err
	}
	nbrs, err = decodeArray(nbrBlob)
	if err != nil {
		return nil, nil, err
	}
	edges, err = decodeArray(edgeBlob)
	return nbrs, edges, err
}

// DeltaLen reports how many (source, slot) neighbor entries are pending in the
// delta. It is zero immediately after a checkpoint.
func (a *Adj) DeltaLen() int {
	n := 0
	for _, m := range a.delta {
		for _, list := range m {
			n += len(list)
		}
	}
	return n
}

// writeSlotBlobs compresses a slot's three CSR arrays and packs them into one
// fresh log, returning the directory cell that locates them. Each array is run
// through the colcodec cascade, which picks the smallest scheme for its shape: the
// monotone offsets fall to delta-FOR, the sorted-per-run neighbors to delta, the
// clustered edge ids to FOR (doc 15 §15.2-§15.4). The blobs are appended in the
// fixed order offsets, neighbors, edges, so the byte lengths in the cell slice
// them apart on read.
func (a *Adj) writeSlotBlobs(offsets, nbrs, edges []uint64) (dirCell, error) {
	offBlob := encodeArray(offsets)
	nbrBlob := encodeArray(nbrs)
	edgBlob := encodeArray(edges)
	log, err := store.CreateLog(a.p, format.PageTypeRelGroup)
	if err != nil {
		return dirCell{}, err
	}
	for _, blob := range [][]byte{offBlob, nbrBlob, edgBlob} {
		if _, err := log.Append(blob); err != nil {
			return dirCell{}, err
		}
	}
	return dirCell{
		logHead:  log.Head(),
		offBytes: uint64(len(offBlob)),
		nbrBytes: uint64(len(nbrBlob)),
		edgBytes: uint64(len(edgBlob)),
		offLen:   uint64(len(offsets)),
	}, nil
}

// BuildAdj creates an empty Adj for writing pre-built CSR arrays via WriteSlot.
// It has no rel.Store and therefore must not call Checkpoint, Expand, or
// rebuildDelta; it is used only by the bulk loader's pass 4 to write the initial
// base CSR from the counting-sort arrays built in pass 3.
func BuildAdj(p *pager.Pager, secs *store.Sections) (*Adj, error) {
	dir, err := store.CreateVector(p, dirStride, format.PageTypeRelGroup)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecAdjDir, dir.Head(), 0); err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecAdjMeta, 0, 0); err != nil {
		return nil, err
	}
	return &Adj{
		p: p, secs: secs, rels: nil, dir: dir,
		delta:    map[uint32]map[uint64][]Neighbor{},
		cache:    map[uint32]*base{},
		degTail:  map[uint32]map[uint64]int64{},
		degStats: map[uint32]DegreeSummary{},
	}, nil
}

// WriteSlot writes pre-built CSR arrays for (relType, direction) directly into
// the adjacency directory. It grows the directory to reach the slot, encodes the
// three arrays, and records the cell. It is called by the bulk loader's pass 4
// to materialize the in-memory CSR from pass 3 without going through the normal
// edge-insert → checkpoint path.
func (a *Adj) WriteSlot(relType uint32, d Dir, offsets, nbrs, edges []uint64) error {
	s := slot(relType, d)
	// Grow the directory to include slot s.
	empty := make([]byte, dirStride)
	for a.dir.Count() <= int(s) {
		if _, err := a.dir.Append(empty); err != nil {
			return err
		}
	}
	cell, err := a.writeSlotBlobs(offsets, nbrs, edges)
	if err != nil {
		return err
	}
	return a.writeDir(s, cell)
}

// encodeArray compresses a CSR array of unsigned positions with the colcodec
// cascade. The uint64-to-int64 cast is bit-preserving and the matching cast in
// decodeArray reverses it, so a dense id with its top bit set still round-trips.
func encodeArray(vals []uint64) []byte {
	ints := make([]int64, len(vals))
	for i, v := range vals {
		ints[i] = int64(v)
	}
	return colcodec.Encode(ints)
}
