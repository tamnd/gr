package pager

import (
	"hash/crc32"
	"time"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/wal"
)

// checksumPage stamps the trailing CRC32 over the page body. The checksum
// covers everything but the trailer itself (doc 03 §4, doc 05 §7); the pager
// validates it on every read so a torn page in the database file is caught even
// before the WAL repairs it.
func checksumPage(buf []byte) {
	sum := crc32.ChecksumIEEE(buf[:len(buf)-format.ChecksumSize])
	format.PutU32(buf[len(buf)-format.ChecksumSize:], sum)
}

// verifyPage reports whether a page's trailing checksum matches its body.
func verifyPage(buf []byte) bool {
	sum := crc32.ChecksumIEEE(buf[:len(buf)-format.ChecksumSize])
	return format.U32(buf[len(buf)-format.ChecksumSize:]) == sum
}

// ReadPage returns the frame for page id, reading it from the database file on a
// miss, and pins it. The caller must Unpin when done. Page 0 (the header page)
// is special: it carries the file Header rather than the generic page header and
// checksum, so it is not checksum-validated here.
func (p *Pager) ReadPage(id format.PageID) (*Frame, error) {
	p.mu.Lock()
	if f, ok := p.pool[id]; ok {
		f.pin++
		f.ref = true
		p.hits++
		p.mu.Unlock()
		return f, nil
	}
	p.mu.Unlock()

	// Fault the page in from disk without holding the lock, so a slow read does
	// not serialize every other reader. ReadAt is safe for concurrent use (it is
	// a pread under the hood), so two readers faulting different pages proceed in
	// parallel.
	buf := make([]byte, p.pageSize)
	start := time.Now()
	if _, err := p.db.ReadAt(buf, int64(id)*int64(p.pageSize)); err != nil {
		return nil, err
	}
	// Time the fault, the device-latency floor under every cache miss (doc 20 §4.2). The observe is a
	// lock-free atomic add, so it does not serialize the concurrent readers this path is unlocked for.
	if p.io != nil {
		p.io.ObservePageRead(time.Since(start).Seconds())
	}
	// Attribute the disk read to its store so the per-store breakdown can localize a read spike to one
	// subsystem (doc 20 §4.2). Page 0 carries the file header rather than a page header, so it reports as
	// catalog (file metadata); every other page names its store in its header type.
	if id == 0 {
		p.pagesRead[storeCatalog].Add(1)
	} else {
		p.pagesRead[storeOf(format.ReadHeader(buf).Type)].Add(1)
	}
	if id != 0 && !verifyPage(buf) {
		return nil, ErrBadChecksum
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Another reader may have faulted the same page in while this one read it from
	// disk; if so, pin the resident frame and drop the duplicate buffer so the pool
	// never holds two frames for one page.
	if f, ok := p.pool[id]; ok {
		f.pin++
		f.ref = true
		p.hits++
		return f, nil
	}
	f := &Frame{id: id, Data: buf, pin: 1, ref: true}
	p.admit(f)
	p.misses++
	return f, nil
}

// AllocPage returns a freshly zeroed, pinned frame of the given type, reusing a
// page from the free list when one is available and otherwise growing the file by
// one page. The new page becomes durable at the next Commit.
func (p *Pager) AllocPage(t format.PageType) (*Frame, error) {
	if p.readOnly {
		return nil, ErrReadOnly
	}
	id, ok, err := p.popFree()
	if err != nil {
		return nil, err
	}
	if ok {
		return p.reuse(id, t)
	}
	id = format.PageID(p.header.PageCount)
	p.header.PageCount++
	p.headerDirty = true
	buf := make([]byte, p.pageSize)
	format.WriteHeader(buf, format.PageHeader{Type: t})
	f := &Frame{id: id, Data: buf, pin: 1, dirty: true, ref: true}
	p.mu.Lock()
	p.admit(f)
	p.mu.Unlock()
	return f, nil
}

// MarkDirty records that a frame's contents changed and must be written at the
// next Commit.
func (p *Pager) MarkDirty(f *Frame) { f.dirty = true }

// Unpin releases one pin on a frame, making it eligible for eviction once clean.
func (p *Pager) Unpin(f *Frame) {
	p.mu.Lock()
	if f.pin > 0 {
		f.pin--
	}
	p.mu.Unlock()
}

// admit inserts a frame into the pool and the clock ring, evicting first if the
// pool is over capacity. The caller must hold p.mu.
func (p *Pager) admit(f *Frame) {
	if len(p.pool) >= p.maxPool {
		p.evict()
	}
	p.pool[f.id] = f
	p.clock = append(p.clock, f)
}

// evict runs the clock algorithm to drop one clean, unpinned frame. A pinned
// frame is never evicted (the headline buffer-pool invariant, doc 05 §3); a
// dirty frame is also skipped because it has uncommitted contents — at most one
// transaction's worth of dirty pages can pile up, so the pool simply grows past
// its soft cap rather than lose data. If nothing is evictable the pool grows.
// The caller must hold p.mu.
func (p *Pager) evict() {
	if len(p.clock) == 0 {
		return
	}
	for scan := 0; scan < 2*len(p.clock); scan++ {
		f := p.clock[p.hand]
		p.hand = (p.hand + 1) % len(p.clock)
		if f.pin > 0 || f.dirty {
			continue
		}
		if f.ref {
			f.ref = false
			continue
		}
		// Evict f.
		delete(p.pool, f.id)
		p.clock = removeFrame(p.clock, f)
		if len(p.clock) > 0 {
			p.hand %= len(p.clock)
		} else {
			p.hand = 0
		}
		return
	}
}

// PoolStats is a point-in-time view of the buffer pool's lookup outcomes and resident population
// (doc 20 §4.1), the numbers the buffer-pool metrics expose. Hits and Misses are cumulative since
// open; Resident is the frames currently holding a page and Bytes the memory they occupy. The four
// are read together under the pool lock so the hit rate and the fill level are consistent.
type PoolStats struct {
	Hits     uint64
	Misses   uint64
	Resident int
	Bytes    int
}

// PoolStats returns the buffer pool's cumulative hit and miss counts and its current resident
// population in one lock acquisition (doc 20 §4.1). It takes only the pool lock, a leaf below the
// engine lock, so the metrics snapshot path reads it without risk of the deadlock an engine-lock
// read would invite behind a long-held write transaction.
func (p *Pager) PoolStats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{
		Hits:     p.hits,
		Misses:   p.misses,
		Resident: len(p.pool),
		Bytes:    len(p.pool) * int(p.pageSize),
	}
}

