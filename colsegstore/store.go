package colsegstore

import (
	"sync"

	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
)

// Store is the segmented columnar store for one element kind (node or
// relationship): many segmented columns reached by property-key token, the
// segmented counterpart of the naive [column.Columns]. A directory Vector holds
// one cell per key token recording that column's anchors, and a column is
// materialized lazily on first touch (doc 04 §6).
//
// A column's pages are allocated only when it first receives a segment, so a key
// token that has a directory cell but has never been written carries a zero cell
// and costs no pages. A real backing store never has page 0 as its head (page 0
// is the file header), so a zero head is a safe "no column yet" sentinel.
//
// Like [Column], the store is anchored by its directory's head and count, which
// the owner persists; the per-column anchors live inside the directory cells, so
// the whole store reopens from that one pair.
type Store struct {
	p   *pager.Pager
	dir *store.Vector
	// colsMu guards the cols cache against concurrent readers. A property read
	// (Get, Locate, DecodeSegment) opens the key's column on a cache miss and
	// installs it here, so morsel-parallel readers sharing one snapshot race on the
	// map without a lock. Only the read leaf column() takes it; ensureColumn's own
	// install runs write-path-only under the engine's exclusive lock where no reader
	// is present, so it needs no lock, the same split as the adjacency cache.
	colsMu sync.Mutex
	cols   map[uint32]*Column
}

// colCellStride is one directory cell: a column's directory head and blob head
// (both u64), then its directory count and blob length (both u32). An all-zero
// cell means the key has no column yet.
const colCellStride = 24

// CreateStore allocates a fresh empty segmented store: an empty key directory and
// no columns. The owner records the directory anchors and commits to persist it.
func CreateStore(p *pager.Pager) (*Store, error) {
	dir, err := store.CreateVector(p, colCellStride, format.PageTypeColumn)
	if err != nil {
		return nil, err
	}
	return &Store{p: p, dir: dir, cols: map[uint32]*Column{}}, nil
}

// OpenStore reopens a segmented store from its directory anchors. Individual
// columns are opened lazily on first access from their directory cells.
func OpenStore(p *pager.Pager, dirHead format.PageID, dirCount int) (*Store, error) {
	dir, err := store.OpenVector(p, dirHead, colCellStride, dirCount)
	if err != nil {
		return nil, err
	}
	return &Store{p: p, dir: dir, cols: map[uint32]*Column{}}, nil
}

// DirHead and DirCount anchor the key directory; the owner persists both to
// reopen the store.
func (s *Store) DirHead() format.PageID { return s.dir.Head() }
func (s *Store) DirCount() int          { return s.dir.Count() }

// Free returns every page the store occupies to the pager's free list: each
// column's pages and the key directory Vector. The store is dead afterward and
// must not be used again; the checkpoint calls this on the segmented base it has
// rebuilt into a fresh one so the old pages can be reused. A key with a zero cell
// has no column and costs nothing to skip.
func (s *Store) Free() error {
	for _, key := range s.Keys() {
		col, ok, err := s.column(key)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := col.Free(); err != nil {
			return err
		}
	}
	return s.dir.Free()
}

// readCell decodes the directory cell for a key token.
func (s *Store) readCell(key uint32) (dirHead format.PageID, blobHead format.PageID, dirCount int, blobLen int, err error) {
	var buf [colCellStride]byte
	if err = s.dir.Get(int(key), buf[:]); err != nil {
		return
	}
	dirHead = format.PageID(format.U64(buf[0:8]))
	blobHead = format.PageID(format.U64(buf[8:16]))
	dirCount = int(format.U32(buf[16:20]))
	blobLen = int(format.U32(buf[20:24]))
	return
}

// writeCell records a column's current anchors into its directory cell.
func (s *Store) writeCell(key uint32, col *Column) error {
	var buf [colCellStride]byte
	format.PutU64(buf[0:8], uint64(col.DirHead()))
	format.PutU64(buf[8:16], uint64(col.BlobHead()))
	format.PutU32(buf[16:20], uint32(col.DirCount()))
	format.PutU32(buf[20:24], uint32(col.BlobLen()))
	return s.dir.Set(int(key), buf[:])
}

