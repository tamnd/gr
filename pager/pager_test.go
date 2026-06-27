package pager

import (
	"bytes"
	"sync"
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// fill returns a page-sized buffer whose payload encodes seed (so a torn or
// stale page is detectable) and whose header marks it a data page.
func fill(pageSize uint32, seed byte) []byte {
	b := make([]byte, pageSize)
	format.WriteHeader(b, format.PageHeader{Type: format.PageTypeData})
	for i := format.PayloadOffset(); i < int(pageSize)-format.ChecksumSize; i++ {
		b[i] = seed
	}
	return b
}

func TestPagerRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	ids := make([]format.PageID, n)
	for i := 0; i < n; i++ {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = f.ID()
		copy(f.Data, fill(p.PageSize(), byte(i+1)))
		p.MarkDirty(f)
		p.Unpin(f)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and read every page back.
	p2, err := Open(fsys, "t.gr", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	for i, id := range ids {
		f, err := p2.ReadPage(id)
		if err != nil {
			t.Fatalf("ReadPage(%d): %v", id, err)
		}
		want := fill(p2.PageSize(), byte(i+1))
		// The pager stamps the checksum trailer on commit; compare the body.
		if !bytes.Equal(f.Data[:len(f.Data)-format.ChecksumSize], want[:len(want)-format.ChecksumSize]) {
			t.Fatalf("page %d content mismatch after reopen", id)
		}
		p2.Unpin(f)
	}
}

// TestPagerPinningNeverEvicts is invariant 11 of doc 05 §10: a pinned frame is
// never evicted. We use a tiny pool, pin one page, then churn many others, and
// assert the pinned page's frame pointer is still the same object (never dropped
// and re-read).
func TestPagerPinningNeverEvicts(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{MaxPoolPages: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	pinned, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	copy(pinned.Data, fill(p.PageSize(), 0xAA))
	p.MarkDirty(pinned)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	// pinned stays pinned (we never Unpin it). Churn the pool well past capacity.
	for i := 0; i < 64; i++ {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		p.Unpin(f)
		if err := p.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// The pinned frame must still be resident and the same object.
	if got, ok := (*p.lookup.Load())[pinned.ID()]; !ok || got != pinned {
		t.Fatal("pinned frame was evicted")
	}
	p.Unpin(pinned)
}

// TestPoolStatsCountsHitsAndMisses proves the pool stats track the lookup outcomes the
// buffer-pool metrics expose: a read of a page already resident is a hit, and the first
// read of a page in a fresh pool faults it from disk as a miss that leaves one resident
// frame. The two cases run on two pagers over one committed file so the cold read is
// unambiguous: the second pager starts with an empty pool.
func TestPoolStatsCountsHitsAndMisses(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "stats.gr", Options{})
	if err != nil {
		t.Fatal(err)
	}

	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID()
	copy(f.Data, fill(p.PageSize(), 0x5A))
	p.MarkDirty(f)
	p.Unpin(f)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	// The freshly allocated page is still resident, so reading it back is a hit.
	before := p.PoolStats()
	rf, err := p.ReadPage(id)
	if err != nil {
		t.Fatal(err)
	}
	p.Unpin(rf)
	if s := p.PoolStats(); s.Hits != before.Hits+1 || s.Misses != before.Misses {
		t.Fatalf("after resident read = %+v, want one more hit than %+v", s, before)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh pager over the same file starts with an empty pool, so the first read of
	// the page faults it from disk: a miss that leaves exactly one resident frame.
	p2, err := Open(fsys, "stats.gr", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	rf2, err := p2.ReadPage(id)
	if err != nil {
		t.Fatal(err)
	}
	p2.Unpin(rf2)
	s := p2.PoolStats()
	if s.Hits != 0 || s.Misses != 1 {
		t.Fatalf("cold read stats = %+v, want 0 hits and 1 miss", s)
	}
	if s.Resident != 1 {
		t.Fatalf("resident frames = %d, want 1", s.Resident)
	}
	if want := int(p2.PageSize()); s.Bytes != want {
		t.Fatalf("memory bytes = %d, want one page (%d)", s.Bytes, want)
	}
}

// TestPagerConcurrentReads exercises the read path the way morsel-parallel
// execution will: many goroutines reading pages at once against one committed
// pager, a mix of pool hits (the same hot page) and cold faults (pages a small
// pool must evict and re-read). Without the buffer-pool lock this races on the
// pool map and the frame pin counts and the race detector fails it; with the lock
// every read returns the correct page body and no pin leaks. The pool is kept
// smaller than the page set so reads genuinely contend on eviction.
func TestPagerConcurrentReads(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{MaxPoolPages: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const n = 32
	ids := make([]format.PageID, n)
	for i := 0; i < n; i++ {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = f.ID()
		copy(f.Data, fill(p.PageSize(), byte(i+1)))
		p.MarkDirty(f)
		p.Unpin(f)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	const iters = 400
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Bias toward page 0 (a hot pool hit) but also touch a rotating cold
				// page so eviction and re-fault run under contention.
				idx := 0
				if i%2 == 1 {
					idx = (w*iters + i) % n
				}
				id := ids[idx]
				f, err := p.ReadPage(id)
				if err != nil {
					errs <- err
					return
				}
				want := fill(p.PageSize(), byte(idx+1))
				body := len(f.Data) - format.ChecksumSize
				if !bytes.Equal(f.Data[:body], want[:body]) {
					errs <- errMismatch(idx)
					p.Unpin(f)
					return
				}
				p.Unpin(f)
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// errMismatch names a page whose body did not match what was written to it.
func errMismatch(idx int) error {
	return &pageMismatch{idx}
}

type pageMismatch struct{ idx int }

func (e *pageMismatch) Error() string {
	return "concurrent read returned wrong body for page index " + itoa(e.idx)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestPagerRecoveredFlag confirms a reopen that redoes a committed-but-uncheckpointed
// WAL prefix reports Recovered, and a clean reopen does not. The clean commit protocol
// folds and resets the WAL on every commit, so the only way to leave a committed prefix
// for recovery is a crash between a commit's fsync and its checkpoint; we stage exactly
// that by appending a committed batch to the WAL directly, without folding it into the
// file (doc 20 §11.3, the open event's recovered flag).
func TestPagerRecoveredFlag(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "r.gr", Options{Sync: wal.SyncFull, SaltSeed: 7})
	if err != nil {
		t.Fatal(err)
	}
	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID()
	copy(f.Data, fill(p.PageSize(), 0x11))
	p.MarkDirty(f)
	p.Unpin(f)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	ps := p.PageSize()
	pageCount := p.Header().PageCount
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// A clean reopen replayed nothing, so it is not a recovery.
	clean, err := Open(fsys, "r.gr", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Recovered() {
		t.Errorf("clean reopen reported Recovered, want false")
	}
	if err := clean.Close(); err != nil {
		t.Fatal(err)
	}

	// Stage a torn commit: write a fresh committed image for the page straight into
	// the WAL and leave the database file untouched, the state a crash between fsync
	// and checkpoint leaves behind.
	newImage := fill(ps, 0x22)
	checksumPage(newImage)
	wf, err := fsys.Open("r.gr-wal", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wal.Open(wf, ps, wal.SyncFull, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append([]wal.Frame{{PageID: id, Image: newImage}}, true, uint64(pageCount)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: recovery redoes the committed frame into the file and reports it.
	p2, err := Open(fsys, "r.gr", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if !p2.Recovered() {
		t.Errorf("reopen after a torn commit did not report Recovered")
	}
	got, err := p2.ReadPage(id)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", id, err)
	}
	body := func(b []byte) []byte { return b[format.PayloadOffset() : len(b)-format.ChecksumSize] }
	if !bytes.Equal(body(got.Data), body(newImage)) {
		t.Errorf("recovered page content was not redone from the WAL")
	}
	p2.Unpin(got)
}

func TestPagerChecksumDetectsCorruption(t *testing.T) {
	fsys := vfs.NewMem()
	p, _ := Open(fsys, "t.gr", Options{})
	f, _ := p.AllocPage(format.PageTypeData)
	id := f.ID()
	copy(f.Data, fill(p.PageSize(), 0x5A))
	p.MarkDirty(f)
	p.Unpin(f)
	_ = p.Commit()
	_ = p.Close()

	// Corrupt one payload byte directly in the media.
	raw, _ := fsys.Open("t.gr", false)
	buf := make([]byte, 1)
	off := int64(id)*int64(p.PageSize()) + int64(format.PayloadOffset())
	_, _ = raw.ReadAt(buf, off)
	buf[0] ^= 0xFF
	_, _ = raw.WriteAt(buf, off)
	_ = raw.Close()

	p2, _ := Open(fsys, "t.gr", Options{})
	defer p2.Close()
	if _, err := p2.ReadPage(id); err != ErrBadChecksum {
		t.Fatalf("want ErrBadChecksum on corrupted page, got %v", err)
	}
}
