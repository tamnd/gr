// Package column is gr's columnar property store (spec 2060 doc 04 §6, doc 25 §4
// deliverable 5). A property is reached not through a pointer on the element
// record but by the element's dense position: a node at position i finds the
// value for property key k at position i of column k. Each property key gets its
// own column, so a scan that reads one property of many elements touches only
// that column's pages and never the others — the columnar-read property M1's
// gate calls for.
//
// A column has two parts: an index Vector keyed by element position, each cell a
// fixed (present-flag, length, offset) triple, and a values Log holding the
// encoded values packed densely. An absent property is a cleared present flag;
// it is distinct from a property explicitly set to null (a stored null value).
//
// Columns are discovered through a directory: a Vector indexed by property-key
// token, each cell recording one column's persistent extents. Property keys are
// dense monotonic catalog tokens, so the directory grows with the key space; a
// key not yet used on this element kind gets an empty column. The directory's
// element count lives in the section directory and is kept current per commit,
// so a reopen finds every column's exact committed extent with no replay and no
// zero-tail ambiguity.
//
// Two instances exist per database, one for node properties and one for
// relationship properties, distinguished only by their directory section.
package column

import (
	"sync"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
)

// dirStride is one directory cell: a column's index Vector head and element
// count, then its values Log head and byte length.
const dirStride = 32

// idxStride is one index cell: a flags byte (bit 0 = present, bit 1 = tombstone),
// then the value's length and offset into the values Log. Bytes 1..3 are reserved
// padding.
const idxStride = 16

const (
	flagPresent   = 0x01
	flagTombstone = 0x02
)

// Presence is the three-valued result of reading this store as a delta layer over
// a base (see [Columns.GetDelta]): a value is present, a tombstone marks it
// deleted, or the position carries no entry so a layered read falls through to the
// base.
type Presence uint8

const (
	// Missing means no entry in this layer: a position with no index cell, a key
	// with no column, or a cleared cell with neither flag set (a gap fill from a
	// later write growing the index). A layered read falls through to the base.
	Missing Presence = iota
	// Present means a value is stored at this position.
	Present
	// Deleted means a tombstone: the read resolves absent and does not fall
	// through, so a delta removal hides a value in the base.
	Deleted
)

// Columns is one columnar property store (node or relationship), anchored at its
// directory section. Open columns are cached; an unopened column is materialized
// on first touch from its directory cell.
type Columns struct {
	p      *pager.Pager
	secs   *store.Sections
	dirSec store.Section
	dir    *store.Vector
	// colsMu guards the cols cache against concurrent readers. A read of a
	// property (Get, GetDelta) opens the key's column on a cache miss and installs
	// it here, so morsel-parallel readers sharing one snapshot race on the map
	// without a lock. The write path (Set, Remove, Free) runs under the engine's
	// exclusive lock so it never races a reader, but it reaches the cache only
	// through openColumn, which takes the lock itself, so writers are covered too.
	colsMu sync.Mutex
	cols   map[uint32]*column
}

// column is one property key's data: the per-position index and the value bytes.
type column struct {
	idx  *store.Vector
	vals *store.Log
}

// Create initializes a fresh columnar store with an empty directory and records
// it in the section directory (durable at the next commit). dirSec selects which
// element kind this store serves (store.SecNodeCols or store.SecRelCols).
func Create(p *pager.Pager, secs *store.Sections, dirSec store.Section) (*Columns, error) {
	dir, err := store.CreateVector(p, dirStride, format.PageTypeColumn)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(dirSec, dir.Head(), 0); err != nil {
		return nil, err
	}
	return &Columns{p: p, secs: secs, dirSec: dirSec, dir: dir, cols: map[uint32]*column{}}, nil
}

// Open reopens a columnar store from the section directory. Individual columns
// are opened lazily on first access.
func Open(p *pager.Pager, secs *store.Sections, dirSec store.Section) (*Columns, error) {
	head, count, err := secs.Get(dirSec)
	if err != nil {
		return nil, err
	}
	dir, err := store.OpenVector(p, head, dirStride, int(count))
	if err != nil {
		return nil, err
	}
	return &Columns{p: p, secs: secs, dirSec: dirSec, dir: dir, cols: map[uint32]*column{}}, nil
}

// Free returns every page this store occupies to the pager's free list: each
// column's index Vector and values Log, and the directory Vector. The store is
// dead afterward and must not be used again; the checkpoint calls this on the
// naive columns it has folded into the segmented base so their pages can be
// reused. Columns are opened lazily, so this materializes each one to reach its
// chains.
func (c *Columns) Free() error {
	for k := range uint32(c.dir.Count()) {
		col, err := c.ensureColumn(k)
		if err != nil {
			return err
		}
		if err := col.idx.Free(); err != nil {
			return err
		}
		if err := col.vals.Free(); err != nil {
			return err
		}
	}
	return c.dir.Free()
}

