// Package wal is gr's write-ahead log and crash recovery (spec 2060 doc 05).
// The design follows the proven SQLite-WAL shape: an append-only sequence of
// full-page-image frames, each carrying a running (chained) checksum seeded by a
// per-WAL salt, so that a torn or otherwise invalid frame — and everything after
// it — is ignored on recovery. A frame whose truncate field is non-zero is a
// commit frame; recovery applies frames up to and including the last valid
// commit frame and no further, which is exactly the durable-prefix property
// (doc 05 §10 invariants 5, 6, 7).
//
// The full-page-image design gives torn-write protection for free: a partially
// written page in the main database file is repaired on recovery by redoing the
// committed full image from the WAL (doc 05 §7).
package wal

import (
	"errors"
	"hash/crc32"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
)

// SyncLevel controls how aggressively the WAL is flushed (doc 05 §4, doc 24).
type SyncLevel uint8

const (
	// SyncOff never fsyncs: fastest, not crash-safe. Test/bulk use only.
	SyncOff SyncLevel = iota
	// SyncNormal fsyncs the WAL at each commit (durable commits).
	SyncNormal
	// SyncFull fsyncs the WAL at commit and the database at checkpoint.
	SyncFull
)

// Frame/header geometry. The WAL file begins with a header, then frames; each
// frame is a frame header followed by one page image.
const (
	walMagic       = 0x6772_77_61 // "grwa"
	walHeaderSize  = 32
	frameHeaderLen = 24
)

// WAL is a write-ahead log over a vfs.File.
type WAL struct {
	f        vfs.File
	pageSize uint32
	sync     SyncLevel

	salt1, salt2 uint32 // per-WAL salt, randomized on reset
	cksum1       uint32 // running checksum state (chained over frames)
	cksum2       uint32

	frameCount int    // frames physically present after the header
	maxLSN     uint64 // highest LSN written

	// Observability counters (doc 20 §5.2), bumped on the append and reset paths under the engine
	// write lock the single writer holds, and read lock-free as atomics off the metrics snapshot path.
	// bytesWritten is the cumulative frame bytes (header plus image) appended; framesPage and
	// framesCommit split the frames appended by kind; sizeBytes is the current on-disk WAL size,
	// republished after each append and each reset so the gauge reads it without the frame count lock.
	bytesWritten atomic.Uint64
	framesPage   atomic.Uint64
	framesCommit atomic.Uint64
	sizeBytes    atomic.Uint64

	// fsync observability (doc 20 §5.2). Every fsync the WAL issues, at commit and at reset, runs
	// through syncFile so these stay in step. fsyncTotal is the cumulative fsync count, the durable
	// barrier count a commit amortizes against; fsyncErrors counts fsyncs that returned an error, any
	// nonzero value a durability alarm. The per-call durations cannot be delta-mirrored the way a
	// counter can, so they are buffered under fsyncDurMu and the metrics sync step drains the buffer
	// into the fsync-latency histogram, the same drain-queue shape the version-GC duration uses. The
	// counters are atomics read lock-free off the snapshot path; the buffer's lock is a leaf lock the
	// writer takes under the engine write lock and the metrics path takes alone, so neither inverts.
	fsyncTotal     atomic.Uint64
	fsyncErrors    atomic.Uint64
	fsyncDurMu     sync.Mutex
	fsyncDurations []float64

	crcTab *crc32.Table
}

// Stats is a point-in-time view of the WAL's cumulative write activity and current footprint (doc 20
// §5.2). The counters are cumulative since the WAL was opened; SizeBytes is the live on-disk size.
type Stats struct {
	BytesWritten uint64
	FramesPage   uint64
	FramesCommit uint64
	SizeBytes    uint64
	FsyncTotal   uint64
	FsyncErrors  uint64
}

// Stats returns the WAL's cumulative write counters and current size, each a lock-free atomic load, so
// the metrics path reads them without taking any WAL or engine lock (doc 20 §5.2). The fsync durations
// are not here: a histogram needs each sample, so they are drained separately by DrainFsyncDurations.
func (w *WAL) Stats() Stats {
	return Stats{
		BytesWritten: w.bytesWritten.Load(),
		FramesPage:   w.framesPage.Load(),
		FramesCommit: w.framesCommit.Load(),
		SizeBytes:    w.sizeBytes.Load(),
		FsyncTotal:   w.fsyncTotal.Load(),
		FsyncErrors:  w.fsyncErrors.Load(),
	}
}

