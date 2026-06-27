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
	"time"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// PageIOObserver receives the wall-clock latency in seconds of each page read and write the pager
// performs against the VFS (doc 20 §4.2): the device-side view that complements the cache hit rate. The
// root wires one that observes into the gr_page_read_seconds and gr_page_write_seconds histograms;
// until then it is nil and the pager times nothing. The observe is lock-free on the metric side (an
// atomic bucket add), so the pager calls it on the read and commit paths without taking the registry
// lock, the same way it keeps its hit and miss counts off the registry. The interface keeps the pager
// free of the metric package, the same decoupling the constraint observer uses.
type PageIOObserver interface {
	ObservePageRead(seconds float64)
	ObservePageWrite(seconds float64)
}

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
	// PageIO observes the latency of each page read and write against the VFS (doc 20 §4.2). It is set
	// before open does any I/O, so the page reads that load the stores into memory at open are timed
	// too, the dominant read I/O this engine does today. nil leaves page I/O untimed.
	PageIO PageIOObserver
}

var (
	// ErrReadOnly is returned by mutating calls on a read-only pager.
	ErrReadOnly = errors.New("gr/pager: database is read-only")
	// ErrPinned is returned if Close is called with pages still pinned.
	ErrPinned = errors.New("gr/pager: pages still pinned at close")
	// ErrBadChecksum indicates a page failed its checksum on read.
	ErrBadChecksum = errors.New("gr/pager: page checksum mismatch")
)

// store enumerates the file's logical stores for per-store page I/O accounting (doc 20 §4.2). The order
// fixes the array slot each store's read and write counters live in, and storeLabels gives the label each
// slot reports. The set matches the catalogue's store label domain (doc 20 §4.2, doc 03 §4); page types
// without a store of their own (the header, the section directory, generic data, statistics, credentials)
// fold into catalog, since they are all small file-metadata stores an operator reasons about together.
type store int

const (
	storeNode store = iota
	storeRel
	storeRelGroup
	storePropCol
	storeDynamic
	storeIndex
	storeCatalog
	storeFreelist
	numStores
)

var storeLabels = [numStores]string{"node", "rel", "relgroup", "propcol", "dynamic", "index", "catalog", "freelist"}

// storeOf maps a page type to the store its reads and writes are attributed to (doc 20 §4.2). The id-map
// is index infrastructure, so it reports as index; everything without a store of its own reports as
// catalog (the default), the small file-metadata bucket.
func storeOf(t format.PageType) store {
	switch t {
	case format.PageTypeNode:
		return storeNode
	case format.PageTypeRel:
		return storeRel
	case format.PageTypeRelGroup:
		return storeRelGroup
	case format.PageTypeColumn:
		return storePropCol
	case format.PageTypeDynamic:
		return storeDynamic
	case format.PageTypeIDMap:
		return storeIndex
	case format.PageTypeFree:
		return storeFreelist
	default:
		return storeCatalog
	}
}

// StorePageIO is one store's cumulative page read and write counts since open (doc 20 §4.2), the per-store
// breakdown of file I/O the gr_pages_read_total{store} and gr_pages_written_total{store} counters expose.
type StorePageIO struct {
	Store   string
	Read    uint64
	Written uint64
}

// Frame is a cached page. Callers read and mutate Data (the full page image),
// call Pager.MarkDirty after mutating, and Unpin when done.
//
// pin and ref are atomic so the read path can pin a resident frame and set its
// reference bit while holding only the pool's read lock, and Unpin can release a
// pin with no lock at all (doc 12 §10). Many morsel-parallel reader goroutines
// share one snapshot, so a per-read mutex on these counters serializes them; the
// atomics let concurrent readers of resident pages proceed without parking on a
// lock. dirty is written only on the write path under the engine's exclusive
// lock, so it stays a plain field.
type Frame struct {
	id    format.PageID
	Data  []byte
	pin   atomic.Int32
	dirty bool
	ref   atomic.Bool // clock reference bit
}

// ID returns the page id of the frame.
func (f *Frame) ID() format.PageID { return f.id }

// pageTable maps a page id to its resident frame. The pager swaps it atomically
// (copy-on-write) so a read hit can index it lock-free; see Pager.lookup.
type pageTable = map[format.PageID]*Frame

// counterShards is the number of cache lines a stripedCounter spreads its writes
// over. A power of two so the page-id index is a mask, not a divide; 64 is enough
// to scatter a working set's worth of pages across distinct lines on the core
// counts gr runs morsels on.
const counterShards = 64

