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
	return nil
}

// hasIndexes reports whether any property index is declared, so the write commit
// path can skip the rebuild when there is nothing to maintain.
func (e *DiskEngine) hasIndexes() bool {
	return e.idx != nil && len(e.idx.defs) > 0
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
	defer t.rguard()()
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