// growDir extends the key directory with zero cells up to and including key, so a
// later write can address its cell. Property-key tokens are dense, so this usually
// adds just the one cell.
func (s *Store) growDir(key uint32) error {
	empty := make([]byte, colCellStride)
	for s.dir.Count() <= int(key) {
		if _, err := s.dir.Append(empty); err != nil {
			return err
		}
	}
	return nil
}

// column opens the cached or stored column for a key, or returns ok false when the
// key has no column yet (out of directory range or a zero cell).
func (s *Store) column(key uint32) (*Column, bool, error) {
	s.colsMu.Lock()
	col, ok := s.cols[key]
	s.colsMu.Unlock()
	if ok {
		return col, true, nil
	}
	if int(key) >= s.dir.Count() {
		return nil, false, nil
	}
	dirHead, blobHead, dirCount, blobLen, err := s.readCell(key)
	if err != nil {
		return nil, false, err
	}
	if dirHead == 0 {
		return nil, false, nil
	}
	col, err = Open(s.p, dirHead, dirCount, blobHead, blobLen)
	if err != nil {
		return nil, false, err
	}
	// Drop the lock across Open above (it does its own IO through the safe pager),
	// then install under the lock with a double-check so a peer that opened the
	// same column first wins and this caller drops its duplicate.
	s.colsMu.Lock()
	defer s.colsMu.Unlock()
	if existing, ok := s.cols[key]; ok {
		return existing, true, nil
	}
	s.cols[key] = col
	return col, true, nil
}

// ensureColumn returns the column for a key, creating and recording it on first
// use. It grows the directory to reach the key first.
func (s *Store) ensureColumn(key uint32) (*Column, error) {
	col, ok, err := s.column(key)
	if err != nil {
		return nil, err
	}
	if ok {
		return col, nil
	}
	if err := s.growDir(key); err != nil {
		return nil, err
	}
	col, err = Create(s.p)
	if err != nil {
		return nil, err
	}
	s.cols[key] = col
	if err := s.writeCell(key, col); err != nil {
		return nil, err
	}
	return col, nil
}

// Append adds a segment to the column for key, creating the column on first use
// and updating its directory cell. The contiguity invariant is per column (see
// [Column.Append]).
func (s *Store) Append(key uint32, firstPos uint64, valueType value.Type, cells []colseg.Cell) error {
	col, err := s.ensureColumn(key)
	if err != nil {
		return err
	}
	if err := col.Append(firstPos, valueType, cells); err != nil {
		return err
	}
	return s.writeCell(key, col)
}

// Get returns the value for key at position pos. A key with no column, or a
// position past that column's coverage or marked absent, returns (Null, false).
func (s *Store) Get(key uint32, pos uint64) (value.Value, bool, error) {
	col, ok, err := s.column(key)
	if err != nil || !ok {
		return value.Null, false, err
	}
	return col.Get(pos)
}

// Locate returns the ordinal of the segment covering pos in key's column and that
// segment's first position, or ok false when the key has no column or pos is past
// its coverage. It decodes no blob, so the engine can front the read with a
// decoded-segment cache keyed by the ordinal (doc 14 §4).
func (s *Store) Locate(key uint32, pos uint64) (ord int, firstPos uint64, ok bool, err error) {
	col, ok, err := s.column(key)
	if err != nil || !ok {
		return 0, 0, false, err
	}
	return col.Locate(pos)
}

// DecodeSegment decodes the whole segment at ordinal ord of key's column into its
// cells. The engine calls it only on a cache miss, then caches the returned cells. A
// key with no column returns nil cells.
func (s *Store) DecodeSegment(key uint32, ord int) ([]colseg.Cell, error) {
	col, ok, err := s.column(key)
	if err != nil || !ok {
		return nil, err
	}
	return col.DecodeSegment(ord)
}

// Keys returns the key tokens that have a directory cell. A key in the result may
// still carry a zero cell (no column yet); Get on it reads as absent.
func (s *Store) Keys() []uint32 {
	keys := make([]uint32, s.dir.Count())
	for i := range keys {
		keys[i] = uint32(i)
	}
	return keys
}
