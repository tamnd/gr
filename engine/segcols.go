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

// baseNodeProp reads a node property from the durable base: the naive column as
// the post-checkpoint delta first, then the segmented base for positions the last
// checkpoint folded and drained from the delta (doc 60 §6; doc 14 §4.7). A delta
// tombstone resolves absent without consulting the base, so a removal since the
// checkpoint hides a folded value; a missing delta entry falls through to the
// base. This is the latest-committed-state read, not a snapshot read; the MVCC
// overlay sits above it in the snapshot resolvers.
func (e *DiskEngine) baseNodeProp(key uint32, pos uint64) (value.Value, bool, error) {
	return baseProp(e.ncols, e.nseg, key, pos)
}

// baseRelProp is baseNodeProp for relationship properties.
func (e *DiskEngine) baseRelProp(key uint32, pos uint64) (value.Value, bool, error) {
	return baseProp(e.rcols, e.rseg, key, pos)
}

// baseProp layers a naive delta column over a segmented base: a present delta
// entry wins, a tombstone resolves absent, and a missing entry falls through to
// the base.
func baseProp(delta *column.Columns, base *colsegstore.Store, key uint32, pos uint64) (value.Value, bool, error) {
	v, pres, err := delta.GetDelta(key, pos)
	if err != nil {
		return value.Null, false, err
	}
	switch pres {
	case column.Present:
		return v, true, nil
	case column.Deleted:
		return value.Null, false, nil
	default:
		return base.Get(key, pos)
	}
}

// basePropKeys returns the property-key tokens that may carry a value across the
// delta and the base. Both stores number keys densely from zero, so the union is
// the range up to the larger key count.
func basePropKeys(delta *column.Columns, base *colsegstore.Store) []uint32 {
	n := len(delta.Keys())
	if b := len(base.Keys()); b > n {
		n = b
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = uint32(i)
	}
	return out
}

// foldSegmented rebuilds the segmented base for one element kind by merging the
// naive delta over the current base, the way adjacency rebuilds its CSR at
// checkpoint (doc 59 §7). It reads each property column's current value over
// [0, count) through baseProp, so a delta write overwrites the base and a delta
// tombstone drops the base value, and writes the merged result back as fixed-size
// segments into a fresh store, then points the section at the new directory. count
// is the element record high-water mark, so every live position is covered.
//
// Building a fresh store and repointing the section leaves the old store's pages
// to later reclamation, the same trade the adjacency checkpoint makes. The caller
// drains the naive delta after the merge so the next window starts empty.
func (e *DiskEngine) foldSegmented(delta *column.Columns, base *colsegstore.Store, count uint64, sec store.Section) (*colsegstore.Store, error) {
	seg, err := colsegstore.CreateStore(e.p)
	if err != nil {
		return nil, err
	}
	for _, key := range basePropKeys(delta, base) {
		cells, vt, any, err := readMergedColumn(delta, base, key, count)
		if err != nil {
			return nil, err
		}
		if !any {
			// No present value anywhere in the merged column: leave the key with no
			// segmented column, which reads as absent.
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

// readMergedColumn reads a column's current logical cells over [0, count) as the
// delta layered over the base, into the segment-encoder cell form. It also returns
// the column's value type (the type of its first present value) and whether any
// value is present, so the caller can skip an all-absent column and pick the typed
// encoding plane.
func readMergedColumn(delta *column.Columns, base *colsegstore.Store, key uint32, count uint64) ([]colseg.Cell, value.Type, bool, error) {
	cells := make([]colseg.Cell, count)
	vt := value.TypeNull
	any := false
	for pos := range count {
		v, ok, err := baseProp(delta, base, key, pos)
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
