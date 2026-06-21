package store

import (
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
)

// A Section names one persistent store the engine owns. The section directory
// maps each Section to the (head, meta) pair needed to reopen that store: head
// is the chain's first page, and meta is the store's logical extent — a Log's
// byte length or a Vector's element count. The numeric values are part of the
// on-disk layout and must never be reused for a different store.
//
// New sections are appended here as later milestones add stores; because the
// directory is allocated at a fixed capacity (MaxSections), adding a section id
// does not change the on-disk size of existing files, so a database written by
// an earlier build reopens unchanged (its new slots simply read back empty).
type Section uint32

const (
	// SecCatalog is the catalog token-dictionary Log.
	SecCatalog Section = iota
	// SecIDMap is the id-map Log (element id <-> dense position).
	SecIDMap
	// SecNodeRec is the node record Vector (per dense node position).
	SecNodeRec
	// SecNodeLabels is the node label-set Log (sorted token lists).
	SecNodeLabels
	// SecRelRec is the relationship record Vector (per dense relationship position).
	SecRelRec
	// SecNodeCols is the node property-column directory (a Vector indexed by
	// property-key token; see package column).
	SecNodeCols
	// SecRelCols is the relationship property-column directory.
	SecRelCols
	// SecAdjDir is the CSR adjacency base directory (a Vector indexed by
	// type-and-direction slot; see package adj).
	SecAdjDir
	// SecAdjMeta carries the adjacency's folded relationship count (the number of
	// relationship positions already folded into the base CSR) in its meta field.
	SecAdjMeta
	// SecStatsLabel is the per-label node-count Vector (a count per label token;
	// see package stats).
	SecStatsLabel
	// SecStatsRel is the per-type relationship-count Vector (a count per
	// relationship-type token; see package stats).
	SecStatsRel
	// SecNodeColSeg is the node segmented-column store directory (the compressed
	// base columns; see package colsegstore). Its meta field holds the directory's
	// segment-key count. Empty until the first checkpoint folds the naive columns
	// into segments.
	SecNodeColSeg
	// SecRelColSeg is the relationship segmented-column store directory.
	SecRelColSeg
	// numSectionsInUse is the count of section ids currently defined. It must
	// stay <= MaxSections; later milestones add ids before this marker.
	numSectionsInUse
)

// MaxSections is the fixed capacity of the section directory. It is generous so
// future milestones can add stores without changing the on-disk geometry.
const MaxSections = 64

// Compile-time assertion that the defined section ids fit the fixed capacity;
// this underflows (and fails to compile) if numSectionsInUse ever exceeds it.
const _ = uint(MaxSections - numSectionsInUse)

// sectionStride is the size of one directory record: head (8) + meta (8).
const sectionStride = 16

// Sections is the section directory: a fixed-capacity vector of (head, meta)
// records anchored at the header's SectionDir. Because its element count is the
// compile-time constant MaxSections, it reopens without needing its length
// recorded anywhere else — it is the root anchor every other store hangs from.
type Sections struct {
	v *Vector
}

// CreateSections allocates an empty section directory (all slots empty) and
// records its root in the header. Durable at the next commit.
func CreateSections(p *pager.Pager) (*Sections, error) {
	v, err := CreateVector(p, sectionStride, format.PageTypeSectDir)
	if err != nil {
		return nil, err
	}
	zero := make([]byte, sectionStride)
	for range MaxSections {
		if _, err := v.Append(zero); err != nil {
			return nil, err
		}
	}
	p.SetSectionDir(v.Head())
	return &Sections{v: v}, nil
}

// OpenSections reopens the section directory anchored at the header's SectionDir.
func OpenSections(p *pager.Pager) (*Sections, error) {
	v, err := OpenVector(p, p.SectionDir(), sectionStride, MaxSections)
	if err != nil {
		return nil, err
	}
	return &Sections{v: v}, nil
}

// Get returns the head page and meta value recorded for a section. A zero head
// (NoPage) means the section has not been created yet.
func (s *Sections) Get(sec Section) (format.PageID, uint64, error) {
	buf := make([]byte, sectionStride)
	if err := s.v.Get(int(sec), buf); err != nil {
		return 0, 0, err
	}
	return format.PageID(format.U64(buf[0:8])), format.U64(buf[8:16]), nil
}

// Set records the head page and meta value for a section. Durable at the next
// commit.
func (s *Sections) Set(sec Section, head format.PageID, meta uint64) error {
	buf := make([]byte, sectionStride)
	format.PutU64(buf[0:8], uint64(head))
	format.PutU64(buf[8:16], meta)
	return s.v.Set(int(sec), buf)
}