// stripedCounter is a monotonically increasing counter sharded across cache lines
// to remove the false sharing a single hot atomic suffers under many writers. Each
// shard is padded to its own 64-byte line so incrementing one never invalidates
// another. Add takes a key (a page id) and increments the shard that key hashes to,
// so reads of distinct pages land on distinct lines; Sum totals the shards and is
// only called for metrics, off the hot path.
type stripedCounter struct {
	shards [counterShards]struct {
		v atomic.Uint64
		_ [56]byte // pad atomic.Uint64 (8B) out to a full cache line
	}
}

func (c *stripedCounter) Add(key uint64) { c.shards[key&(counterShards-1)].v.Add(1) }

func (c *stripedCounter) Sum() uint64 {
	var total uint64
	for i := range c.shards {
		total += c.shards[i].v.Load()
	}
	return total
}

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

	// lookup is the page table: page id to resident frame. It is an atomically
	// swapped immutable map so a read hit is lock-free: ReadPage loads the pointer
	// (a shared-line read that never invalidates another core's cache) and indexes
	// the map, with no lock and no shared-line write, so every morsel-parallel reader
	// scales without contending (doc 12 §10). A reader that misses must mutate the
	// table; it does so under mu by cloning the map, adding the frame, and storing the
	// new pointer (copy-on-write), so a concurrent reader keeps reading the old map
	// safely. A write transaction holds the engine's exclusive lock, so no reader runs
	// beside it and it mutates the live map in place (no clone), keeping a bulk load
	// O(1) per page.
	//
	// mu guards the clock ring (clock, hand) and serializes the table mutators
	// (admit, evict, the COW store) among writers and missing readers; the lock-free
	// read hit never takes it. Unpin touches only a frame's own atomic and takes no
	// lock either.
	mu       sync.Mutex
	lookup   atomic.Pointer[pageTable]
	clock    []*Frame
	hand     int
	maxPool  int
	saltNext uint64

	// hits and misses are the cumulative buffer-pool lookup outcomes since open, the page-table
	// hit rate that is the single most important storage metric (doc 20 §4.1). A hit is a ReadPage
	// that found the page resident; a miss is one that faulted it from disk. hits is striped across
	// cache lines and indexed by page id: a single shared counter incremented on every hit would
	// bounce one cache line between every morsel-parallel worker (millions of hits per scan), which
	// alone serialized the parallel read path; striping by page id spreads the writes so resident-set
	// reads on distinct pages touch distinct lines (doc 12 §10). PoolStats sums the shards. misses are
	// rare (one fault per page), so a single counter is fine.
	hits   stripedCounter
	misses atomic.Uint64

	// writers counts the write transactions currently holding the engine's exclusive
	// lock (it is 0 or 1 in practice). A reader pins a frame to keep eviction from
	// dropping a page it still holds; but evict never recycles a frame's buffer, and
	// a writer holds the engine's exclusive lock so no reader runs beside it, so among
	// the morsel-parallel readers an unpinned frame is still safe to read: each holds
	// the *Frame and copies out of its stable, snapshot-frozen bytes. So ReadPage pins
	// only while a writer is active (single-threaded, uncontended); a read-only scan
	// skips the per-frame pin atomic that otherwise ping-pongs one cache line across
	// every reader core (doc 12 §10). The engine bumps it around its write sections.
	writers atomic.Int32

	// pagesWritten is the cumulative count of page images Commit has copied into the database file
	// since open, the write-back volume the checkpoint metrics attribute to a fold (doc 20 §5.4). Each
	// Commit adds the frames it wrote, including the header frame. It is an atomic so a reader can load
	// it without the pool lock; the engine reads it around its checkpoint to mirror the fold's writes.
	pagesWritten atomic.Uint64

	// pagesRead and pagesWrittenByStore are the cumulative per-store page read and write counts since
	// open, the per-store breakdown of file I/O that localizes a read or write spike to one subsystem
	// (doc 20 §4.2). A read is counted when ReadPage faults a page in from disk (a cache hit reads no
	// page), a write when Commit copies a page image into the file. Each slot is an atomic, so a reader
	// loads the pair without the pool lock, and the page's own header type picks the slot, so no separate
	// page-to-store map is needed.
	pagesRead           [numStores]atomic.Uint64
	pagesWrittenByStore [numStores]atomic.Uint64

	// io observes the latency of each page read and write against the VFS (doc 20 §4.2), or is nil when
	// no observer is wired (a test pager). It is set from Options before open does any I/O and never
	// changed after, so reading it on the read and commit paths needs no synchronization.
	io PageIOObserver

	headerDirty bool
	closed      bool
	recovered   bool          // true if open redid committed WAL frames after a crash
	recoveredTx int           // committed transactions the recovery redid (doc 20 §11.3)
	recoverDur  time.Duration // wall-clock the recovery took, for the recovery_complete event
	recoverWAL  int64         // WAL byte size found at open, the backlog the recovery_start event reports
	recoverFrom uint64        // durable change counter before replay, the recovery_start event's last_checkpoint_lsn
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
		maxPool:  opt.MaxPoolPages,
		saltNext: opt.SaltSeed + 1,
		io:       opt.PageIO,
	}
	empty := make(pageTable)
	p.lookup.Store(&empty)

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
	start := time.Now()
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
			p.recoveredTx = res.Commits
			// Capture what the recovery_start event reports before the replay changes anything:
			// the WAL backlog found on disk and the durable change counter the replay starts from,
			// the last checkpoint point (doc 20 §11.3). The header still holds the pre-replay
			// durable value here, since loadHeader reloads it only after the frames are applied.
			if sz, serr := wf.Size(); serr == nil {
				p.recoverWAL = sz
			}
			p.recoverFrom = p.header.ChangeCounter
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
	if err := p.loadHeader(); err != nil {
		return err
	}
	p.recoverDur = time.Since(start)
	return nil
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

