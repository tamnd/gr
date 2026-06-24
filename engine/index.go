package engine

import (
	"slices"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/idmap"
	"github.com/tamnd/gr/mvcc"
	"github.com/tamnd/gr/value"
)

// indexDefKey identifies a declared single-property index by its target: the
// catalog label token and the catalog property-key token it indexes.
type indexDefKey struct {
	label uint32
	prop  uint32
}

// propIndexSet is the engine's in-memory set of equality property indexes. Each
// declared index maps a value key (uniqueKey, the type-tagged canonical text) to
// the dense positions of the live label-nodes holding that value, so a lookup by
// value is a map probe instead of a column scan (doc 07 §4).
//
// The set reflects the latest committed base state: it is rebuilt from the base
// stores whenever the committed state changes (CreateIndex, a data write commit,
// and rollback during Abort). This whole-rebuild maintenance is the
// correctness-first form (doc 07 §9); incremental, durable, B+-tree-backed index
// maintenance is the M4 refinement. A snapshot-correct lookup reconciles this
// base index against the overlay and the writer's own pending writes (see
// diskTx.IndexSeek), so the base-only structure is enough to be correct.
type propIndexSet struct {
	defs map[indexDefKey]map[string][]uint64
}

// autoIndexName is the generated name of an unnamed index, derived from its label
// and property so a DROP INDEX can still address it and a repeat CREATE collides
// with itself (doc 07 §4). The prefix keeps it from colliding with a generated
// constraint name on the same label and property.
func autoIndexName(label, prop string) string {
	return "index_" + label + "_" + prop
}

// rebuildIndexes rebuilds every declared property index from the committed base
// stores. It runs under the engine lock (its callers hold it) and replaces the
// whole in-memory set, so an index always reflects the latest committed state. A
// database with no declared indexes gets an empty set and pays nothing.
func (e *DiskEngine) rebuildIndexes() error {
	set := &propIndexSet{defs: make(map[indexDefKey]map[string][]uint64)}
	defs := e.cat.Indexes()
	for _, d := range defs {
		// Only single-property indexes are supported in this release; a composite
		// definition is recorded in the catalog but not yet served.
		if len(d.Props) != 1 {
			continue
		}
		set.defs[indexDefKey{label: d.Label, prop: d.Props[0]}] = make(map[string][]uint64)
	}
	e.idx = set
	if len(set.defs) == 0 {
		e.publishIndexStats()
		return nil
	}
	n := uint64(e.nodes.Count())
	for pos := uint64(0); pos < n; pos++ {
		if !e.nodes.Exists(pos) {
			continue
		}
		cats, err := e.nodes.Labels(pos)
		if err != nil {
			return err
		}
		for dk, bucket := range set.defs {
			if !slices.Contains(cats, dk.label) {
				continue
			}
			v, ok, err := e.baseNodeProp(dk.prop, pos)
			if err != nil {
				return err
			}
			if !ok || v.IsNull() {
				continue
			}
			k := uniqueKey(v)
			bucket[k] = append(bucket[k], pos)
		}
	}
	e.publishIndexStats()
	return nil
}

// indexEntryOverheadBytes is the per-value-bucket constant in the footprint estimate: a Go string
// header (a pointer and a length) plus a slice header (a pointer, a length, and a capacity), the
// fixed cost each distinct indexed value carries on top of its key bytes and its position list. It is
// an estimate of the dominant terms, not an exact heap measurement, which is all an operator needs to
// compare indexes and watch one grow (doc 20 §6.4).
const indexEntryOverheadBytes = 16 + 24

