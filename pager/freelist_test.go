package pager

import (
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// allocN allocates n data pages and returns their ids, committing once at the end.
func allocN(t *testing.T, p *Pager, n int) []format.PageID {
	t.Helper()
	ids := make([]format.PageID, n)
	for i := range ids {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = f.ID()
		p.MarkDirty(f)
		p.Unpin(f)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestFreeListReusesPages proves a freed page is handed back by the next alloc
// before the file grows: the page count does not climb while a freed page is
// available to reuse.
func TestFreeListReusesPages(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ids := allocN(t, p, 4)
	before := p.Header().PageCount

	// Free two of them, then allocate two: both should reuse, not grow the file.
	if err := p.FreePage(ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := p.FreePage(ids[3]); err != nil {
		t.Fatal(err)
	}
	reused := map[format.PageID]bool{ids[1]: true, ids[3]: true}
	for range 2 {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		if !reused[f.ID()] {
			t.Fatalf("alloc returned %d, want a reused page from %v", f.ID(), reused)
		}
		delete(reused, f.ID())
		p.Unpin(f)
	}
	if got := p.Header().PageCount; got != before {
		t.Fatalf("page count grew from %d to %d while reusing freed pages", before, got)
	}

	// With the free list drained, the next alloc grows the file again.
	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Header().PageCount; got != before+1 {
		t.Fatalf("page count = %d after draining free list, want %d", got, before+1)
	}
	p.Unpin(f)
}

// TestFreeCount proves FreeCount tracks the free list: zero on an empty list, the
// number of freed pages after frees, and back down as allocs reclaim them.
func TestFreeCount(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ids := allocN(t, p, 4)
	if n, err := p.FreeCount(); err != nil || n != 0 {
		t.Fatalf("FreeCount on empty list = %d, %v; want 0", n, err)
	}
	if err := p.FreePage(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.FreePage(ids[2]); err != nil {
		t.Fatal(err)
	}
	if n, err := p.FreeCount(); err != nil || n != 2 {
		t.Fatalf("FreeCount after 2 frees = %d, %v; want 2", n, err)
	}
	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	p.Unpin(f)
	if n, err := p.FreeCount(); err != nil || n != 1 {
		t.Fatalf("FreeCount after a reclaiming alloc = %d, %v; want 1", n, err)
	}
}

// TestFreeListSpillsAcrossPages frees more pages than one free-list page can hold,
// so the chain must grow to a second free-list page, then drains them all back.
func TestFreeListSpillsAcrossPages(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// One more than a single free-list page can index forces a second chain page.
	n := p.flCapacity() + 5
	ids := allocN(t, p, n)
	for _, id := range ids {
		if err := p.FreePage(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	// Every freed page reuses; none of the n allocs grows the file.
	freed := make(map[format.PageID]bool, n)
	for _, id := range ids {
		freed[id] = true
	}
	before := p.Header().PageCount
	for i := range n {
		f, err := p.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		if !freed[f.ID()] {
			t.Fatalf("alloc %d returned non-reused page %d", i, f.ID())
		}
		delete(freed, f.ID())
		p.Unpin(f)
	}
	if got := p.Header().PageCount; got != before {
		t.Fatalf("page count grew from %d to %d draining a spilled free list", before, got)
	}
	if len(freed) != 0 {
		t.Fatalf("%d freed pages were never reused", len(freed))
	}
}

// TestFreeListSurvivesReopen proves the free list is durable: pages freed and
// committed before a clean close are still reused after reopening the file.
func TestFreeListSurvivesReopen(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}

	ids := allocN(t, p, 3)
	if err := p.FreePage(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.FreePage(ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	want := map[format.PageID]bool{ids[0]: true, ids[2]: true}
	for range 2 {
		f, err := p2.AllocPage(format.PageTypeData)
		if err != nil {
			t.Fatal(err)
		}
		if !want[f.ID()] {
			t.Fatalf("after reopen alloc returned %d, want one of %v", f.ID(), want)
		}
		delete(want, f.ID())
		p2.Unpin(f)
	}
	if err := p2.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestFreeListRollback proves an aborted transaction's frees are undone: a page
// freed in a rolled-back transaction is not reused afterward.
func TestFreeListRollback(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ids := allocN(t, p, 2)

	// Free a page but roll the transaction back instead of committing.
	if err := p.FreePage(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.Rollback(); err != nil {
		t.Fatal(err)
	}

	// The free was undone, so the next alloc grows the file rather than reusing.
	before := p.Header().PageCount
	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID() == ids[0] {
		t.Fatal("rolled-back free was still reused")
	}
	if got := p.Header().PageCount; got != before+1 {
		t.Fatalf("page count = %d, want %d (alloc should have grown the file)", got, before+1)
	}
	p.Unpin(f)
}
