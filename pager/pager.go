// Package pager is gr's pager and buffer pool over the file format and WAL
// (spec 2060 doc 05 §2, §3). It presents fixed-size pages to the layers above,
// caches them in a buffer pool with pin/unpin and clock eviction, and makes
// commits durable by writing full page images through the WAL and checkpointing
// them into the database file.
//
// M0 uses the correctness-first commit protocol: every commit writes its dirty
// pages to the WAL, fsyncs (the commit point), checkpoints them into the
// database file, and resets the WAL. Reads after a commit therefore come
// straight from the up-to-date database file. Performance work — deferring
// checkpoints, WAL-shadowing reads, group commit across transactions — is M4/M6
// and changes none of the durability contract proven here.
package pager

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// Options configure a pager at open time.
type Options struct {
	// PageSize is used only when creating a new file; an existing file's page
	// size comes from its header. 0 means the default.
	PageSize uint32
	// Sync is the durability level (default SyncFull).
	Sync wal.SyncLevel
	// MaxPoolPages bounds the buffer pool; 0 means a small default.
	MaxPoolPages int
	// SaltSeed seeds WAL salt generation deterministically for tests.
	SaltSeed uint64
	// ReadOnly opens without a writable WAL (queries only).
	ReadOnly bool
}

var (
	// ErrReadOnly is returned by mutating calls on a read-only pager.
	ErrReadOnly = errors.New("gr/pager: database is read-only")
	// ErrPinned is returned if Close is called with pages still pinned.
	ErrPinned = errors.New("gr/pager: pages still pinned at close")
	// ErrBadChecksum indicates a page failed its checksum on read.
	ErrBadChecksum = errors.New("gr/pager: page checksum mismatch")
)

// Frame is a cached page. Callers read and mutate Data (the full page image),
// call Pager.MarkDirty after mutating, and Unpin when done.
type Frame struct {
	id    format.PageID
	Data  []byte
	pin   int
	dirty bool
	ref   bool // clock reference bit
}

// ID returns the page id of the frame.
func (f *Frame) ID() format.PageID { return f.id }

// Pager is the pager and buffer pool.
type Pager struct {
	vfs      vfs.VFS
	path     string
	db       vfs.File
	wal      *wal.WAL
	walPath  string
	header   format.Header
	pageSize uint32
	sync     wal.SyncLevel
	readOnly bool

	// mu guards the buffer pool (pool, clock, hand) and the per-frame pin and ref
	// fields against concurrent readers. A write transaction holds the engine's
	// exclusive lock so it never races a reader, but morsel-parallel execution
	// runs many reader goroutines against one snapshot, and each ReadPage/Unpin
	// mutates the shared pool and a frame's pin count, so the read path needs its
	// own lock below the engine's shared read lock. It is taken only by the leaf
	// methods (ReadPage, Unpin) and the admit/evict helpers they call; the
	// write-path helpers (AllocPage, FreePage, popFree, reuse) run under the
	// engine write lock and reach the pool only through those leaf methods, so
	// they never hold mu themselves and cannot re-enter it.
	mu       sync.Mutex
	pool     map[format.PageID]*Frame
	clock    []*Frame
	hand     int
	maxPool  int
	saltNext uint64

	// hits and misses are the cumulative buffer-pool lookup outcomes since open, the page-table
	// hit rate that is the single most important storage metric (doc 20 §4.1). A hit is a ReadPage
	// that found the page resident; a miss is one that faulted it from disk. They are bumped under
	// mu on the ReadPage paths, so a snapshot reads a consistent pair under the one lock the pool
	// already uses, and the pager never reaches up into the metric registry.
	hits   uint64
	misses uint64

	// pagesWritten is the cumulative count of page images Commit has copied into the database file
	// since open, the write-back volume the checkpoint metrics attribute to a fold (doc 20 §5.4). Each
	// Commit adds the frames it wrote, including the header frame. It is an atomic so a reader can load
	// it without the pool lock; the engine reads it around its checkpoint to mirror the fold's writes.
	pagesWritten atomic.Uint64

	headerDirty bool
	closed      bool
	recovered   bool // true if open redid committed WAL frames after a crash
}

func (o Options) withDefaults() Options {
	if o.PageSize == 0 {
		o.PageSize = format.DefaultPageSize
	}
	if o.MaxPoolPages == 0 {
		o.MaxPoolPages = 1024
	}
	if o.Sync == 0 {
		o.Sync = wal.SyncFull
	}
	return o
}

