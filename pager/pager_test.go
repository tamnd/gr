package pager

import (
	"bytes"
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
	if got, ok := p.pool[pinned.ID()]; !ok || got != pinned {
		t.Fatal("pinned frame was evicted")
	}
	p.Unpin(pinned)
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
