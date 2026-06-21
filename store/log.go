package store

import (
	"fmt"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
)

// Log is an append-only byte stream over a page chain. Bytes are packed densely
// across pages: a write that does not fit in the current tail page straddles
// into freshly allocated pages, so the stream has no per-record padding. The
// catalog token dictionaries and any dynamic/blob payloads are stored as Logs
// (doc 04 §2, doc 09 §3). A Log exposes its total length and a Read that copies
// an arbitrary byte range out of the stream, so a higher layer can index into it
// with absolute offsets.
type Log struct {
	p     *pager.Pager
	pages []format.PageID
	cap   int // usable bytes per page
	len   int // logical bytes written
}

// CreateLog creates an empty log whose pages carry the given page type.
func CreateLog(p *pager.Pager, t format.PageType) (*Log, error) {
	head, err := appendPage(p, format.NoPage, t)
	if err != nil {
		return nil, err
	}
	return &Log{p: p, pages: []format.PageID{head}, cap: usable(p)}, nil
}

// OpenLog reopens a log rooted at head with the given logical length.
func OpenLog(p *pager.Pager, head format.PageID, length int) (*Log, error) {
	ids, err := walkChain(p, head)
	if err != nil {
		return nil, err
	}
	return &Log{p: p, pages: ids, cap: usable(p), len: length}, nil
}

// Free returns every page the log occupies to the pager's free list. The log is
// dead afterward and must not be used again; a checkpoint calls this on a log it
// has replaced so the old pages can be reused.
func (l *Log) Free() error { return freeChain(l.p, l.pages) }

// Head returns the chain's first page id (persist this to reopen the log).
func (l *Log) Head() format.PageID { return l.pages[0] }

// Len returns the number of bytes written (persist this to reopen the log).
func (l *Log) Len() int { return l.len }

// Append writes b at the end of the stream and returns the absolute offset at
// which it was written.
func (l *Log) Append(b []byte) (int, error) {
	start := l.len
	off := l.len
	for len(b) > 0 {
		pageIdx := off / l.cap
		if pageIdx == len(l.pages) {
			id, err := appendPage(l.p, l.pages[len(l.pages)-1], l.headType())
			if err != nil {
				return 0, err
			}
			l.pages = append(l.pages, id)
		}
		within := off % l.cap
		n := min(l.cap-within, len(b))
		f, err := l.p.ReadPage(l.pages[pageIdx])
		if err != nil {
			return 0, err
		}
		copy(dataRegion(f)[within:within+n], b[:n])
		l.p.MarkDirty(f)
		l.p.Unpin(f)
		off += n
		b = b[n:]
	}
	l.len = off
	return start, nil
}

// Read copies length bytes starting at absolute offset into dst.
func (l *Log) Read(offset, length int, dst []byte) error {
	if offset < 0 || length < 0 || offset+length > l.len {
		return fmt.Errorf("gr/store: log read [%d,%d) out of range [0,%d)", offset, offset+length, l.len)
	}
	off := offset
	got := 0
	for got < length {
		pageIdx := off / l.cap
		within := off % l.cap
		n := min(l.cap-within, length-got)
		f, err := l.p.ReadPage(l.pages[pageIdx])
		if err != nil {
			return err
		}
		copy(dst[got:got+n], dataRegion(f)[within:within+n])
		l.p.Unpin(f)
		off += n
		got += n
	}
	return nil
}

func (l *Log) headType() format.PageType {
	f, err := l.p.ReadPage(l.pages[0])
	if err != nil {
		return format.PageTypeDynamic
	}
	t := format.ReadHeader(f.Data).Type
	l.p.Unpin(f)
	return t
}
