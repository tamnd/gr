// Package colsegstore is gr's durable segmented column: a sequence of
// COLUMN_SEGMENT blobs ([colseg]) over the pager, with a directory mapping each
// segment to the position range it covers (doc 03 §6.1, doc 04 §6). It is the
// durable home the checkpoint and compaction path fills when it re-encodes a
// column through the codec library, and the read path consults to find and decode
// the segment covering a position (doc 25 §4 deliverable 5, doc 15).
//
// A column here is one property key's data for one element kind. Its segments
// partition the column's positions into contiguous, non-overlapping, ordered
// ranges: segment k covers [first_pos, first_pos + element_count). A read finds
// the covering segment by a binary search over the directory, decodes it, and
// reads the cell at the within-segment offset (doc 03 §6.1).
//
// Like the other stores ([store.Log], [store.Vector]), a column is anchored by
// the heads and lengths of its two backing stores, which the owner persists in
// the section directory or a parent directory cell; Create returns a fresh empty
// column and Open reopens one from those anchors. Nothing is durable until the
// owner commits the pager.
//
// This is the segmented form M1's naive single-index-plus-value-log column
// ([column]) grows into. It lands and is proven against the pager on its own; the
// engine checkpoint that fills it and the read path that consults it (with a
// decoded-segment cache, [blockcache]) are later slices.
package colsegstore

import (
	"errors"
	"sort"

	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
)

// ErrUnordered means an Append would break the directory's invariant that
// segments are contiguous, ordered, and non-overlapping: a segment must start
// exactly where the previous one ended.
var ErrUnordered = errors.New("gr/colsegstore: segment out of order")

// dirStride is one directory record: the segment's first position and the blob
// offset (both u64), then the element count and blob length (both u32).
const dirStride = 24

// Column is one property key's durable segmented data: a directory Vector of
// segment records and a blob Log holding the encoded segments back to back.
type Column struct {
	p    *pager.Pager
	dir  *store.Vector
	blob *store.Log
}

// seg is one decoded directory record.
type seg struct {
	firstPos uint64
	blobOff  uint64
	elemCnt  uint32
	blobLen  uint32
}

// Create allocates a fresh empty segmented column: an empty directory and an
// empty blob log. The owner records the returned anchors and commits to make it
// durable.
func Create(p *pager.Pager) (*Column, error) {
	dir, err := store.CreateVector(p, dirStride, format.PageTypeColumn)
	if err != nil {
		return nil, err
	}
	blob, err := store.CreateLog(p, format.PageTypeColumn)
	if err != nil {
		return nil, err
	}
	return &Column{p: p, dir: dir, blob: blob}, nil
}

// Open reopens a segmented column from its anchors: the directory's head and
// segment count, and the blob log's head and byte length.
func Open(p *pager.Pager, dirHead format.PageID, dirCount int, blobHead format.PageID, blobLen int) (*Column, error) {
	dir, err := store.OpenVector(p, dirHead, dirStride, dirCount)
	if err != nil {
		return nil, err
	}
	blob, err := store.OpenLog(p, blobHead, blobLen)
	if err != nil {
		return nil, err
	}
	return &Column{p: p, dir: dir, blob: blob}, nil
}

// DirHead and DirCount anchor the segment directory; BlobHead and BlobLen anchor
// the blob log. The owner persists all four to reopen the column.
func (c *Column) DirHead() format.PageID  { return c.dir.Head() }
func (c *Column) DirCount() int           { return c.dir.Count() }
func (c *Column) BlobHead() format.PageID { return c.blob.Head() }
func (c *Column) BlobLen() int            { return c.blob.Len() }

// SegmentCount is the number of segments in the column.
func (c *Column) SegmentCount() int { return c.dir.Count() }

