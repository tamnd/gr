// Package node is gr's node record store (spec 2060 doc 04 §4). A node is a
// dense position within the node kind; this store holds, per position, the
// node's existence and its label set. A node's properties are not here: they are
// columnar, indexed by the same dense position (doc 04 §6), and live in the
// column store (a later PR), so a node record carries no property pointer.
//
// The store is two pieces from the store layer:
//
//   - a fixed-stride Vector of node records, indexed by position; each record is
//     a flags byte plus a (offset, length) reference into the label-set Log.
//   - a label-set Log holding each node's labels as a sorted list of catalog
//     tokens. A label set is small and read whole, so it is stored as one packed
//     record the node points at; changing a node's labels appends a new list and
//     repoints the record (the superseded bytes are reclaimed by compaction in a
//     later PR, doc 04 §9).
//
// Both ride the pager, so the node store inherits the substrate's durability and
// recovers with the durable-prefix property. Labels are interned through the
// catalog before they reach this store; the store deals only in tokens.
package node

import (
	"errors"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
)

// recordStride is the size of one node record: flags(1) + labelsLen(u16 at 2) +
// labelsOff(u64 at 8). Bytes 1 and 4..7 are reserved padding.
const recordStride = 16

const flagLive = 0x01

// ErrNoSuchNode is returned for a position that was never created or is deleted.
var ErrNoSuchNode = errors.New("gr/node: no such node")

// Store is the node record store.
type Store struct {
	p      *pager.Pager
	secs   *store.Sections
	recs   *store.Vector
	labels *store.Log
}

// Create initializes a fresh node store, allocating its record Vector and
// label-set Log and recording both in the section directory.
func Create(p *pager.Pager, secs *store.Sections) (*Store, error) {
	recs, err := store.CreateVector(p, recordStride, format.PageTypeNode)
	if err != nil {
		return nil, err
	}
	labels, err := store.CreateLog(p, format.PageTypeNode)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecNodeRec, recs.Head(), uint64(recs.Count())); err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecNodeLabels, labels.Head(), 0); err != nil {
		return nil, err
	}
	return &Store{p: p, secs: secs, recs: recs, labels: labels}, nil
}

// Open reopens the node store from the section directory.
func Open(p *pager.Pager, secs *store.Sections) (*Store, error) {
	rHead, rCount, err := secs.Get(store.SecNodeRec)
	if err != nil {
		return nil, err
	}
	recs, err := store.OpenVector(p, rHead, recordStride, int(rCount))
	if err != nil {
		return nil, err
	}
	lHead, lLen, err := secs.Get(store.SecNodeLabels)
	if err != nil {
		return nil, err
	}
	labels, err := store.OpenLog(p, lHead, int(lLen))
	if err != nil {
		return nil, err
	}
	return &Store{p: p, secs: secs, recs: recs, labels: labels}, nil
}

// Count returns the number of node positions ever allocated (live or deleted).
func (s *Store) Count() int { return s.recs.Count() }

// Create appends a new node with the given sorted label tokens and returns its
// dense position. Callers pass labels already interned and sorted.
func (s *Store) Create(labels []uint32) (uint64, error) {
	off, length, err := s.writeLabels(labels)
	if err != nil {
		return 0, err
	}
	var rec [recordStride]byte
	rec[0] = flagLive
	format.PutU16(rec[2:], uint16(length))
	format.PutU64(rec[8:], uint64(off))
	idx, err := s.recs.Append(rec[:])
	if err != nil {
		return 0, err
	}
	if err := s.secs.Set(store.SecNodeRec, s.recs.Head(), uint64(s.recs.Count())); err != nil {
		return 0, err
	}
	return uint64(idx), nil
}

// Exists reports whether the node at pos is live.
func (s *Store) Exists(pos uint64) bool {
	rec, err := s.readRec(pos)
	if err != nil {
		return false
	}
	return rec[0]&flagLive != 0
}

