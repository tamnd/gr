package loader

import "fmt"

// IDMapEntry is the loader's record for one loaded node: which label group it
// landed in and its dense id within that group (doc 19 §7.4).
//
// This is the *loader's* in-flight id map (input external id → dense id). It is
// a separate structure from the engine's runtime idmap ([idmap.Map]), which maps
// element ids → dense positions. The loader derives the runtime element ids from
// (kind, group, dense) during finalization; the loader's map is only live for
// the duration of the load.
type IDMapEntry struct {
	Group  LabelGroup
	DenseID uint64
}

// idSpaceMap is one id-space's in-memory hash map: external string id → entry.
// Using a plain Go map is correct for the in-RAM case; the spill form (for maps
// that exceed the memory budget) is a later extension (doc 19 §9.4).
type idSpaceMap map[string]IDMapEntry

// IDMapBuilder is the loader's mutable external-id → (group, dense) map
// (doc 19 §7.4). It is keyed on (id-space, external-id), so ids that are unique
// only within a type are disambiguated by their space (doc 19 §7.1).
//
// The global space (space == "") is the fallback when no id space is declared.
// Using a single global space is correct when all external ids are globally
// unique; using named spaces is the recommended practice for multi-type graphs.
type IDMapBuilder struct {
	spaces map[string]idSpaceMap // space name → (externalID → entry)
}

// newIDMapBuilder returns an empty IDMapBuilder.
func newIDMapBuilder() *IDMapBuilder {
	return &IDMapBuilder{spaces: make(map[string]idSpaceMap)}
}

// Has reports whether (space, id) is already in the map.
func (m *IDMapBuilder) Has(space, id string) bool {
	sm, ok := m.spaces[space]
	if !ok {
		return false
	}
	_, ok = sm[id]
	return ok
}

// Put records (space, id) → entry. It panics if the entry already exists;
// callers must check Has first and handle duplicates themselves.
func (m *IDMapBuilder) Put(space, id string, entry IDMapEntry) {
	sm, ok := m.spaces[space]
	if !ok {
		sm = make(idSpaceMap)
		m.spaces[space] = sm
	}
	sm[id] = entry
}

// Get returns the entry for (space, id) and whether it exists.
func (m *IDMapBuilder) Get(space, id string) (IDMapEntry, bool) {
	sm, ok := m.spaces[space]
	if !ok {
		return IDMapEntry{}, false
	}
	e, ok := sm[id]
	return e, ok
}

// Len returns the total number of entries across all spaces.
func (m *IDMapBuilder) Len() int {
	n := 0
	for _, sm := range m.spaces {
		n += len(sm)
	}
	return n
}

// errDuplicateID is the error for a duplicate external id under the Fail policy.
func errDuplicateID(space, id string, lineno int) error {
	if space == "" {
		return fmt.Errorf("loader: duplicate node id %q at line %d", id, lineno)
	}
	return fmt.Errorf("loader: duplicate node id %q in space %q at line %d", id, space, lineno)
}

// errMissingID is the error for a node row whose :ID field is empty.
func errMissingID(lineno int) error {
	return fmt.Errorf("loader: missing :ID at line %d", lineno)
}
