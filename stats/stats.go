// Package stats is the engine's statistics store: the per-label node counts and
// per-type relationship counts the M4 cost-based planner consumes for cardinality
// estimation (spec 2060 doc 04 §19; doc 08 §3.5, §7.3; doc 25 §4 deliverable 11).
//
// M1 maintains the cheap, incrementally-updatable counts so M4 inherits accurate
// statistics (doc 25 §4.4, "Statistics accuracy"): each is a single durable
// counter per token, bumped on the write path as nodes and relationships are
// created and deleted and as labels are added and removed. The counters live in
// the file (their roots are catalog bookkeeping, doc 08 §3.5) as two Vectors of
// u64 cells indexed by catalog token, so they are updated inside the writing
// transaction and recovered with the rest of the database: a commit makes a
// count durable, an abort rolls it back with the pager, and a reopen reads it
// straight back without a recomputing scan.
//
// Token indexing matches the catalog's zero-based tokens (the engine's one-based
// SPI wildcard offset is applied above this layer). A token not yet recorded
// reads as a zero count; the backing Vector grows on demand to cover a token the
// first time it is bumped.
//
// The richer statistics the planner can also use — degree distributions and
// property histograms — are computed elsewhere: per-node degree is the
// adjacency's offset-array statistic (package adj; doc 04 §12.5, §19.3), and the
// per-segment zone maps are free column statistics (package column; doc 04
// §19.2). This package holds the two catalog-level cardinalities.
package stats

import (
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
)

// cell is the size of one count: a little-endian u64.
const cell = 8

// Stats is the statistics store over one pager: a per-label node-count Vector and
// a per-type relationship-count Vector, both indexed by catalog token.
type Stats struct {
	p      *pager.Pager
	secs   *store.Sections
	labels *store.Vector // u64 node count per label token
	types  *store.Vector // u64 relationship count per type token
}

// Create initializes empty statistics: two zero-length count Vectors, their roots
// recorded in the section directory (durable at the next commit).
func Create(p *pager.Pager, secs *store.Sections) (*Stats, error) {
	labels, err := store.CreateVector(p, cell, format.PageTypeStats)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecStatsLabel, labels.Head(), 0); err != nil {
		return nil, err
	}
	types, err := store.CreateVector(p, cell, format.PageTypeStats)
	if err != nil {
		return nil, err
	}
	if err := secs.Set(store.SecStatsRel, types.Head(), 0); err != nil {
		return nil, err
	}
	return &Stats{p: p, secs: secs, labels: labels, types: types}, nil
}

// Open reopens the statistics from the section directory.
func Open(p *pager.Pager, secs *store.Sections) (*Stats, error) {
	lHead, lCount, err := secs.Get(store.SecStatsLabel)
	if err != nil {
		return nil, err
	}
	labels, err := store.OpenVector(p, lHead, cell, int(lCount))
	if err != nil {
		return nil, err
	}
	tHead, tCount, err := secs.Get(store.SecStatsRel)
	if err != nil {
		return nil, err
	}
	types, err := store.OpenVector(p, tHead, cell, int(tCount))
	if err != nil {
		return nil, err
	}
	return &Stats{p: p, secs: secs, labels: labels, types: types}, nil
}

// AddLabel adjusts the node count for a label token by delta (positive on create
// or label-add, negative on delete or label-remove). The count never drops below
// zero.
func (s *Stats) AddLabel(label uint32, delta int64) error {
	return adjust(s.labels, s.secs, store.SecStatsLabel, label, delta)
}

// AddRelType adjusts the relationship count for a type token by delta.
func (s *Stats) AddRelType(ty uint32, delta int64) error {
	return adjust(s.types, s.secs, store.SecStatsRel, ty, delta)
}

// LabelCount returns the number of live nodes carrying a label.
func (s *Stats) LabelCount(label uint32) (uint64, error) {
	return read(s.labels, label)
}

// RelTypeCount returns the number of live relationships of a type.
func (s *Stats) RelTypeCount(ty uint32) (uint64, error) {
	return read(s.types, ty)
}

// read returns the count at a token index, or zero if the index is beyond the
// Vector's recorded length (a token never bumped).
func read(v *store.Vector, tok uint32) (uint64, error) {
	if int(tok) >= v.Count() {
		return 0, nil
	}
	var buf [cell]byte
	if err := v.Get(int(tok), buf[:]); err != nil {
		return 0, err
	}
	return format.U64(buf[:]), nil
}

// adjust grows the Vector to cover the token if needed, applies delta to its
// count (clamped at zero), and records the possibly-grown length in the section
// directory so a reopen sees every recorded token.
func adjust(v *store.Vector, secs *store.Sections, sec store.Section, tok uint32, delta int64) error {
	var zero [cell]byte
	grew := false
	for v.Count() <= int(tok) {
		if _, err := v.Append(zero[:]); err != nil {
			return err
		}
		grew = true
	}
	var buf [cell]byte
	if err := v.Get(int(tok), buf[:]); err != nil {
		return err
	}
	next := max(int64(format.U64(buf[:]))+delta, 0)
	format.PutU64(buf[:], uint64(next))
	if err := v.Set(int(tok), buf[:]); err != nil {
		return err
	}
	if grew {
		return secs.Set(sec, v.Head(), uint64(v.Count()))
	}
	return nil
}
