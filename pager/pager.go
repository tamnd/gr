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

	headerDirty bool
	closed      bool
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

// PayloadSize returns usable payload bytes per page.
func (p *Pager) PayloadSize() int { return format.PayloadSize(p.pageSize) }
