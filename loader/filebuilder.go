package loader

import (
	"fmt"

	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// fileBuilder manages the output .gr pager and the property column stores and
// CSR arrays built during passes 2–4 (doc 19 §4.2–4.5). It is created once the
// catalog and id-map are final (after pass 1) and closed in finalization.
//
// Node property columns and the CSR use GLOBAL node positions (not per-group
// local positions). Global position = groupBase[group] + localDenseID.
//
// groupBase[g] is the starting global position of label group g:
//
//	groupBase[0] = 0
//	groupBase[g] = groupBase[g-1] + count[g-1]
//	totalNodes   = groupBase[len(groups)]
type fileBuilder struct {
	p    *pager.Pager
	fsys vfs.VFS // non-nil for mem pagers (tests); nil means OS VFS
	path string

	// groupBase[g] is the global node-position offset for label group g.
	// len(groupBase) == groups+1; groupBase[groups] == totalNodes.
	groupBase []uint64
	// totalNodes is groupBase[last].
	totalNodes uint64

	// nodeStore is the single global node property column store.
	// Columns are addressed by (propertyKeyToken, globalNodePos).
	nodeStore *colsegstore.Store

	// keyBuilders holds one colBuilder per unique property-key token. All groups
	// share the same builder for a key so the column is contiguous across group
	// boundaries. Created on first use in pass 2.
	keyBuilders map[uint32]*colBuilder

	// groupBuilders[g] is the ordered colBuilder slice for label group g,
	// parallel to the propColDescs returned by propColDescs. Created on first use
	// in pass 2; entries are pointers into keyBuilders (may be shared across groups).
	groupBuilders [][](*colBuilder)

	// relCSR holds the in-memory CSR arrays built during pass 3.
	// Keyed by csrKey{relType, dir}. The offset arrays have size totalNodes+1;
	// neighbor values are global node positions. Written to adj pages in pass 4.
	relCSR map[csrKey]*csrBuilder

	// edgeCnt tracks the next dense edge id per relationship type.
	edgeCnt map[uint32]uint64
}

// openFileBuilder creates the output .gr file, computes groupBase, and
// initializes the global node property column store. It is called after pass 1
// so the group sizes are final.
func openFileBuilder(fsys vfs.VFS, path string, cat *CatalogBuilder, maxPoolPages int) (*fileBuilder, error) {
	if fsys == nil {
		fsys = vfs.NewOS()
	}
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, MaxPoolPages: maxPoolPages})
	if err != nil {
		return nil, fmt.Errorf("loader: open %s: %w", path, err)
	}

	// Compute groupBase offsets from group sizes.
	groups := cat.Groups()
	groupBase := make([]uint64, groups+1)
	for g := range groups {
		groupBase[g+1] = groupBase[g] + cat.GroupCount(LabelGroup(g))
	}
	totalNodes := groupBase[groups]

	// Create the single global node property column store.
	nodeStore, err := colsegstore.CreateStore(p)
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("loader: create node column store: %w", err)
	}

	return &fileBuilder{
		p:             p,
		fsys:          fsys,
		path:          path,
		groupBase:     groupBase,
		totalNodes:    totalNodes,
		nodeStore:     nodeStore,
		keyBuilders:   make(map[uint32]*colBuilder),
		groupBuilders: make([][](*colBuilder), groups),
		relCSR:        make(map[csrKey]*csrBuilder),
		edgeCnt:       make(map[uint32]uint64),
	}, nil
}

// GlobalPos converts a (group, localDenseID) pair to a global node position.
func (fb *fileBuilder) GlobalPos(g LabelGroup, localID uint64) uint64 {
	return fb.groupBase[int(g)] + localID
}

// ensureBuilders returns the column builders for group g, creating them on
// first use. propCols describes the property columns from the source's header.
// Each builder targets fb.nodeStore at global positions starting at groupBase[g].
func (fb *fileBuilder) ensureBuilders(g LabelGroup, propCols []propColDesc) []*colBuilder {
	gi := int(g)
	if gi >= len(fb.groupBuilders) {
		for len(fb.groupBuilders) <= gi {
			fb.groupBuilders = append(fb.groupBuilders, nil)
		}
	}
	if fb.groupBuilders[gi] != nil {
		return fb.groupBuilders[gi]
	}
	bs := make([]*colBuilder, len(propCols))
	for i, pc := range propCols {
		key := uint32(pc.keyToken)
		b, ok := fb.keyBuilders[key]
		if !ok {
			// First time we see this key. Start at position 0; the colBuilder
			// gap-fills absent cells up to the first actual globalPos when Append
			// is called. This satisfies the column store's contiguity requirement
			// (first segment must start at 0) and naturally marks group-foreign
			// positions as absent.
			b = newColBuilder(fb.nodeStore, key, pc.vtype, 0)
			fb.keyBuilders[key] = b
		}
		bs[i] = b
	}
	fb.groupBuilders[gi] = bs
	return bs
}

// propColDesc describes one property column's storage parameters.
type propColDesc struct {
	colIdx   int        // header column index
	keyToken int        // catalog token for the property key
	vtype    value.Type // storage plane
	pt       PropType   // for parsing
	isList   bool
}

// ensureCSR returns the CSRBuilder for (relType, dir), creating it with
// totalNodes positions on first call. The offset array covers all nodes globally,
// not just the type's source group.
func (fb *fileBuilder) ensureCSR(relType uint32, dir csrDir) *csrBuilder {
	k := csrKey{relType, dir}
	if b, ok := fb.relCSR[k]; ok {
		return b
	}
	b := newCSRBuilder(fb.totalNodes)
	fb.relCSR[k] = b
	return b
}

// nextEdgeID returns the next dense edge id for the given relationship type.
func (fb *fileBuilder) nextEdgeID(relType uint32) uint64 {
	id := fb.edgeCnt[relType]
	fb.edgeCnt[relType]++
	return id
}

// Close commits any buffered pages and closes the pager.
func (fb *fileBuilder) Close() error {
	return fb.p.Close()
}