// RecoveryStats returns what the crash recovery on open redid: the number of committed
// transactions replayed, the durable commit sequence the header now records (the change
// counter, bumped once per commit, the last_lsn the recovery_complete event carries), and
// how long the recovery took (doc 20 §11.3). They are zero when the open did not recover.
func (p *Pager) RecoveryStats() (txReplayed int, lastSeq uint64, dur time.Duration) {
	return p.recoveredTx, p.header.ChangeCounter, p.recoverDur
}

// RecoveryStartStats returns what the recovery_start event reports before the replay runs (doc 20
// §11.3): the WAL byte size found at open, the backlog to redo, and the durable change counter the
// replay starts from, the last checkpoint point. They are zero when the open did not recover.
func (p *Pager) RecoveryStartStats() (walSizeBytes int64, lastCheckpointLSN uint64) {
	return p.recoverWAL, p.recoverFrom
}

// PagesWritten returns the cumulative count of page images Commit has copied into the database file
// since open (doc 20 §5.4). The engine reads it around a checkpoint to attribute the fold's write-back
// volume; it is a lock-free atomic load, so it never contends the pool lock.
func (p *Pager) PagesWritten() uint64 { return p.pagesWritten.Load() }

// PagesByStore returns the cumulative per-store page read and write counts since open (doc 20 §4.2), one
// entry per store in label order. The counts are lock-free atomic loads, so the metrics snapshot path
// reads them without the pool lock or the engine lock, the same discipline the hit and miss counts use.
func (p *Pager) PagesByStore() []StorePageIO {
	out := make([]StorePageIO, numStores)
	for i := range out {
		out[i] = StorePageIO{
			Store:   storeLabels[i],
			Read:    p.pagesRead[i].Load(),
			Written: p.pagesWrittenByStore[i].Load(),
		}
	}
	return out
}

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

// PageCount returns the number of pages currently allocated in the file, from the header.
func (p *Pager) PageCount() uint64 { return p.header.PageCount }

// ScanPages calls fn with the raw bytes of every page in the file, in order from 0 to
// PageCount-1. The bytes are a private copy — fn must not retain them. fn returning a
// non-nil error stops the scan and ScanPages returns that error. Page 0 (the header page)
// is included; its raw bytes are read without checksum validation (it carries the file
// Header, not the generic page trailer). All other pages are read raw: the checksum is
// included in the slice and the caller is responsible for validating it.
func (p *Pager) ScanPages(fn func(id format.PageID, raw []byte) error) error {
	buf := make([]byte, p.pageSize)
	for id := format.PageID(0); id < format.PageID(p.header.PageCount); id++ {
		if _, err := p.db.ReadAt(buf, int64(id)*int64(p.pageSize)); err != nil {
			return err
		}
		if err := fn(id, buf); err != nil {
			return err
		}
	}
	return nil
}

// WalkFreeList returns the complete set of page ids that are on the free list,
// detecting cycles and out-of-range pointers. Each trunk page is itself a freed page
// (the trunk-and-leaf layout, doc 03 §16.11), so it is included in the returned set.
func (p *Pager) WalkFreeList() (map[format.PageID]bool, error) {
	free := make(map[format.PageID]bool)
	id := format.PageID(p.header.FreeListRoot)
	for id != format.NoPage {
		if free[id] {
			return free, errors.New("free-list cycle detected")
		}
		if id >= format.PageID(p.header.PageCount) {
			return free, errors.New("free-list page id out of range")
		}
		free[id] = true
		hf, err := p.ReadPage(id)
		if err != nil {
			return free, err
		}
		body := p.payload(hf)
		count := int(format.U32(body[flCountOff:]))
		next := format.PageID(format.U64(body[flNextOff:]))
		for i := range count {
			pid := format.PageID(format.U64(body[flArrayOff+i*8:]))
			free[pid] = true
		}
		p.Unpin(hf)
		id = next
	}
	return free, nil
}