func removeFrame(ring []*Frame, f *Frame) []*Frame {
	for i, x := range ring {
		if x == f {
			return append(ring[:i], ring[i+1:]...)
		}
	}
	return ring
}

// Commit makes all dirty pages durable. It stamps each dirty page's checksum,
// writes the dirty set (plus the header page if the header changed) to the WAL
// as one atomic batch, fsyncs (the commit point), checkpoints the batch into the
// database file, fsyncs the database, and resets the WAL. A crash anywhere in
// this sequence recovers to either the old or the new state, never a mix — the
// durable-prefix property the M0 crash campaign proves (doc 05 §10, doc 25 §3.4).
func (p *Pager) Commit() error {
	if p.readOnly {
		return ErrReadOnly
	}
	// Gather dirty data pages in id order for deterministic WAL layout.
	dirty := make([]*Frame, 0, len(p.pool))
	for _, f := range p.pool {
		if f.dirty {
			dirty = append(dirty, f)
		}
	}
	if len(dirty) == 0 && !p.headerDirty {
		return nil // nothing to do
	}
	sortFramesByID(dirty)

	// Stamp checksums on data pages before they are logged.
	for _, f := range dirty {
		checksumPage(f.Data)
	}

	// Build the WAL batch. The header page (page 0) goes last so that the commit
	// frame — the one whose presence makes the batch durable — carries the new
	// page count and change counter.
	frames := make([]wal.Frame, 0, len(dirty)+1)
	for _, f := range dirty {
		if f.id == 0 {
			continue // page 0 is appended explicitly below
		}
		frames = append(frames, wal.Frame{PageID: f.id, Image: f.Data})
	}
	p.header.ChangeCounter++
	page0 := make([]byte, p.pageSize)
	copy(page0, p.header.Marshal())
	frames = append(frames, wal.Frame{PageID: 0, Image: page0})

	if _, err := p.wal.Append(frames, true, p.header.PageCount); err != nil {
		return err
	}

	// Checkpoint: copy the committed images into the database file, timing each write for the page-write
	// latency distribution (doc 20 §4.2). The observe is a lock-free atomic add and runs under the engine
	// write lock the commit already holds, so it adds nothing to the read path.
	for _, fr := range frames {
		off := int64(fr.PageID) * int64(p.pageSize)
		start := time.Now()
		if _, err := p.db.WriteAt(fr.Image, off); err != nil {
			return err
		}
		if p.io != nil {
			p.io.ObservePageWrite(time.Since(start).Seconds())
		}
		// Attribute the write-back to its store (doc 20 §4.2). Page 0 is the file header, counted as
		// catalog; every other frame names its store in its header type.
		if fr.PageID == 0 {
			p.pagesWrittenByStore[storeCatalog].Add(1)
		} else {
			p.pagesWrittenByStore[storeOf(format.ReadHeader(fr.Image).Type)].Add(1)
		}
	}
	// Count the images written back, the page write-back volume the checkpoint metrics attribute to a
	// fold (doc 20 §5.4). Every commit checkpoints its frames straight into the file here.
	p.pagesWritten.Add(uint64(len(frames)))
	if err := p.db.Truncate(int64(p.header.PageCount) * int64(p.pageSize)); err != nil {
		return err
	}
	if p.sync >= wal.SyncFull {
		if err := p.db.Sync(); err != nil {
			return err
		}
	}

	// Reset the WAL for the next epoch with a fresh salt.
	if err := p.wal.Reset(p.nextSalt()); err != nil {
		return err
	}

	// Refresh the cached page-0 frame, if resident, and clear dirty flags.
	if f, ok := p.pool[0]; ok {
		copy(f.Data, page0)
		f.dirty = false
	}
	for _, f := range dirty {
		f.dirty = false
	}
	p.headerDirty = false
	return nil
}