// Delete marks the node at pos deleted. Its position is not reclaimed in M1.
func (s *Store) Delete(pos uint64) error {
	rec, err := s.readRec(pos)
	if err != nil {
		return err
	}
	if rec[0]&flagLive == 0 {
		return ErrNoSuchNode
	}
	rec[0] &^= flagLive
	return s.recs.Set(int(pos), rec[:])
}

// Labels returns the node's label tokens (sorted), or ErrNoSuchNode if deleted.
func (s *Store) Labels(pos uint64) ([]uint32, error) {
	rec, err := s.readRec(pos)
	if err != nil {
		return nil, err
	}
	if rec[0]&flagLive == 0 {
		return nil, ErrNoSuchNode
	}
	return s.decodeRecLabels(rec[:])
}

// LabelsRaw returns the node's label tokens ignoring the live flag. A snapshot
// reader that still sees a node another transaction has since deleted resolves
// the retained label bytes through this, since the slot is kept until GC and any
// label change before the delete would have left its own pre-image in the
// overlay (consulted first by the caller).
func (s *Store) LabelsRaw(pos uint64) ([]uint32, error) {
	rec, err := s.readRec(pos)
	if err != nil {
		return nil, err
	}
	return s.decodeRecLabels(rec[:])
}

// decodeRecLabels reads the label bytes a node record points at and decodes them.
func (s *Store) decodeRecLabels(rec []byte) ([]uint32, error) {
	length := int(format.U16(rec[2:]))
	if length == 0 {
		return nil, nil
	}
	off := int(format.U64(rec[8:]))
	buf := make([]byte, length)
	if err := s.labels.Read(off, length, buf); err != nil {
		return nil, err
	}
	return decodeLabels(buf)
}

// HasLabel reports whether the node at pos carries the given label token.
func (s *Store) HasLabel(pos uint64, t uint32) (bool, error) {
	labels, err := s.Labels(pos)
	if err != nil {
		return false, err
	}
	for _, l := range labels {
		if l == t {
			return true, nil
		}
	}
	return false, nil
}

// SetLabels replaces the node's label set with the given sorted tokens, writing
// a fresh label-set record and repointing the node record.
func (s *Store) SetLabels(pos uint64, labels []uint32) error {
	rec, err := s.readRec(pos)
	if err != nil {
		return err
	}
	if rec[0]&flagLive == 0 {
		return ErrNoSuchNode
	}
	off, length, err := s.writeLabels(labels)
	if err != nil {
		return err
	}
	format.PutU16(rec[2:], uint16(length))
	format.PutU64(rec[8:], uint64(off))
	return s.recs.Set(int(pos), rec[:])
}

func (s *Store) readRec(pos uint64) ([recordStride]byte, error) {
	var rec [recordStride]byte
	if pos >= uint64(s.recs.Count()) {
		return rec, ErrNoSuchNode
	}
	if err := s.recs.Get(int(pos), rec[:]); err != nil {
		return rec, err
	}
	return rec, nil
}

// writeLabels appends a packed sorted token list to the label Log and returns
// its (offset, byte length). An empty set writes nothing.
func (s *Store) writeLabels(labels []uint32) (off, length int, err error) {
	if len(labels) == 0 {
		return 0, 0, nil
	}
	buf := format.AppendUvarint(nil, uint64(len(labels)))
	for _, t := range labels {
		buf = format.AppendUvarint(buf, uint64(t))
	}
	off, err = s.labels.Append(buf)
	if err != nil {
		return 0, 0, err
	}
	if err := s.secs.Set(store.SecNodeLabels, s.labels.Head(), uint64(s.labels.Len())); err != nil {
		return 0, 0, err
	}
	return off, len(buf), nil
}

func decodeLabels(buf []byte) ([]uint32, error) {
	n, k, err := format.Uvarint(buf)
	if err != nil {
		return nil, err
	}
	buf = buf[k:]
	out := make([]uint32, 0, n)
	for range n {
		v, k, err := format.Uvarint(buf)
		if err != nil {
			return nil, err
		}
		buf = buf[k:]
		out = append(out, uint32(v))
	}
	return out, nil
}