// DrainFsyncDurations returns the fsync durations in seconds buffered since the last drain and clears
// the buffer (doc 20 §5.2). The metrics sync step calls it on a scrape and observes each sample into
// the fsync-latency histogram exactly once. It takes only the buffer's leaf lock, never the engine
// lock, so a long-held write transaction cannot deadlock a snapshot. A nil return means no fsync ran
// since the last drain.
func (w *WAL) DrainFsyncDurations() []float64 {
	w.fsyncDurMu.Lock()
	defer w.fsyncDurMu.Unlock()
	if len(w.fsyncDurations) == 0 {
		return nil
	}
	out := w.fsyncDurations
	w.fsyncDurations = nil
	return out
}

// syncFile fsyncs the WAL file, counting and timing the call for the fsync metrics (doc 20 §5.2).
// Every Sync the WAL issues, at commit in Append and at Reset, goes through here so the fsync count,
// the latency samples, and the error count stay in step. The duration is recorded whether or not the
// fsync succeeded, since a failed barrier still cost wall-clock time; a nonzero error count is a
// durability alarm and the caller turns the error fatal. It runs under the engine write lock, so the
// buffer append is uncontended; the counters are atomics a snapshot reads lock-free.
func (w *WAL) syncFile() error {
	start := time.Now()
	err := w.f.Sync()
	dur := time.Since(start).Seconds()
	w.fsyncTotal.Add(1)
	w.fsyncDurMu.Lock()
	w.fsyncDurations = append(w.fsyncDurations, dur)
	w.fsyncDurMu.Unlock()
	if err != nil {
		w.fsyncErrors.Add(1)
		return err
	}
	return nil
}

// publishSize republishes the current on-disk WAL size for the size gauge, called after every append
// and reset under the engine write lock. It is header plus the physical frames currently present.
func (w *WAL) publishSize() {
	w.sizeBytes.Store(uint64(walHeaderSize) + uint64(w.frameCount)*uint64(w.frameSize()))
}

var (
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("gr/wal: closed")
)

// Frame is one page image staged for commit.
type Frame struct {
	PageID format.PageID
	Image  []byte // exactly pageSize bytes; the full page image
}

// Open opens (creating if absent) the WAL file and prepares it for appends. It
// does not perform recovery; call Recover for that. saltSeed seeds the initial
// salt so tests are deterministic.
func Open(f vfs.File, pageSize uint32, sync SyncLevel, saltSeed uint64) (*WAL, error) {
	w := &WAL{
		f:        f,
		pageSize: pageSize,
		sync:     sync,
		crcTab:   crc32.IEEETable,
	}
	w.salt1 = uint32(saltSeed)
	w.salt2 = uint32(saltSeed >> 32)
	return w, nil
}

func (w *WAL) frameSize() int64 { return int64(frameHeaderLen) + int64(w.pageSize) }

// writeHeader writes (or rewrites) the WAL header reflecting current salts.
func (w *WAL) writeHeader() error {
	b := make([]byte, walHeaderSize)
	format.PutU32(b[0:], walMagic)
	format.PutU32(b[4:], w.pageSize)
	format.PutU32(b[8:], w.salt1)
	format.PutU32(b[12:], w.salt2)
	// bytes 16..27 reserved
	format.PutU32(b[28:], crc32.Checksum(b[:28], w.crcTab))
	if _, err := w.f.WriteAt(b, 0); err != nil {
		return err
	}
	// reset running checksum to the salt
	w.cksum1, w.cksum2 = w.salt1, w.salt2
	return nil
}

