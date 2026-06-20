package wal

import (
	"hash/crc32"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
)

// RecoveredFrame is a committed page image to redo into the database file.
type RecoveredFrame struct {
	PageID format.PageID
	Image  []byte
}

// RecoverResult is the outcome of scanning a WAL.
type RecoverResult struct {
	// Frames are the page images of all committed transactions, in WAL order
	// (later frames for the same page supersede earlier ones; the caller applies
	// them in order so the last write wins).
	Frames []RecoveredFrame
	// DBPages is the database size in pages recorded by the last commit frame,
	// used to truncate/extend the database to its committed size.
	DBPages uint64
	// Committed reports whether any committed transaction was found.
	Committed bool
}

// Recover scans the WAL file and returns the committed prefix to redo. It walks
// frames recomputing the chained checksum from the header salt; the first frame
// whose checksum does not match ends the valid region (a torn or partial frame
// and everything after it is discarded — the durable-prefix boundary, doc 05
// §10 invariants 5–7). Within the valid region, the result is the frames up to
// and including the last commit frame; any frames after the last commit frame
// belong to an uncommitted transaction and are dropped (atomic commit).
func Recover(f vfs.File, pageSize uint32) (RecoverResult, error) {
	size, err := f.Size()
	if err != nil {
		return RecoverResult{}, err
	}
	if size < walHeaderSize {
		return RecoverResult{}, nil // no/empty WAL: nothing to recover
	}
	hdr := make([]byte, walHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return RecoverResult{}, err
	}
	if format.U32(hdr[0:]) != walMagic {
		return RecoverResult{}, nil // not a WAL we wrote: ignore
	}
	if format.U32(hdr[28:]) != crc32.Checksum(hdr[:28], crc32.IEEETable) {
		return RecoverResult{}, nil // corrupt header: treat as empty
	}
	hPageSize := format.U32(hdr[4:])
	if hPageSize != pageSize {
		return RecoverResult{}, nil // page-size mismatch: ignore stale WAL
	}
	salt1 := format.U32(hdr[8:])
	salt2 := format.U32(hdr[12:])

	frameSz := int64(frameHeaderLen) + int64(pageSize)
	var all []RecoveredFrame
	var lastCommit = -1
	var lastCommitPages uint64

	s1, s2 := salt1, salt2
	off := int64(walHeaderSize)
	for off+frameSz <= size {
		fh := make([]byte, frameHeaderLen)
		if _, err := f.ReadAt(fh, off); err != nil {
			break
		}
		img := make([]byte, pageSize)
		if _, err := f.ReadAt(img, off+frameHeaderLen); err != nil {
			break
		}
		// recompute the chained checksum over header[0:16] + image
		n1, n2 := chain(crc32.IEEETable, s1, s2, fh[0:16])
		n1, n2 = chain(crc32.IEEETable, n1, n2, img)
		if n1 != format.U32(fh[16:]) || n2 != format.U32(fh[20:]) {
			break // torn/invalid frame: end of the valid region
		}
		s1, s2 = n1, n2
		pageID := format.PageID(format.U64(fh[0:]))
		trunc := format.U64(fh[8:])
		all = append(all, RecoveredFrame{PageID: pageID, Image: img})
		if trunc != 0 {
			lastCommit = len(all) - 1
			lastCommitPages = trunc
		}
		off += frameSz
	}

	if lastCommit < 0 {
		return RecoverResult{}, nil // valid frames but no commit: nothing durable
	}
	return RecoverResult{
		Frames:    all[:lastCommit+1],
		DBPages:   lastCommitPages,
		Committed: true,
	}, nil
}
