// Package rel is gr's relationship record store (spec 2060 doc 04 §5). A
// relationship is a dense position within the relationship kind; this store
// holds, per position, the relationship's existence, its type token, and its
// source and target node positions. A relationship's properties are columnar,
// indexed by the same dense position (doc 04 §6), so the record carries no
// property pointer.
//
// These records are the source of truth for "which edges exist and what they
// connect"; the CSR adjacency index (a later PR) is built from them to make the
// expand primitive O(1)+O(degree). The store itself is a single fixed-stride
// Vector over the pager, so it inherits the substrate's durability and recovers
// with the durable-prefix property.
package rel

import (
	"errors"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
)

// recordStride is the size of one relationship record: flags(1) + type(u32 at 4)
// + src(u64 at 8) + dst(u64 at 16). Bytes 1..3 are reserved padding.
const recordStride = 24

const flagLive = 0x01

// ErrNoSuchRel is returned for a position that was never created or is deleted.
var ErrNoSuchRel = errors.New("gr/rel: no such relationship")

// Rel is a decoded relationship record.
type Rel struct {
	Type uint32
	Src  uint64
	Dst  uint64
}

// Store is the relationship record store.
type Store struct {
	p    *pager.Pager
	secs *store.Sections
	recs *store.Vector
}

// Create initializes a fresh relationship store, allocating its record Vector
// and recording it in the section directory.
func Create(p *pager.Pager, secs *store.Sections) (*Store, error) {
	recs, err := store.CreateVector(p, recordStride, format.PageTypeRel)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecRelRec, recs.Head(), uint64(recs.Count())); err != nil {
		return nil, err
	}
	return &Store{p: p, secs: secs, recs: recs}, nil
}

// Open reopens the relationship store from the section directory.
func Open(p *pager.Pager, secs *store.Sections) (*Store, error) {
	head, count, err := secs.Get(store.SecRelRec)
	if err != nil {
		return nil, err
	}
	recs, err := store.OpenVector(p, head, recordStride, int(count))
	if err != nil {
		return nil, err
	}
	return &Store{p: p, secs: secs, recs: recs}, nil
}

// Count returns the number of relationship positions ever allocated.
func (s *Store) Count() int { return s.recs.Count() }

// Create appends a new relationship of the given type between src and dst node
// positions and returns its dense position.
func (s *Store) Create(relType uint32, src, dst uint64) (uint64, error) {
	var rec [recordStride]byte
	rec[0] = flagLive
	format.PutU32(rec[4:], relType)
	format.PutU64(rec[8:], src)
	format.PutU64(rec[16:], dst)
	idx, err := s.recs.Append(rec[:])
	if err != nil {
		return 0, err
	}
	if err := s.secs.Set(store.SecRelRec, s.recs.Head(), uint64(s.recs.Count())); err != nil {
		return 0, err
	}
	return uint64(idx), nil
}

// Exists reports whether the relationship at pos is live.
func (s *Store) Exists(pos uint64) bool {
	rec, err := s.readRec(pos)
	if err != nil {
		return false
	}
	return rec[0]&flagLive != 0
}

// Delete marks the relationship at pos deleted. Its position is not reclaimed in M1.
func (s *Store) Delete(pos uint64) error {
	rec, err := s.readRec(pos)
	if err != nil {
		return err
	}
	if rec[0]&flagLive == 0 {
		return ErrNoSuchRel
	}
	rec[0] &^= flagLive
	return s.recs.Set(int(pos), rec[:])
}

// Get returns the decoded relationship at pos, or ErrNoSuchRel if deleted.
func (s *Store) Get(pos uint64) (Rel, error) {
	rec, err := s.readRec(pos)
	if err != nil {
		return Rel{}, err
	}
	if rec[0]&flagLive == 0 {
		return Rel{}, ErrNoSuchRel
	}
	return Rel{
		Type: format.U32(rec[4:]),
		Src:  format.U64(rec[8:]),
		Dst:  format.U64(rec[16:]),
	}, nil
}

func (s *Store) readRec(pos uint64) ([recordStride]byte, error) {
	var rec [recordStride]byte
	if pos >= uint64(s.recs.Count()) {
		return rec, ErrNoSuchRel
	}
	if err := s.recs.Get(int(pos), rec[:]); err != nil {
		return rec, err
	}
	return rec, nil
}