// Open opens or creates the database at path. It recovers the WAL if present,
// validates the header, and mounts the buffer pool.
func Open(fsys vfs.VFS, path string, opt Options) (*Pager, error) {
	opt = opt.withDefaults()
	created := !fsys.Exists(path)
	db, err := fsys.Open(path, true)
	if err != nil {
		return nil, err
	}
	// An existing file too small to hold a header was never durably initialized
	// (a crash during creation, before page 0's sync). Treat it as fresh.
	if !created {
		if sz, err := db.Size(); err == nil && sz < int64(format.HeaderSize) {
			created = true
		}
	}
	p := &Pager{
		vfs:      fsys,
		path:     path,
		db:       db,
		walPath:  path + "-wal",
		sync:     opt.Sync,
		readOnly: opt.ReadOnly,
		pool:     make(map[format.PageID]*Frame),
		maxPool:  opt.MaxPoolPages,
		saltNext: opt.SaltSeed + 1,
	}

	if created {
		if err := p.initNew(opt.PageSize, opt.SaltSeed); err != nil {
			_ = db.Close()
			return nil, err
		}
		return p, nil
	}

	// Existing file: read the header to learn the page size, then recover.
	if err := p.loadHeader(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := p.recover(opt.SaltSeed); err != nil {
		_ = db.Close()
		return nil, err
	}
	return p, nil
}

// initNew creates a fresh header page and an empty WAL.
func (p *Pager) initNew(pageSize uint32, saltSeed uint64) error {
	h, err := format.NewHeader(pageSize)
	if err != nil {
		return err
	}
	p.header = h
	p.pageSize = pageSize

	// Write page 0 (the header page) directly and durably.
	page0 := make([]byte, pageSize)
	copy(page0, h.Marshal())
	if _, err := p.db.WriteAt(page0, 0); err != nil {
		return err
	}
	if err := p.db.Sync(); err != nil {
		return err
	}

	wf, err := p.vfs.Open(p.walPath, true)
	if err != nil {
		return err
	}
	p.wal, err = wal.Open(wf, pageSize, p.sync, saltSeed)
	if err != nil {
		return err
	}
	return p.wal.Init()
}

// loadHeader reads and validates page 0's header.
func (p *Pager) loadHeader() error {
	// We don't yet know the page size; the header lives in the first HeaderSize
	// bytes regardless, so read those first.
	hb := make([]byte, format.HeaderSize)
	if _, err := p.db.ReadAt(hb, 0); err != nil {
		return err
	}
	h, err := format.Unmarshal(hb)
	if err != nil {
		return err
	}
	p.header = h
	p.pageSize = h.PageSize
	return nil
}

// recover opens the WAL, redoes any committed frames into the database file,
// resets the WAL, and reloads the header.
func (p *Pager) recover(saltSeed uint64) error {
	wf, err := p.vfs.Open(p.walPath, true)
	if err != nil {
		return err
	}
	res, err := wal.Recover(wf, p.pageSize)
	if err != nil {
		return err
	}
	if res.Committed {
		// A committed WAL prefix means the previous process crashed after the
		// commit fsync but before the checkpoint folded it into the file, so this
		// open is a crash recovery, not a clean reopen (doc 20 §11.3, the open
		// event's recovered flag).
		if len(res.Frames) > 0 {
			p.recovered = true
		}
		// Redo committed frames into the database file (idempotent: full images).
		for _, fr := range res.Frames {
			off := int64(fr.PageID) * int64(p.pageSize)
			if _, err := p.db.WriteAt(fr.Image, off); err != nil {
				return err
			}
		}
		// Extend/truncate the database to its committed page count.
		if err := p.db.Truncate(int64(res.DBPages) * int64(p.pageSize)); err != nil {
			return err
		}
		if err := p.db.Sync(); err != nil {
			return err
		}
	}
	// Reset the WAL to a clean state with a fresh salt.
	p.wal, err = wal.Open(wf, p.pageSize, p.sync, saltSeed)
	if err != nil {
		return err
	}
	if err := p.wal.Reset(p.nextSalt()); err != nil {
		return err
	}
	// Reload the header now that the database reflects the committed prefix.
	return p.loadHeader()
}

func (p *Pager) nextSalt() uint64 {
	p.saltNext = p.saltNext*6364136223846793005 + 1442695040888963407
	return p.saltNext
}

// Header returns a copy of the current file header.
func (p *Pager) Header() format.Header { return p.header }

// CatalogRoot returns the page id the header records as the catalog root, or
// NoPage if none has been set.
func (p *Pager) CatalogRoot() format.PageID { return format.PageID(p.header.CatalogRoot) }

// SetCatalogRoot records the catalog root in the header; it becomes durable at
// the next Commit.
func (p *Pager) SetCatalogRoot(id format.PageID) {
	p.header.CatalogRoot = uint64(id)
	p.headerDirty = true
}

// SectionDir returns the page id the header records as the section-directory
// root (used by the storage engine to find all its store roots), or NoPage.
func (p *Pager) SectionDir() format.PageID { return format.PageID(p.header.SectionDir) }

// SetSectionDir records the section-directory root in the header; it becomes
// durable at the next Commit.
func (p *Pager) SetSectionDir(id format.PageID) {
	p.header.SectionDir = uint64(id)
	p.headerDirty = true
}

// PageSize returns the file's page size.
func (p *Pager) PageSize() uint32 { return p.pageSize }

// Recovered reports whether opening the file redid a committed WAL prefix, which
// means the previous process crashed between a commit's fsync and its checkpoint.
// It feeds the open event's recovered flag (doc 20 §11.3).
func (p *Pager) Recovered() bool { return p.recovered }

// PagesWritten returns the cumulative count of page images Commit has copied into the database file
// since open (doc 20 §5.4). The engine reads it around a checkpoint to attribute the fold's write-back
// volume; it is a lock-free atomic load, so it never contends the pool lock.
func (p *Pager) PagesWritten() uint64 { return p.pagesWritten.Load() }

// WALStats returns the write-ahead log's cumulative write counters and current size (doc 20 §5.2), or
// a zero value on a read-only pager that has no writable WAL. The WAL's own accessor is a lock-free
// atomic load, so this never takes the pool lock or the engine lock and is safe off the snapshot path.
func (p *Pager) WALStats() wal.Stats {
	if p.wal == nil {
		return wal.Stats{}
	}
	return p.wal.Stats()
}

// DrainWALFsyncDurations returns the WAL's fsync durations in seconds buffered since the last drain and
// clears the buffer (doc 20 §5.2), or nil on a read-only pager with no writable WAL. It forwards to the
// WAL's own drain, which takes only its small buffer lock, never the pool lock or the engine lock, so it
// is safe off the metrics snapshot path.
func (p *Pager) DrainWALFsyncDurations() []float64 {
	if p.wal == nil {
		return nil
	}
	return p.wal.DrainFsyncDurations()
}

// PayloadSize returns usable payload bytes per page.
func (p *Pager) PayloadSize() int { return format.PayloadSize(p.pageSize) }