// Append adds a segment covering positions [firstPos, firstPos+len(cells)),
// encoding the cells through colseg. Segments must be appended in position order
// with no gap or overlap: the first segment must start at 0 and each later one
// exactly where the previous ended, so the covered range stays contiguous and a
// binary search over first positions is exact. An empty segment is rejected,
// since it would cover no position and break the contiguity check.
func (c *Column) Append(firstPos uint64, valueType value.Type, cells []colseg.Cell) error {
	if len(cells) == 0 {
		return ErrUnordered
	}
	if err := c.checkContiguous(firstPos); err != nil {
		return err
	}
	blob, err := colseg.Encode(valueType, cells)
	if err != nil {
		return err
	}
	off, err := c.blob.Append(blob)
	if err != nil {
		return err
	}
	var rec [dirStride]byte
	format.PutU64(rec[0:8], firstPos)
	format.PutU64(rec[8:16], uint64(off))
	format.PutU32(rec[16:20], uint32(len(cells)))
	format.PutU32(rec[20:24], uint32(len(blob)))
	_, err = c.dir.Append(rec[:])
	return err
}

// checkContiguous verifies a new segment at firstPos abuts the existing coverage:
// the first segment starts at 0, a later one starts where the last ended.
func (c *Column) checkContiguous(firstPos uint64) error {
	n := c.dir.Count()
	if n == 0 {
		if firstPos != 0 {
			return ErrUnordered
		}
		return nil
	}
	last, err := c.readSeg(n - 1)
	if err != nil {
		return err
	}
	if firstPos != last.firstPos+uint64(last.elemCnt) {
		return ErrUnordered
	}
	return nil
}

// Count is the number of positions the column covers: the end of the last
// segment, or zero when the column is empty.
func (c *Column) Count() (uint64, error) {
	n := c.dir.Count()
	if n == 0 {
		return 0, nil
	}
	last, err := c.readSeg(n - 1)
	if err != nil {
		return 0, err
	}
	return last.firstPos + uint64(last.elemCnt), nil
}

// Get returns the value at position pos and whether it is present. A position past
// the covered range, or one in a segment but marked absent, returns (Null, false).
// The whole covering segment is decoded; a decoded-segment cache that avoids
// re-decoding on a repeated point read is the engine's to add when it wires this
// in (doc 14 §4).
func (c *Column) Get(pos uint64) (value.Value, bool, error) {
	s, ok, err := c.find(pos)
	if err != nil || !ok {
		return value.Null, false, err
	}
	buf := make([]byte, s.blobLen)
	if err := c.blob.Read(int(s.blobOff), int(s.blobLen), buf); err != nil {
		return value.Null, false, err
	}
	_, cells, err := colseg.Decode(buf)
	if err != nil {
		return value.Null, false, err
	}
	i := pos - s.firstPos
	if i >= uint64(len(cells)) {
		return value.Null, false, nil
	}
	cell := cells[i]
	if !cell.Present {
		return value.Null, false, nil
	}
	return cell.Value, true, nil
}

// find returns the segment covering pos, or ok false when pos is past the covered
// range. It binary-searches the directory for the last segment whose first
// position is at or below pos, then checks pos falls within that segment.
func (c *Column) find(pos uint64) (seg, bool, error) {
	n := c.dir.Count()
	if n == 0 {
		return seg{}, false, nil
	}
	// sort.Search finds the first segment whose first position is > pos; the one
	// before it is the candidate covering segment.
	var searchErr error
	idx := sort.Search(n, func(i int) bool {
		if searchErr != nil {
			return true
		}
		s, err := c.readSeg(i)
		if err != nil {
			searchErr = err
			return true
		}
		return s.firstPos > pos
	})
	if searchErr != nil {
		return seg{}, false, searchErr
	}
	if idx == 0 {
		return seg{}, false, nil // pos is before the first segment's start
	}
	s, err := c.readSeg(idx - 1)
	if err != nil {
		return seg{}, false, err
	}
	if pos >= s.firstPos+uint64(s.elemCnt) {
		return seg{}, false, nil // pos is past this segment and there is no next one covering it
	}
	return s, true, nil
}

// readSeg decodes the directory record at index i.
func (c *Column) readSeg(i int) (seg, error) {
	var rec [dirStride]byte
	if err := c.dir.Get(i, rec[:]); err != nil {
		return seg{}, err
	}
	return seg{
		firstPos: format.U64(rec[0:8]),
		blobOff:  format.U64(rec[8:16]),
		elemCnt:  format.U32(rec[16:20]),
		blobLen:  format.U32(rec[20:24]),
	}, nil
}
