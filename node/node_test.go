package node

import (
	"slices"
	"testing"

	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "node.gr"

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNodeRoundTrip creates nodes with varying label sets, mutates and deletes
// some, commits, reopens, and verifies everything replayed.
func TestNodeRoundTrip(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	secs, _ := store.CreateSections(p)
	s, err := Create(p, secs)
	if err != nil {
		t.Fatal(err)
	}

	p0, _ := s.Create([]uint32{1, 3, 7})
	p1, _ := s.Create(nil) // no labels
	p2, _ := s.Create([]uint32{2})
	if p0 != 0 || p1 != 1 || p2 != 2 {
		t.Fatalf("positions = %d,%d,%d", p0, p1, p2)
	}
	if err := s.SetLabels(p2, []uint32{2, 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(p1); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2db := openPager(t, fsys)
	defer p2db.Close()
	secs2, _ := store.OpenSections(p2db)
	s2, err := Open(p2db, secs2)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := s2.Labels(p0); err != nil || !slices.Equal(l, []uint32{1, 3, 7}) {
		t.Fatalf("labels(p0) = %v, %v", l, err)
	}
	if l, err := s2.Labels(p2); err != nil || !slices.Equal(l, []uint32{2, 5}) {
		t.Fatalf("labels(p2) = %v, %v", l, err)
	}
	if s2.Exists(p1) {
		t.Fatal("deleted node still exists after reopen")
	}
	if _, err := s2.Labels(p1); err != ErrNoSuchNode {
		t.Fatalf("Labels on deleted = %v, want ErrNoSuchNode", err)
	}
	if has, err := s2.HasLabel(p0, 3); err != nil || !has {
		t.Fatalf("HasLabel(p0,3) = %v,%v", has, err)
	}
	if has, _ := s2.HasLabel(p0, 99); has {
		t.Fatal("HasLabel(p0,99) should be false")
	}
}

func TestNodeNoSuch(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()
	secs, _ := store.CreateSections(p)
	s, _ := Create(p, secs)
	if _, err := s.Labels(42); err != ErrNoSuchNode {
		t.Fatalf("Labels(42) = %v, want ErrNoSuchNode", err)
	}
	if s.Exists(42) {
		t.Fatal("nonexistent node reported existing")
	}
}