// Rollback discards all uncommitted changes: it drops every dirty frame from the
// pool and reloads the header from the database file, which holds the last
// committed prefix (Commit writes committed images straight through). After
// Rollback, reads fault committed pages back in from disk, so the pager presents
// exactly the last-committed state. It is the page-level half of a transaction
// abort; callers that cache derived state above the pager must rebuild it from
// the rolled-back pager (the engine re-opens its stores).
func (p *Pager) Rollback() error {
	if p.readOnly {
		return nil
	}
	for id, f := range p.pool {
		if f.dirty {
			delete(p.pool, id)
			p.clock = removeFrame(p.clock, f)
		}
	}
	p.hand = 0
	p.headerDirty = false
	return p.loadHeader()
}

// sortFramesByID sorts frames ascending by page id (small n, insertion sort
// keeps it allocation-free and dependency-free).
func sortFramesByID(fs []*Frame) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j-1].id > fs[j].id; j-- {
			fs[j-1], fs[j] = fs[j], fs[j-1]
		}
	}
}

// Close flushes nothing (callers Commit explicitly), checks no pages are pinned,
// and closes the WAL and database files.
func (p *Pager) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	for _, f := range p.pool {
		if f.pin > 0 {
			return ErrPinned
		}
	}
	var firstErr error
	if p.wal != nil {
		if err := p.wal.Close(); err != nil {
			firstErr = err
		}
	}
	if err := p.db.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