// readDir decodes the directory cell for a key token.
func (c *Columns) readDir(key uint32) (idxHead format.PageID, idxCount uint64, valsHead format.PageID, valsLen uint64, err error) {
	var buf [dirStride]byte
	if err = c.dir.Get(int(key), buf[:]); err != nil {
		return
	}
	idxHead = format.PageID(format.U64(buf[0:8]))
	idxCount = format.U64(buf[8:16])
	valsHead = format.PageID(format.U64(buf[16:24]))
	valsLen = format.U64(buf[24:32])
	return
}

// writeDir records a column's current extents into its directory cell.
func (c *Columns) writeDir(key uint32, col *column) error {
	var buf [dirStride]byte
	format.PutU64(buf[0:8], uint64(col.idx.Head()))
	format.PutU64(buf[8:16], uint64(col.idx.Count()))
	format.PutU64(buf[16:24], uint64(col.vals.Head()))
	format.PutU64(buf[24:32], uint64(col.vals.Len()))
	return c.dir.Set(int(key), buf[:])
}

// openColumn materializes the column for an already-registered key from its
// directory cell.
func (c *Columns) openColumn(key uint32) (*column, error) {
	idxHead, idxCount, valsHead, valsLen, err := c.readDir(key)
	if err != nil {
		return nil, err
	}
	idx, err := store.OpenVector(c.p, idxHead, idxStride, int(idxCount))
	if err != nil {
		return nil, err
	}
	vals, err := store.OpenLog(c.p, valsHead, int(valsLen))
	if err != nil {
		return nil, err
	}
	col := &column{idx: idx, vals: vals}
	// Drop the lock across the vector opens above (they do their own IO through
	// the safe pager), then install under the lock with a double-check so a peer
	// that opened the same column first wins and this caller drops its duplicate.
	c.colsMu.Lock()
	defer c.colsMu.Unlock()
	if existing, ok := c.cols[key]; ok {
		return existing, nil
	}
	c.cols[key] = col
	return col, nil
}

// ensureColumn returns the column for key, creating empty columns for every key
// token up to and including it if the directory does not yet reach that far.
// Property-key tokens are dense, so this usually creates just the one column.
func (c *Columns) ensureColumn(key uint32) (*column, error) {
	c.colsMu.Lock()
	col, ok := c.cols[key]
	c.colsMu.Unlock()
	if ok {
		return col, nil
	}
	if int(key) < c.dir.Count() {
		return c.openColumn(key)
	}
	for k := c.dir.Count(); k <= int(key); k++ {
		idx, err := store.CreateVector(c.p, idxStride, format.PageTypeColumn)
		if err != nil {
			return nil, err
		}
		vals, err := store.CreateLog(c.p, format.PageTypeColumn)
		if err != nil {
			return nil, err
		}
		var buf [dirStride]byte
		format.PutU64(buf[0:8], uint64(idx.Head()))
		format.PutU64(buf[8:16], 0)
		format.PutU64(buf[16:24], uint64(vals.Head()))
		format.PutU64(buf[24:32], 0)
		if _, err := c.dir.Append(buf[:]); err != nil {
			return nil, err
		}
	}
	if err := c.secs.Set(c.dirSec, c.dir.Head(), uint64(c.dir.Count())); err != nil {
		return nil, err
	}
	return c.openColumn(key)
}

// Set stores value v for property key at element position pos, overwriting any
// previous value. The old value bytes are left as garbage in the values Log,
// reclaimed by compaction in a later PR. Durable when the transaction commits.
func (c *Columns) Set(key uint32, pos uint64, v value.Value) error {
	col, err := c.ensureColumn(key)
	if err != nil {
		return err
	}
	enc := format.AppendValue(nil, v)
	off, err := col.vals.Append(enc)
	if err != nil {
		return err
	}
	// Grow the index to cover pos, filling skipped positions as absent.
	empty := make([]byte, idxStride)
	for col.idx.Count() <= int(pos) {
		if _, err := col.idx.Append(empty); err != nil {
			return err
		}
	}
	var cell [idxStride]byte
	cell[0] = flagPresent
	format.PutU32(cell[4:8], uint32(len(enc)))
	format.PutU64(cell[8:16], uint64(off))
	if err := col.idx.Set(int(pos), cell[:]); err != nil {
		return err
	}
	return c.writeDir(key, col)
}

