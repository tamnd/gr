package store

import (
	"fmt"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
)

// Vector is a growable fixed-stride array over a page chain. Element i lives at a
// deterministic (page, offset) computed from a fixed number of elements per
// page, so random access is O(1) given the in-memory page directory (rebuilt by
// walking the chain on open). Records never straddle a page boundary, which
// keeps addressing simple and makes a single-element write dirty exactly one
// page (bounded write amplification, doc 04 §6).
type Vector struct {
	p      *pager.Pager
	stride int
	epp    int // elements per page
	pages  []format.PageID
	count  int
}

// CreateVector creates an empty vector with the given record stride and the
// page type its pages carry, allocating its first (empty) page.
func CreateVector(p *pager.Pager, stride int, t format.PageType) (*Vector, error) {
	if stride <= 0 || stride > usable(p) {
		return nil, ErrStrideTooLarge
	}
	head, err := appendPage(p, format.NoPage, t)
	if err != nil {
		return nil, err
	}
	return &Vector{p: p, stride: stride, epp: usable(p) / stride, pages: []format.PageID{head}}, nil
}

// OpenVector reopens a vector rooted at head with the given stride and logical
// element count, rebuilding its page directory by walking the chain.
func OpenVector(p *pager.Pager, head format.PageID, stride, count int) (*Vector, error) {
	if stride <= 0 || stride > usable(p) {
		return nil, ErrStrideTooLarge
	}
	ids, err := walkChain(p, head)
	if err != nil {
		return nil, err
	}
	return &Vector{p: p, stride: stride, epp: usable(p) / stride, pages: ids, count: count}, nil
}

// Free returns every page the vector occupies to the pager's free list. The
// vector is dead afterward and must not be used again; a checkpoint calls this on
// a vector it has rebuilt into a fresh one so the old pages can be reused.
func (v *Vector) Free() error { return freeChain(v.p, v.pages) }

// Head returns the chain's first page id (persist this to reopen the vector).
func (v *Vector) Head() format.PageID { return v.pages[0] }

// Count returns the number of elements (persist this to reopen the vector).
func (v *Vector) Count() int { return v.count }

func (v *Vector) locate(i int) (format.PageID, int) {
	return v.pages[i/v.epp], (i % v.epp) * v.stride
}

// Get copies element i into dst (which must be at least stride bytes).
func (v *Vector) Get(i int, dst []byte) error {
	if i < 0 || i >= v.count {
		return fmt.Errorf("gr/store: index %d out of range [0,%d)", i, v.count)
	}
	id, off := v.locate(i)
	f, err := v.p.ReadPage(id)
	if err != nil {
		return err
	}
	copy(dst[:v.stride], dataRegion(f)[off:off+v.stride])
	v.p.Unpin(f)
	return nil
}

// Set overwrites element i with src (stride bytes are written).
func (v *Vector) Set(i int, src []byte) error {
	if i < 0 || i >= v.count {
		return fmt.Errorf("gr/store: index %d out of range [0,%d)", i, v.count)
	}
	id, off := v.locate(i)
	f, err := v.p.ReadPage(id)
	if err != nil {
		return err
	}
	copy(dataRegion(f)[off:off+v.stride], src[:v.stride])
	v.p.MarkDirty(f)
	v.p.Unpin(f)
	return nil
}

// Append adds a new element and returns its index, growing the chain by a page
// when the current tail page is full.
func (v *Vector) Append(src []byte) (int, error) {
	idx := v.count
	pageIdx := idx / v.epp
	if pageIdx == len(v.pages) {
		id, err := appendPage(v.p, v.pages[len(v.pages)-1], v.pages0Type())
		if err != nil {
			return 0, err
		}
		v.pages = append(v.pages, id)
	}
	v.count++
	if err := v.Set(idx, src); err != nil {
		v.count--
		return 0, err
	}
	return idx, nil
}

// pages0Type reads the page type of the chain head so appended pages match it.
func (v *Vector) pages0Type() format.PageType {
	f, err := v.p.ReadPage(v.pages[0])
	if err != nil {
		return format.PageTypeColumn
	}
	t := format.ReadHeader(f.Data).Type
	v.p.Unpin(f)
	return t
}
