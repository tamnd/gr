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
	fb := &fileBuilder{p: p, fsys: fsys, path: path, nstores: make([]*colsegstore.Store, groupCount)}
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

// Close commits any buffered pages and closes the pager.
func (fb *fileBuilder) Close() error {
	return fb.p.Close()
}