// Get returns the value for property key at element position pos. The bool is
// false when the property is absent (never set or removed); a stored null value
// returns (value.Null, true, nil).
func (c *Columns) Get(key uint32, pos uint64) (value.Value, bool, error) {
	if int(key) >= c.dir.Count() {
		return value.Null, false, nil
	}
	col, err := c.ensureColumn(key)
	if err != nil {
		return value.Null, false, err
	}
	if int(pos) >= col.idx.Count() {
		return value.Null, false, nil
	}
	var cell [idxStride]byte
	if err := col.idx.Get(int(pos), cell[:]); err != nil {
		return value.Null, false, err
	}
	if cell[0]&flagPresent == 0 {
		return value.Null, false, nil
	}
	n := format.U32(cell[4:8])
	off := format.U64(cell[8:16])
	buf := make([]byte, n)
	if err := col.vals.Read(int(off), int(n), buf); err != nil {
		return value.Null, false, err
	}
	v, _, err := format.DecodeValue(buf)
	if err != nil {
		return value.Null, false, err
	}
	return v, true, nil
}

// Remove clears the property key at element position pos. It is a no-op if the
// property is already absent or the column does not exist.
func (c *Columns) Remove(key uint32, pos uint64) error {
	if int(key) >= c.dir.Count() {
		return nil
	}
	col, err := c.ensureColumn(key)
	if err != nil {
		return err
	}
	if int(pos) >= col.idx.Count() {
		return nil
	}
	var cell [idxStride]byte
	if err := col.idx.Get(int(pos), cell[:]); err != nil {
		return err
	}
	if cell[0]&flagPresent == 0 {
		return nil
	}
	cell[0] &^= flagPresent
	return col.idx.Set(int(pos), cell[:])
}

// GetDelta reads property key at position pos treating this store as a delta layer
// over a base: it reports whether the position is present (with its value),
// tombstoned, or missing. A missing position has no index cell, a cleared cell
// with neither flag set, or a key with no column, all of which a layered read
// resolves by falling through to the base. A tombstone resolves absent without
// falling through. This is the read the engine uses once the store is a
// post-checkpoint delta over the segmented base (doc 14 §4.7).
func (c *Columns) GetDelta(key uint32, pos uint64) (value.Value, Presence, error) {
	if int(key) >= c.dir.Count() {
		return value.Null, Missing, nil
	}
	col, err := c.ensureColumn(key)
	if err != nil {
		return value.Null, Missing, err
	}
	if int(pos) >= col.idx.Count() {
		return value.Null, Missing, nil
	}
	var cell [idxStride]byte
	if err := col.idx.Get(int(pos), cell[:]); err != nil {
		return value.Null, Missing, err
	}
	switch {
	case cell[0]&flagPresent != 0:
		n := format.U32(cell[4:8])
		off := format.U64(cell[8:16])
		buf := make([]byte, n)
		if err := col.vals.Read(int(off), int(n), buf); err != nil {
			return value.Null, Missing, err
		}
		v, _, err := format.DecodeValue(buf)
		if err != nil {
			return value.Null, Missing, err
		}
		return v, Present, nil
	case cell[0]&flagTombstone != 0:
		return value.Null, Deleted, nil
	default:
		return value.Null, Missing, nil
	}
}

// Tombstone marks property key at position pos as deleted in this delta layer, so
// a layered read resolves it absent without falling through to the base. It grows
// the index to cover pos if needed and clears any present flag there. This is the
// removal form once the store is a delta over a base: a plain [Columns.Remove]
// only clears the present flag, which a base read cannot tell apart from a never
// written position, so it would wrongly expose the base value. Durable when the
// transaction commits.
func (c *Columns) Tombstone(key uint32, pos uint64) error {
	col, err := c.ensureColumn(key)
	if err != nil {
		return err
	}
	empty := make([]byte, idxStride)
	for col.idx.Count() <= int(pos) {
		if _, err := col.idx.Append(empty); err != nil {
			return err
		}
	}
	var cell [idxStride]byte
	cell[0] = flagTombstone
	if err := col.idx.Set(int(pos), cell[:]); err != nil {
		return err
	}
	return c.writeDir(key, col)
}

// Keys returns the property-key tokens that have a column in this store. A key
// in the result may still have no value at a given position.
func (c *Columns) Keys() []uint32 {
	keys := make([]uint32, c.dir.Count())
	for i := range keys {
		keys[i] = uint32(i)
	}
	return keys
}

// All returns every present property at element position pos as a key->value
// map. It probes each column, which suits M1's correctness-first stance;
// label/type-grouped columns for locality are a later optimization.
func (c *Columns) All(pos uint64) (map[uint32]value.Value, error) {
	out := map[uint32]value.Value{}
	for k := range uint32(c.dir.Count()) {
		v, ok, err := c.Get(k, pos)
		if err != nil {
			return nil, err
		}
		if ok {
			out[k] = v
		}
	}
	return out, nil
}
