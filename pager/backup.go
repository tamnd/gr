package pager

import "io"

// CopyImage writes the database's committed page image, pages 0 through
// PageCount-1, to w and returns the number of bytes written (doc 17 §6.13, doc
// 19 §15). It is the physical-backup primitive: because every Commit writes the
// committed page images straight into the main file and resets the WAL (see
// Commit), the main file always holds the full last-committed state with an empty
// WAL, so its page image is a self-contained .gr file that opens directly.
//
// The image is read from the database file rather than the buffer pool, so it is
// exactly the committed bytes (checksums and all) and does not disturb the pool.
// The caller must hold writers off for the copy's duration; the engine takes its
// exclusive lock around this call so no commit lands mid-copy.
func (p *Pager) CopyImage(w io.Writer) (int64, error) {
	buf := make([]byte, p.pageSize)
	var total int64
	for id := uint64(0); id < p.header.PageCount; id++ {
		off := int64(id) * int64(p.pageSize)
		if _, err := p.db.ReadAt(buf, off); err != nil {
			return total, err
		}
		n, err := w.Write(buf)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
