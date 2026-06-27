package pager

import "github.com/tamnd/gr/format"

// The free list lets a freed page be reused before the file grows (doc 03 §8.4,
// doc 05 §2.3). It is a chain of FREELIST pages rooted at the header's
// FreeListRoot: the head page holds an array of freed page ids and a link to the
// next free-list page. A free page that is itself part of the chain doubles as a
// free page (the trunk-and-leaf design of doc 03 §16.11), so freeing never needs
// to allocate.
//
// A free-list page is a normal pager page: it is checksummed and logged like any
// other, so a free or an alloc is a page change that the WAL makes crash safe.
// Recovery restores a consistent free list along with the rest of the file, so a
// crash can neither leak nor double-allocate a page (doc 03 §8.4, invariant 6).
//
// The whole mechanism is inert until something frees a page: with an empty free
// list AllocPage always grows the file, exactly as before.

// Free-list page payload layout: a small in-page header then the id array.
const (
	flNextOff  = 0  // u64: the next free-list page id, 0 ends the chain
	flCountOff = 8  // u32: how many freed ids this page holds
	flArrayOff = 12 // u64 array of freed page ids
)

// flCapacity is how many freed ids one free-list page can hold.
func (p *Pager) flCapacity() int {
	return (format.PayloadSize(p.pageSize) - flArrayOff) / 8
}

// payload returns the page body between the page header and the checksum trailer.
func (p *Pager) payload(f *Frame) []byte {
	return f.Data[format.PayloadOffset() : len(f.Data)-format.ChecksumSize]
}

// FreePage returns a page to the free list so a later AllocPage can reuse it
// before the file grows. The change is durable at the next Commit. Freeing page 0
// (the header) is a no-op, and freeing a page never allocates: when the head page
// is full the freed page becomes the new head, linking the old head after it.
func (p *Pager) FreePage(id format.PageID) error {
	if p.readOnly {
		return ErrReadOnly
	}
	if id == format.NoPage {
		return nil
	}
	root := format.PageID(p.header.FreeListRoot)
	if root != format.NoPage {
		hf, err := p.ReadPage(root)
		if err != nil {
			return err
		}
		body := p.payload(hf)
		count := int(format.U32(body[flCountOff:]))
		if count < p.flCapacity() {
			format.PutU64(body[flArrayOff+count*8:], uint64(id))
			format.PutU32(body[flCountOff:], uint32(count+1))
			p.MarkDirty(hf)
			p.Unpin(hf)
			return nil
		}
		p.Unpin(hf)
	}
	// No head, or the head is full: turn the freed page into the new head, linking
	// the old head (if any) after it. This needs no allocation.
	nf, err := p.ReadPage(id)
	if err != nil {
		return err
	}
	zero(nf.Data)
	format.WriteHeader(nf.Data, format.PageHeader{Type: format.PageTypeFree})
	body := p.payload(nf)
	format.PutU64(body[flNextOff:], uint64(root))
	format.PutU32(body[flCountOff:], 0)
	p.MarkDirty(nf)
	p.Unpin(nf)
	p.header.FreeListRoot = uint64(id)
	p.headerDirty = true
	return nil
}

// FreeCount returns how many pages the free list holds, walking the trunk chain
// (doc 17 §6.15). Each chain page is itself a free page (the trunk doubles as a
// freed page, §16.11), so the total is, over every chain page, one for the page
// itself plus the freed ids it carries.
func (p *Pager) FreeCount() (uint64, error) {
	var total uint64
	id := format.PageID(p.header.FreeListRoot)
	for id != format.NoPage {
		hf, err := p.ReadPage(id)
		if err != nil {
			return 0, err
		}
		body := p.payload(hf)
		total += 1 + uint64(format.U32(body[flCountOff:]))
		id = format.PageID(format.U64(body[flNextOff:]))
		p.Unpin(hf)
	}
	return total, nil
}

// popFree takes one reusable page id from the free list, or reports the list is
// empty so the caller grows the file. It pops from the head page's id array; when
// that array is empty it reuses the head page itself and advances the root to the
// next free-list page.
func (p *Pager) popFree() (format.PageID, bool, error) {
	root := format.PageID(p.header.FreeListRoot)
	if root == format.NoPage {
		return 0, false, nil
	}
	hf, err := p.ReadPage(root)
	if err != nil {
		return 0, false, err
	}
	body := p.payload(hf)
	count := int(format.U32(body[flCountOff:]))
	if count > 0 {
		id := format.PageID(format.U64(body[flArrayOff+(count-1)*8:]))
		format.PutU32(body[flCountOff:], uint32(count-1))
		p.MarkDirty(hf)
		p.Unpin(hf)
		return id, true, nil
	}
	// The head holds no ids: the head page itself is the page to hand back, and the
	// root moves to the next free-list page.
	next := format.PageID(format.U64(body[flNextOff:]))
	p.Unpin(hf)
	p.header.FreeListRoot = uint64(next)
	p.headerDirty = true
	return root, true, nil
}

// reuse turns a reclaimed page id into a freshly zeroed, typed, pinned frame, the
// same shape AllocPage's grow path returns.
func (p *Pager) reuse(id format.PageID, t format.PageType) (*Frame, error) {
	f, err := p.ReadPage(id)
	if err != nil {
		return nil, err
	}
	zero(f.Data)
	format.WriteHeader(f.Data, format.PageHeader{Type: t})
	f.dirty = true
	f.ref.Store(true)
	return f, nil
}

// zero clears a page buffer.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