// publishIndexStats snapshots the per-index entry count into idxCounts and the per-index in-memory
// footprint estimate into idxBytes, each as a fresh map (doc 20 §6.4), so the gr_index_entries and
// gr_index_memory_bytes gauges read a live value without taking the engine lock. The caller holds the
// engine lock (every rebuildIndexes caller does, and Open is exclusive), so reading the catalog and
// the index buckets here is safe. The count is the total indexed positions across the index's value
// buckets; the byte estimate adds, per distinct value, its key bytes, the fixed header overhead, and
// eight bytes per indexed position. An index with no served buckets reports zero for both. Each map is
// swapped in atomically, so a concurrent reader sees either the old map or the new one, never a
// half-built one.
func (e *DiskEngine) publishIndexStats() {
	counts := make(map[string]uint64, len(e.idx.defs))
	bytes := make(map[string]uint64, len(e.idx.defs))
	for _, ix := range e.cat.Indexes() {
		if len(ix.Props) != 1 {
			counts[ix.Name] = 0
			bytes[ix.Name] = 0
			continue
		}
		var n, b uint64
		if bucket, ok := e.idx.defs[indexDefKey{label: ix.Label, prop: ix.Props[0]}]; ok {
			for k, positions := range bucket {
				n += uint64(len(positions))
				b += uint64(len(k)) + indexEntryOverheadBytes + uint64(len(positions))*8
			}
		}
		counts[ix.Name] = n
		bytes[ix.Name] = b
	}
	e.idxCounts.Store(&counts)
	e.idxBytes.Store(&bytes)
}

// hasIndexes reports whether any property index is declared, so the write commit
// path can skip the rebuild when there is nothing to maintain.
func (e *DiskEngine) hasIndexes() bool {
	return e.idx != nil && len(e.idx.defs) > 0
}

// IndexEntryCount returns the number of indexed positions a named single-property index holds in
// the committed base (doc 20 §6.4): the count of live label-nodes carrying a non-null value for the
// indexed property. It reads the count map last published under the engine lock, so it takes no lock
// itself and is safe to call from the metrics snapshot even while a write transaction holds the
// engine lock. An unknown name (including a dropped index) returns zero, so a gone index's gauge
// reads zero rather than going stale.
func (e *DiskEngine) IndexEntryCount(name string) uint64 {
	if m := e.idxCounts.Load(); m != nil {
		return (*m)[name]
	}
	return 0
}

// IndexMemoryBytes returns the estimated in-memory footprint of a named single-property index (doc
// 20 §6.4): per distinct indexed value, its key bytes plus a fixed header overhead plus eight bytes
// per indexed position. Like IndexEntryCount it reads the map last published under the engine lock, so
// it takes no lock and an unknown name returns zero. The value is an estimate of the dominant terms,
// enough to compare indexes and watch one grow, not an exact heap measurement.
func (e *DiskEngine) IndexMemoryBytes(name string) uint64 {
	if m := e.idxBytes.Load(); m != nil {
		return (*m)[name]
	}
	return 0
}

// IndexNames returns the names of every declared index, read lock-free from the published count map
// (doc 20 §6.4). The metrics layer uses it to decide which gr_index_entries gauges to register, so
// like IndexEntryCount it must not take the engine lock: a snapshot can run while a write
// transaction holds it.
func (e *DiskEngine) IndexNames() []string {
	m := e.idxCounts.Load()
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(*m))
	for name := range *m {
		out = append(out, name)
	}
	return out
}

