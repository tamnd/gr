// Package store provides the durable, page-backed building blocks the graph
// storage engine is built from (spec 2060 doc 04 §2, doc 03 §6). Two primitives
// cover every store the engine needs:
//
//   - Vector: a growable fixed-stride array (node records, relationship records,
//     CSR offset/neighbor/edge arrays, columnar property segments, the id-map).
//   - Log: an append-only byte stream (the catalog token dictionaries, the
//     dynamic/blob store).
//
// Both live in a self-describing chain of pager pages: each page reserves its
// last 8 payload bytes for the page id of the next page in the chain (0 ends the
// chain), so a store reconstructs its page directory on open by walking the
// chain from its head. All mutations go through the pager, so every store
// inherits the substrate's durability — a store change is durable when the
// transaction that made it commits, and recovers with the durable-prefix
// property proven in M0.
//
// M1 stores everything in the RAW (uncompressed) encoding; compression and
// page-granular segmentation are M4 (doc 15, doc 25 §4.7).
package store

import (
	"errors"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
)

// nextPtrSize is the per-page reserved tail holding the next page id.
const nextPtrSize = 8

// ErrStrideTooLarge means a record stride does not fit in a page.
var ErrStrideTooLarge = errors.New("gr/store: record stride larger than a page")

// dataRegion returns the writable payload of a frame, excluding the next-pointer
// tail. The chain's next pointer lives in the final 8 bytes of the payload.
func dataRegion(f *pager.Frame) []byte {
	payload := f.Data[format.PayloadOffset() : len(f.Data)-format.ChecksumSize]
	return payload[:len(payload)-nextPtrSize]
}

func getNext(f *pager.Frame) format.PageID {
	payload := f.Data[format.PayloadOffset() : len(f.Data)-format.ChecksumSize]
	return format.PageID(format.U64(payload[len(payload)-nextPtrSize:]))
}

func setNext(f *pager.Frame, id format.PageID) {
	payload := f.Data[format.PayloadOffset() : len(f.Data)-format.ChecksumSize]
	format.PutU64(payload[len(payload)-nextPtrSize:], uint64(id))
}

// usable is the per-page data capacity (payload minus the next pointer).
func usable(p *pager.Pager) int { return p.PayloadSize() - nextPtrSize }

// walkChain returns the page ids of a chain starting at head, in order.
func walkChain(p *pager.Pager, head format.PageID) ([]format.PageID, error) {
	var ids []format.PageID
	for id := head; id != format.NoPage; {
		f, err := p.ReadPage(id)
		if err != nil {
			return nil, err
		}
		next := getNext(f)
		p.Unpin(f)
		ids = append(ids, id)
		id = next
	}
	return ids, nil
}

// appendPage allocates a new chain page of the given type, links it after prev
// (if prev is not NoPage), and returns its id.
func appendPage(p *pager.Pager, prev format.PageID, t format.PageType) (format.PageID, error) {
	nf, err := p.AllocPage(t)
	if err != nil {
		return 0, err
	}
	setNext(nf, format.NoPage)
	id := nf.ID()
	p.MarkDirty(nf)
	p.Unpin(nf)
	if prev != format.NoPage {
		pf, err := p.ReadPage(prev)
		if err != nil {
			return 0, err
		}
		setNext(pf, id)
		p.MarkDirty(pf)
		p.Unpin(pf)
	}
	return id, nil
}
