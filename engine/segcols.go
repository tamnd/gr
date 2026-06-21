package engine

import (
	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/column"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
)

// segPositions is how many positions a checkpoint packs into one column segment.
// It is the decode granularity of a point read (a read decodes one covering
// segment) and the re-encode unit of a fold, so it trades the memory of one
// decode against the size of the directory (doc 03 §6.1). The fixed value is a
// starting point the segmentation policy tunes once the read path is wired
// (doc 58 §7).
const segPositions = 1024

// createSegStore makes a fresh empty segmented column store for one element kind
// and records its directory anchors in the given section, so a later open finds
// it. The store stays empty until the first checkpoint folds the naive columns
// into segments.
func createSegStore(p *pager.Pager, secs *store.Sections, sec store.Section) (*colsegstore.Store, error) {
	s, err := colsegstore.CreateStore(p)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(sec, s.DirHead(), uint64(s.DirCount())); err != nil {
		return nil, err
	}
	return s, nil
}

// openSegStore reopens a segmented column store from the directory anchors in its
// section. A zero head means no checkpoint has folded into it yet, so it opens as
// a fresh empty store.
func openSegStore(p *pager.Pager, secs *store.Sections, sec store.Section) (*colsegstore.Store, error) {
	head, count, err := secs.Get(sec)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return createSegStore(p, secs, sec)
	}
	return colsegstore.OpenStore(p, head, int(count))
}

// foldSegmented rebuilds the segmented base for one element kind from the naive
// column store, the way adjacency rebuilds its CSR at checkpoint (doc 59 §7). It
// reads each property column's current logical cells over [0, count) and writes
// them back as fixed-size segments into a fresh store, then points the section at
// the new directory. count is the element record high-water mark, so every live
// position is covered.
//
// This is the first slice of the column-compression wiring: it populates the
// segmented base but does not change the read path, which still answers from the
// naive columns, so a checkpoint produces a correct segmented base that nothing
// reads yet (doc 59 §7, slice one). Building a fresh store and repointing the
// section leaves the old store's pages to later reclamation, the same trade the
// adjacency checkpoint makes.
func (e *DiskEngine) foldSegmented(naive *column.Columns, count uint64, sec store.Section) (*colsegstore.Store, error) {
	seg, err := colsegstore.CreateStore(e.p)
	if err != nil {
		return nil, err
	}
	for _, key := range naive.Keys() {
		cells, vt, any, err := readNaiveColumn(naive, key, count)
		if err != nil {
			return nil, err
		}
		if !any {
			// No present value anywhere in the column: leave the key with no
			// segmented column, which reads as absent, matching the naive store.
			continue
		}
		for off := uint64(0); off < count; off += segPositions {
			end := min(off+segPositions, count)
			if err := seg.Append(key, off, vt, cells[off:end]); err != nil {
				return nil, err
			}
		}
	}
	if err := e.secs.Set(sec, seg.DirHead(), uint64(seg.DirCount())); err != nil {
		return nil, err
	}
	return seg, nil
}

// readNaiveColumn reads a naive column's logical cells over [0, count) into the
// segment-encoder cell form. It also returns the column's value type (the type of
// its first present value) and whether any value is present at all, so the caller
// can skip an all-absent column and pick the typed encoding plane.
func readNaiveColumn(naive *column.Columns, key uint32, count uint64) ([]colseg.Cell, value.Type, bool, error) {
	cells := make([]colseg.Cell, count)
	vt := value.TypeNull
	any := false
	for pos := range count {
		v, ok, err := naive.Get(key, pos)
		if err != nil {
			return nil, value.TypeNull, false, err
		}
		cells[pos] = colseg.Cell{Present: ok, Value: v}
		if ok {
			any = true
			if vt == value.TypeNull {
				vt = v.Type()
			}
		}
	}
	return cells, vt, any, nil
}
