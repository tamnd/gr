// Package idmap is gr's two-id scheme: the bridge between the dense internal
// positions the storage engine addresses with and the stable external element
// ids the application holds (spec 2060 doc 04 §7, ADR-15).
//
// The engine works in dense positions internally (a node or relationship is a
// position within its kind, which indexes the CSR and column arrays in O(1));
// the application works in element ids that are stable for the life of the
// element and never reused, so a stored reference to a deleted element resolves
// cleanly to "no such element" rather than silently aliasing a different one.
// The id-map maintains both directions: element id -> position (forward) and
// position -> element id (reverse).
//
// An element id encodes its kind in the top bit so a node id can never equal a
// relationship id, and a per-kind monotone sequence in the remaining bits so ids
// are allocated strictly increasing and never reused. Sequences start at 1, so 0
// is never a valid element id and reads as "absent".
//
// Persistence follows the catalog's proven pattern (doc 04 §7.5 notes a B-tree
// is the eventual forward structure; M1 is correctness-first): every allocation
// and deletion appends a tagged tuple to a single durable Log, and on open the
// Log is replayed front to back to rebuild the in-memory maps exactly, so the
// id-map recovers with the substrate's durable-prefix property. The forward map
// is a Go map and the reverse map is a dense per-kind slice (the dense-array
// reverse structure doc 04 §7.5 prescribes), both reconstructed from the Log.
// Dense-position reuse and compaction (doc 04 §7.3, §9) are deferred to the PR
// that introduces the delta store; in M1 a position is allocated once per create
// and not reclaimed on delete.
package idmap

import (
	"errors"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
)

// Kind is the element kind an id belongs to.
type Kind uint8

const (
	// KindNode is a node element.
	KindNode Kind = 0
	// KindRel is a relationship element.
	KindRel Kind = 1
)

const kindBit = uint64(1) << 63

// tombstone is the position value an entry carries when an element is deleted.
const tombstone = ^uint64(0)

// ErrNoSuchElement is returned when an element id is not live in the map.
var ErrNoSuchElement = errors.New("gr/idmap: no such element")

// encode builds an element id from a kind and a per-kind sequence.
func encode(kind Kind, seq uint64) uint64 { return uint64(kind)<<63 | seq }

// KindOf reports the kind encoded in an element id.
func KindOf(eid uint64) Kind { return Kind(eid >> 63) }

// seqOf returns the per-kind sequence encoded in an element id.
func seqOf(eid uint64) uint64 { return eid &^ kindBit }

// Map is the bidirectional id-map backed by a durable Log.
type Map struct {
	p    *pager.Pager
	secs *store.Sections
	log  *store.Log

	fwd  map[uint64]uint64 // element id -> dense position (live entries only)
	rev  [2][]uint64       // per kind: dense position -> element id
	next [2]uint64         // per kind: next sequence to allocate
}

func newMap(p *pager.Pager, secs *store.Sections, log *store.Log) *Map {
	return &Map{
		p:    p,
		secs: secs,
		log:  log,
		fwd:  make(map[uint64]uint64),
		next: [2]uint64{1, 1}, // sequences start at 1
	}
}

// Create initializes a fresh id-map, allocating its backing Log and recording
// its coordinates in the section directory (durable at the next commit).
func Create(p *pager.Pager, secs *store.Sections) (*Map, error) {
	log, err := store.CreateLog(p, format.PageTypeIDMap)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecIDMap, log.Head(), 0); err != nil {
		return nil, err
	}
	return newMap(p, secs, log), nil
}

// Open reopens the id-map from the section directory, replaying its Log to
// rebuild the forward and reverse maps and the per-kind sequence counters.
func Open(p *pager.Pager, secs *store.Sections) (*Map, error) {
	head, length, err := secs.Get(store.SecIDMap)
	if err != nil {
		return nil, err
	}
	log, err := store.OpenLog(p, head, int(length))
	if err != nil {
		return nil, err
	}
	m := newMap(p, secs, log)
	if err := m.replay(); err != nil {
		return nil, err
	}
	return m, nil
}

// replay reads the whole Log front to back, reapplying each allocation and
// deletion in order so the in-memory state matches the committed prefix.
func (m *Map) replay() error {
	buf := make([]byte, m.log.Len())
	if err := m.log.Read(0, len(buf), buf); err != nil {
		return err
	}
	for len(buf) > 0 {
		eid, n, err := format.Uvarint(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
		pos, n, err := format.Uvarint(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
		m.apply(eid, pos)
	}
	return nil
}

// apply reflects one log tuple into the in-memory maps. A tombstone position
// removes the forward entry; any other position installs or restores it.
func (m *Map) apply(eid, pos uint64) {
	kind := KindOf(eid)
	if seq := seqOf(eid); seq >= m.next[kind] {
		m.next[kind] = seq + 1
	}
	if pos == tombstone {
		delete(m.fwd, eid)
		return
	}
	m.fwd[eid] = pos
	rev := m.rev[kind]
	for uint64(len(rev)) <= pos {
		rev = append(rev, 0)
	}
	rev[pos] = eid
	m.rev[kind] = rev
}

// record appends a tuple to the Log and keeps the section directory's recorded
// length current, so a reopen replays exactly this much (durable at commit).
func (m *Map) record(eid, pos uint64) error {
	rec := format.AppendUvarint(nil, eid)
	rec = format.AppendUvarint(rec, pos)
	if _, err := m.log.Append(rec); err != nil {
		return err
	}
	return m.secs.Set(store.SecIDMap, m.log.Head(), uint64(m.log.Len()))
}

// Alloc allocates a new element of the given kind, returning its freshly minted
// element id and its dense position. The element id is monotone and never
// reused; the position is the next dense slot for the kind.
func (m *Map) Alloc(kind Kind) (eid, pos uint64, err error) {
	eid = encode(kind, m.next[kind])
	pos = uint64(len(m.rev[kind]))
	if err := m.record(eid, pos); err != nil {
		return 0, 0, err
	}
	m.apply(eid, pos)
	return eid, pos, nil
}

// Delete marks an element id deleted. Its position is not reclaimed in M1; the
// element id is gone from the forward map and resolves to absence thereafter.
func (m *Map) Delete(eid uint64) error {
	if _, ok := m.fwd[eid]; !ok {
		return ErrNoSuchElement
	}
	if err := m.record(eid, tombstone); err != nil {
		return err
	}
	m.apply(eid, tombstone)
	return nil
}

// Pos resolves an element id to its dense position. The bool is false for an
// unknown or deleted id (the stale-reference safety property, doc 04 §7.4).
func (m *Map) Pos(eid uint64) (uint64, bool) {
	pos, ok := m.fwd[eid]
	return pos, ok
}

// Eid returns the element id at a dense position for a kind. The bool is false
// if the position was never allocated.
func (m *Map) Eid(kind Kind, pos uint64) (uint64, bool) {
	rev := m.rev[kind]
	if pos >= uint64(len(rev)) || rev[pos] == 0 {
		return 0, false
	}
	return rev[pos], true
}

// Live reports the number of live (non-deleted) elements across both kinds.
func (m *Map) Live() int { return len(m.fwd) }

// Allocated reports how many positions have ever been allocated for a kind
// (live or deleted), which is the dense-position high-water mark.
func (m *Map) Allocated(kind Kind) int { return len(m.rev[kind]) }
