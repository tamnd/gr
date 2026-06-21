package store

import (
	"bytes"
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, "t.gr", pager.Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	format.PutU64(b, v)
	return b
}

// TestVectorRoundTrip writes more elements than fit on one page (forcing the
// chain to grow), commits, reopens, and reads every element back.
func TestVectorRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)

	v, err := CreateVector(p, 8, format.PageTypeNode)
	if err != nil {
		t.Fatal(err)
	}
	// Enough to span several pages.
	const n = 5000
	for i := range n {
		idx, err := v.Append(u64(uint64(i * 7)))
		if err != nil {
			t.Fatal(err)
		}
		if idx != i {
			t.Fatalf("Append returned index %d, want %d", idx, i)
		}
	}
	head, count := v.Head(), v.Count()
	if count != n {
		t.Fatalf("count = %d, want %d", count, n)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2 := openPager(t, fsys)
	defer p2.Close()
	v2, err := OpenVector(p2, head, 8, count)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 8)
	for i := range n {
		if err := v2.Get(i, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, u64(uint64(i*7))) {
			t.Fatalf("element %d = %x, want %x", i, got, u64(uint64(i*7)))
		}
	}
}

// TestVectorSet overwrites an element in place and reads it back after reopen.
func TestVectorSet(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	v, _ := CreateVector(p, 8, format.PageTypeNode)
	for i := range 100 {
		v.Append(u64(uint64(i)))
	}
	if err := v.Set(42, u64(0xDEAD)); err != nil {
		t.Fatal(err)
	}
	head, count := v.Head(), v.Count()
	p.Commit()
	p.Close()

	p2 := openPager(t, fsys)
	defer p2.Close()
	v2, _ := OpenVector(p2, head, 8, count)
	got := make([]byte, 8)
	v2.Get(42, got)
	if !bytes.Equal(got, u64(0xDEAD)) {
		t.Fatalf("Set not persisted: %x", got)
	}
}

func TestVectorStrideTooLarge(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()
	if _, err := CreateVector(p, p.PayloadSize()+1, format.PageTypeNode); err != ErrStrideTooLarge {
		t.Fatalf("want ErrStrideTooLarge, got %v", err)
	}
}

// TestLogRoundTrip appends byte runs that straddle page boundaries, commits,
// reopens, and reads arbitrary ranges back.
func TestLogRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)

	l, err := CreateLog(p, format.PageTypeCatalog)
	if err != nil {
		t.Fatal(err)
	}
	// Append chunks of varying size to force straddling.
	var want []byte
	offsets := make([]int, 0)
	for i := range 300 {
		chunk := bytes.Repeat([]byte{byte(i)}, (i%37)+1)
		off, err := l.Append(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if off != len(want) {
			t.Fatalf("append offset = %d, want %d", off, len(want))
		}
		offsets = append(offsets, off)
		want = append(want, chunk...)
	}
	head, length := l.Head(), l.Len()
	if length != len(want) {
		t.Fatalf("len = %d, want %d", length, len(want))
	}
	p.Commit()
	p.Close()

	p2 := openPager(t, fsys)
	defer p2.Close()
	l2, err := OpenLog(p2, head, length)
	if err != nil {
		t.Fatal(err)
	}
	// Read the whole stream and compare.
	got := make([]byte, length)
	if err := l2.Read(0, length, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("log content mismatch after reopen")
	}
	// Read a few individual ranges.
	for i := range 300 {
		clen := (i % 37) + 1
		r := make([]byte, clen)
		if err := l2.Read(offsets[i], clen, r); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(r, bytes.Repeat([]byte{byte(i)}, clen)) {
			t.Fatalf("range %d mismatch", i)
		}
	}
}

func TestLogReadOutOfRange(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()
	l, _ := CreateLog(p, format.PageTypeCatalog)
	l.Append([]byte("hi"))
	if err := l.Read(0, 5, make([]byte, 5)); err == nil {
		t.Fatal("want out-of-range error")
	}
}

// collectFreePages drains the pager's free list, returning the ids it hands back.
// It allocates until the page count grows, which signals the list is empty, then
// the final grown page is discarded by the caller's reuse check.
func freedPageSet(t *testing.T, p *pager.Pager, n int) map[format.PageID]bool {
	t.Helper()
	before := p.Header().PageCount
	got := map[format.PageID]bool{}
	for range n {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		got[f.ID()] = true
		p.Unpin(f)
	}
	if p.Header().PageCount != before {
		t.Fatalf("page count grew from %d to %d reusing freed pages", before, p.Header().PageCount)
	}
	return got
}

// TestVectorFreeReusesPages builds a multi-page vector, frees it, and proves the
// next allocations reuse exactly the freed pages without growing the file.
func TestVectorFreeReusesPages(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()

	v, err := CreateVector(p, 8, format.PageTypeNode)
	if err != nil {
		t.Fatal(err)
	}
	const n = 3000 // spans several pages
	for i := range n {
		if _, err := v.Append(u64(uint64(i))); err != nil {
			t.Fatal(err)
		}
	}
	pages := append([]format.PageID(nil), v.pages...)
	if len(pages) < 2 {
		t.Fatalf("vector only spans %d pages, want several", len(pages))
	}
	if err := v.Free(); err != nil {
		t.Fatal(err)
	}
	reused := freedPageSet(t, p, len(pages))
	for _, id := range pages {
		if !reused[id] {
			t.Fatalf("freed page %d was not reused", id)
		}
	}
}

// TestLogFreeReusesPages builds a multi-page log, frees it, and proves the next
// allocations reuse exactly the freed pages without growing the file.
func TestLogFreeReusesPages(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()

	l, err := CreateLog(p, format.PageTypeCatalog)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{0xab}, 1000)
	for range 50 { // spans several pages
		if _, err := l.Append(chunk); err != nil {
			t.Fatal(err)
		}
	}
	pages := append([]format.PageID(nil), l.pages...)
	if len(pages) < 2 {
		t.Fatalf("log only spans %d pages, want several", len(pages))
	}
	if err := l.Free(); err != nil {
		t.Fatal(err)
	}
	reused := freedPageSet(t, p, len(pages))
	for _, id := range pages {
		if !reused[id] {
			t.Fatalf("freed page %d was not reused", id)
		}
	}
}