// Append stages a transaction's frames and, if commit is true, writes a commit
// frame (dbPages = the database size in pages after this transaction) and
// fsyncs per the sync level. It returns the commit LSN. The whole batch is
// written before the fsync, so the fsync is the single commit point: a crash
// before it loses the whole batch, a crash after it keeps the whole batch — the
// atomic-commit property (doc 05 §10 invariant 2).
func (w *WAL) Append(frames []Frame, commit bool, dbPages uint64) (uint64, error) {
	off := int64(walHeaderSize) + int64(w.frameCount)*w.frameSize()
	for i, fr := range frames {
		if len(fr.Image) != int(w.pageSize) {
			return 0, errors.New("gr/wal: frame image is not page-sized")
		}
		w.maxLSN++
		lsn := w.maxLSN
		isCommit := commit && i == len(frames)-1
		var trunc uint64
		if isCommit {
			trunc = dbPages
		}
		fh := make([]byte, frameHeaderLen)
		format.PutU64(fh[0:], uint64(fr.PageID))
		format.PutU64(fh[8:], trunc) // non-zero marks a commit frame
		// advance the running checksum over this frame's header(0..16) + image
		w.cksum1, w.cksum2 = chain(w.crcTab, w.cksum1, w.cksum2, fh[0:16])
		w.cksum1, w.cksum2 = chain(w.crcTab, w.cksum1, w.cksum2, fr.Image)
		format.PutU32(fh[16:], w.cksum1)
		format.PutU32(fh[20:], w.cksum2)
		_ = lsn

		if _, err := w.f.WriteAt(fh, off); err != nil {
			return 0, err
		}
		if _, err := w.f.WriteAt(fr.Image, off+frameHeaderLen); err != nil {
			return 0, err
		}
		off += w.frameSize()
		w.frameCount++
		// Count the frame's bytes and its kind for the WAL metrics (doc 20 §5.2). Every frame here is a
		// full page image; the last frame of a commit batch is the commit frame, the rest are page frames.
		w.bytesWritten.Add(uint64(w.frameSize()))
		if isCommit {
			w.framesCommit.Add(1)
		} else {
			w.framesPage.Add(1)
		}
	}
	w.publishSize()
	if commit && w.sync >= SyncNormal {
		if err := w.syncFile(); err != nil {
			return 0, err
		}
	}
	return w.maxLSN, nil
}

// chain advances the two-word running checksum over b (doc 05 §7). It is the
// SQLite WAL checksum: a simple, fast, order-and-content-sensitive chain.
func chain(_ *crc32.Table, s1, s2 uint32, b []byte) (uint32, uint32) {
	// process 8 bytes at a time, little-endian word pairs
	for i := 0; i+8 <= len(b); i += 8 {
		s1 += format.U32(b[i:]) + s2
		s2 += format.U32(b[i+4:]) + s1
	}
	// tail (frame images are page-sized multiples of 8; headers are 16); if any
	// remainder, fold it in deterministically
	if rem := len(b) % 8; rem != 0 {
		var tail [8]byte
		copy(tail[:], b[len(b)-rem:])
		s1 += format.U32(tail[0:]) + s2
		s2 += format.U32(tail[4:]) + s1
	}
	return s1, s2
}

// FrameCount returns the number of physical frames currently in the WAL.
func (w *WAL) FrameCount() int { return w.frameCount }

// MaxLSN returns the highest LSN appended.
func (w *WAL) MaxLSN() uint64 { return w.maxLSN }

// Reset truncates the WAL back to an empty header with a fresh salt, called
// after a successful checkpoint so the next epoch of frames starts clean. The
// fresh salt ensures stale frames left in the file (from a crash mid-truncate)
// are rejected by the checksum chain on the next recovery.
func (w *WAL) Reset(newSalt uint64) error {
	w.salt1 = uint32(newSalt)
	w.salt2 = uint32(newSalt >> 32)
	w.frameCount = 0
	if err := w.f.Truncate(walHeaderSize); err != nil {
		return err
	}
	if err := w.writeHeader(); err != nil {
		return err
	}
	w.publishSize()
	if w.sync >= SyncNormal {
		return w.syncFile()
	}
	return nil
}

// Init writes a fresh header for a brand-new WAL.
func (w *WAL) Init() error {
	if err := w.writeHeader(); err != nil {
		return err
	}
	w.publishSize()
	return nil
}

// Close closes the underlying file.
func (w *WAL) Close() error { return w.f.Close() }
