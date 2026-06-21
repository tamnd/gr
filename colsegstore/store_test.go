package colsegstore_test

import (
	"testing"

	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// TestStoreManyColumns appends segments to several key tokens and reads each back
// independently, proving the columns do not interfere.
func TestStoreManyColumns(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	s, err := colsegstore.CreateStore(p)
	if err != nil {
		t.Fatal(err)
	}

	// key 0 = ages (int), two contiguous segments; key 3 = names (string), one
	// segment; key 1 is left with no column at all.
	must(t, s.Append(0, 0, value.TypeInt, []colseg.Cell{present(value.Int(30)), present(value.Int(31))}))
	must(t, s.Append(0, 2, value.TypeInt, []colseg.Cell{present(value.Int(32))}))
	must(t, s.Append(3, 0, value.TypeString, []colseg.Cell{present(value.String("ada"))}))

	for pos, exp := range map[uint64]int64{0: 30, 1: 31, 2: 32} {
		v, ok, err := s.Get(0, pos)
		if err != nil || !ok {
			t.Fatalf("key 0 pos %d: ok=%v err=%v", pos, ok, err)
		}
		if n, _ := v.AsInt(); n != exp {
			t.Fatalf("key 0 pos %d = %d, want %d", pos, n, exp)
		}
	}
	v, ok, err := s.Get(3, 0)
	if err != nil || !ok {
		t.Fatalf("key 3: ok=%v err=%v", ok, err)
	}
	if str, _ := v.AsString(); str != "ada" {
		t.Fatalf("key 3 = %q, want ada", str)
	}

	// key 1 has a directory cell (the directory grew past it to reach key 3) but no
	// column, and key 9 is past the directory entirely; both read as absent.
	if _, ok, err := s.Get(1, 0); err != nil || ok {
		t.Fatalf("key 1: ok=%v err=%v, want absent", ok, err)
	}
	if _, ok, err := s.Get(9, 0); err != nil || ok {
		t.Fatalf("key 9: ok=%v err=%v, want absent", ok, err)
	}
}

// TestStoreReopen proves the whole store reopens from its directory anchors and
// reads every column back, the per-column anchors riding in the directory cells.
func TestStoreReopen(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	s, err := colsegstore.CreateStore(p)
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.Append(0, 0, value.TypeInt, []colseg.Cell{present(value.Int(7))}))
	must(t, s.Append(2, 0, value.TypeFloat, []colseg.Cell{present(value.Float(2.5)), present(value.Float(3.5))}))

	dirHead, dirCount := s.DirHead(), s.DirCount()
	commit(t, p)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2 := openPager(t, fsys)
	defer p2.Close()
	s2, err := colsegstore.OpenStore(p2, dirHead, dirCount)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s2.Get(0, 0); err != nil || !ok {
		t.Fatalf("reopened key 0: ok=%v err=%v", ok, err)
	} else if n, _ := v.AsInt(); n != 7 {
		t.Fatalf("reopened key 0 = %d, want 7", n)
	}
	if v, ok, err := s2.Get(2, 1); err != nil || !ok {
		t.Fatalf("reopened key 2 pos 1: ok=%v err=%v", ok, err)
	} else if f, _ := v.AsFloat(); f != 3.5 {
		t.Fatalf("reopened key 2 pos 1 = %v, want 3.5", f)
	}
	if got := len(s2.Keys()); got != 3 {
		t.Fatalf("reopened key count = %d, want 3", got)
	}
}

// TestStoreFreeReturnsPages proves Free returns the store's pages to the pager's
// free list: after freeing a populated store the next allocation reuses a page
// rather than growing the file.
func TestStoreFreeReturnsPages(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	defer p.Close()

	s, err := colsegstore.CreateStore(p)
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.Append(0, 0, value.TypeInt, []colseg.Cell{present(value.Int(1)), present(value.Int(2))}))
	must(t, s.Append(0, 2, value.TypeInt, []colseg.Cell{present(value.Int(3))}))
	must(t, s.Append(2, 0, value.TypeString, []colseg.Cell{present(value.String("x"))}))
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := s.Free(); err != nil {
		t.Fatal(err)
	}
	before := p.Header().PageCount
	f, err := p.AllocPage(format.PageTypeData)
	if err != nil {
		t.Fatal(err)
	}
	if p.Header().PageCount != before {
		t.Fatalf("page count grew from %d to %d after freeing a store", before, p.Header().PageCount)
	}
	p.Unpin(f)
}