// HasNodeIndex reports whether a single-property node index is declared on the
// given label and property, in SPI token space (doc 07 §4). The planner calls it
// to decide whether a node scan with an equality filter has an index access path
// available; a zero label or property is never indexed. The answer is part of the
// catalog state the plan cache keys on (CatalogVersion bumps on an index add or
// drop), so a plan compiled from this answer is invalidated when the answer
// changes.
func (e *DiskEngine) HasNodeIndex(label, prop Token) bool {
	if label == 0 || prop == 0 {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.idx.defs[indexDefKey{label: toCat(label), prop: toCat(prop)}]
	return ok
}

// CreateIndex declares a node property index on label.prop in its own write
// transaction (doc 07 §4, doc 08 §6.1). It interns the label and property names,
// records the index durably, commits, and builds the index over the existing data.
// It returns whether an index was added: false (with no error) when the index
// already exists and ifNotExists is set, true otherwise. Unlike a constraint an
// index has no invariant, so there is no existing-data validation: any data is a
// valid index population.
func (e *DiskEngine) CreateIndex(name, label, prop string, ifNotExists bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.p.BeginWrite()
	defer e.p.EndWrite()
	if name == "" {
		name = autoIndexName(label, prop)
	}
	if _, exists := e.cat.IndexByName(name); exists {
		if ifNotExists {
			return false, nil
		}
		return false, catalog.ErrIndexExists
	}
	lt, _, err := e.cat.Intern(catalog.KindLabel, label)
	if err != nil {
		return false, err
	}
	pt, _, err := e.cat.Intern(catalog.KindPropKey, prop)
	if err != nil {
		return false, err
	}
	if err := e.cat.AddIndex(catalog.Index{Name: name, Label: lt, Props: []uint32{pt}}); err != nil {
		return false, err
	}
	if _, err := e.commitPager(); err != nil {
		return false, err
	}
	if err := e.rebuildIndexes(); err != nil {
		return false, err
	}
	return true, nil
}

// DropIndex removes an index by name in its own write transaction. It returns
// whether an index was removed: false (no error) when none exists and ifExists is
// set, true otherwise; a plain drop of an absent index errors.
func (e *DiskEngine) DropIndex(name string, ifExists bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.p.BeginWrite()
	defer e.p.EndWrite()
	if _, ok := e.cat.IndexByName(name); !ok {
		if ifExists {
			return false, nil
		}
		return false, catalog.ErrNoSuchIndex
	}
	if err := e.cat.DropIndex(name); err != nil {
		return false, err
	}
	if _, err := e.commitPager(); err != nil {
		return false, err
	}
	if err := e.rebuildIndexes(); err != nil {
		return false, err
	}
	return true, nil
}

// IndexSeek yields the snapshot-visible nodes carrying label and holding value v
// for property key, served by a declared property index (doc 07 §4). It reconciles
// the base index against this snapshot so the result is correct even though the
// index reflects the latest committed state, not this reader's snapshot:
//
//   - the base index gives the positions holding v in the committed base;
//   - the overlay names every position whose existence, labels, or this property
//     changed after the read sequence, which a base index can therefore mislist;
//   - a write transaction additionally reads its own uncommitted writes, which the
//     base index (rebuilt only at commit) does not yet reflect, so its pending
//     pre-images name the positions it has touched.
//
// Every candidate from those three sources is then filtered against its actual
// snapshot state (live, carries the label, value equals v), so over-listed
// candidates are dropped and the yielded set is exactly the snapshot-correct one.
func (t *diskTx) IndexSeek(label, key Token, v value.Value, fn func(NodeID) error) (bool, error) {
	t.rlock()
	defer t.runlock()
	catLabel, catProp := toCat(label), toCat(key)
	bucket, ok := t.e.idx.defs[indexDefKey{label: catLabel, prop: catProp}]
	if !ok {
		return false, nil
	}
	if v.IsNull() {
		return true, nil
	}
	want := uniqueKey(v)
	seen := make(map[uint64]struct{})
	emit := func(pos uint64) error {
		if _, dup := seen[pos]; dup {
			return nil
		}
		seen[pos] = struct{}{}
		if !t.nodeLive(pos) {
			return nil
		}
		cats, err := t.snapLabels(pos)
		if err != nil {
			return err
		}
		if !slices.Contains(cats, catLabel) {
			return nil
		}
		sv, present, err := t.snapNodeProp(pos, catProp)
		if err != nil {
			return err
		}
		if !present || sv.IsNull() || uniqueKey(sv) != want {
			return nil
		}
		eid, ok := t.e.ids.Eid(idmap.KindNode, pos)
		if !ok {
			return nil
		}
		return fn(NodeID(eid))
	}
	for _, pos := range bucket[want] {
		if err := emit(pos); err != nil {
			return true, err
		}
	}
	for _, pos := range t.e.ov.NodeCandidates(catProp, t.readSeq) {
		if err := emit(pos); err != nil {
			return true, err
		}
	}
	if t.write {
		for _, pp := range t.pending {
			if pp.key.Kind == mvcc.NodeExist || pp.key.Kind == mvcc.NodeLabels ||
				(pp.key.Kind == mvcc.NodeProp && pp.key.Sub == catProp) {
				if err := emit(pp.key.Pos); err != nil {
					return true, err
				}
			}
		}
	}
	return true, nil
}
