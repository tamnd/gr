package loader

import (
	"fmt"

	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// fileBuilder manages the output .gr pager and the per-group column stores
// built during passes 2–4 (doc 19 §4.2–4.5). It is created once the catalog
// and id-map are final (after pass 1) and closed in finalization.
type fileBuilder struct {
	p             *pager.Pager
	fsys          vfs.VFS    // non-nil for mem pagers (tests); nil means OS VFS
	path          string
	nstores       []*colsegstore.Store // nstores[g] = node column store for label group g
	groupBuilders [][](*colBuilder)    // groupBuilders[g][i] = colBuilder for prop i of group g
	// relCSR holds the in-memory CSR arrays built during pass 3.
	// Keyed by csrKey{relType, dir}. Written to the pager in pass 4.
	relCSR map[csrKey]*csrBuilder
	// edgeCnt tracks the next dense edge id per relationship type (assigned in scatter order).
	edgeCnt map[uint32]uint64
}

// openFileBuilder creates the output .gr file at path (or in fsys when non-nil)
// and initializes an empty column store for each label group that has nodes.
// groupCount is the number of label groups discovered in pass 1.
func openFileBuilder(fsys vfs.VFS, path string, groupCount int) (*fileBuilder, error) {
	if fsys == nil {
		fsys = vfs.NewOS()
	}
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull})
	if err != nil {
		return nil, fmt.Errorf("loader: open %s: %w", path, err)
	}
	fb := &fileBuilder{
		p:       p,
		fsys:    fsys,
		path:    path,
		nstores: make([]*colsegstore.Store, groupCount),
		relCSR:  make(map[csrKey]*csrBuilder),
		edgeCnt: make(map[uint32]uint64),
	}
	for g := range fb.nstores {
		s, serr := colsegstore.CreateStore(p)
		if serr != nil {
			_ = p.Close()
			return nil, fmt.Errorf("loader: create node store for group %d: %w", g, serr)
		}
		fb.nstores[g] = s
	}
	return fb, nil
}

// nodeStore returns the column store for label group g.
func (fb *fileBuilder) nodeStore(g LabelGroup) *colsegstore.Store {
	return fb.nstores[int(g)]
}

// ensureBuilders returns the column builders for group g, creating them on
// first use. propCols describes the property columns from the source's header;
// when the builders already exist (from a prior file in the same source) the
// existing set is returned unchanged.
func (fb *fileBuilder) ensureBuilders(g LabelGroup, propCols []propColDesc) []*colBuilder {
	gi := int(g)
	if gi >= len(fb.groupBuilders) {
		// Grow the slice to accommodate.
		for len(fb.groupBuilders) <= gi {
			fb.groupBuilders = append(fb.groupBuilders, nil)
		}
	}
	if fb.groupBuilders[gi] != nil {
		return fb.groupBuilders[gi]
	}
	s := fb.nodeStore(g)
	bs := make([]*colBuilder, len(propCols))
	for i, pc := range propCols {
		bs[i] = newColBuilder(s, uint32(pc.keyToken), pc.vtype, 0)
	}
	fb.groupBuilders[gi] = bs
	return bs
}

// propColDesc describes one property column's storage parameters.
type propColDesc struct {
	colIdx   int        // header column index
	keyToken int        // catalog token for the property key
	vtype    value.Type // storage value type
	pt       PropType   // header declared type (for parsing)
	isList   bool
}

// ensureCSR returns the CSRBuilder for (relType, dir), creating it (with the
// given node count) on first call per (relType, dir) pair.
func (fb *fileBuilder) ensureCSR(relType uint32, dir csrDir, nodeCount uint64) *csrBuilder {
	k := csrKey{relType, dir}
	if b, ok := fb.relCSR[k]; ok {
		return b
	}
	b := newCSRBuilder(nodeCount)
	fb.relCSR[k] = b
	return b
}

// nextEdgeID returns the next dense edge id for the given relationship type and
// increments the counter. Edge ids are assigned in scatter-scan order, which
// equals input file order for serial loads.
func (fb *fileBuilder) nextEdgeID(relType uint32) uint64 {
	id := fb.edgeCnt[relType]
	fb.edgeCnt[relType]++
	return id
}

// Close commits any buffered pages and closes the pager.
func (fb *fileBuilder) Close() error {
	return fb.p.Close()
}
