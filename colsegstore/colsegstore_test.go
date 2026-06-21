package colsegstore_test

import (
	"testing"

	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "colseg.gr"

func openPager(t *testing.T, fsys vfs.VFS) *pager.Pager {
	t.Helper()
	p, err := pager.Open(fsys, path, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func present(v value.Value) colseg.Cell { return colseg.Cell{Present: true, Value: v} }

var absent = colseg.Cell{}

// commit flushes the pager so a reopen sees the appended segments.
func commit(t *testing.T, p *pager.Pager) {
	t.Helper()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAppendAndGet appends three contiguous segments with a mix of present and
// absent cells and reads every position back, including the nulls.
func TestAppendAndGet(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	c, err := colsegstore.Create(p)
	if err != nil {
		t.Fatal(err)
	}

	// Three segments: ints 0..2, ints with a null at 4, ints 6..7.
	must(t, c.Append(0, value.TypeInt, []colseg.Cell{present(value.Int(10)), present(value.Int(11)), present(value.Int(12))}))
	must(t, c.Append(3, value.TypeInt, []colseg.Cell{present(value.Int(13)), absent, present(value.Int(15))}))
	must(t, c.Append(6, value.TypeInt, []colseg.Cell{present(value.Int(16)), present(value.Int(17))}))

	want := map[uint64]int64{0: 10, 1: 11, 2: 12, 3: 13, 5: 15, 6: 16, 7: 17}
	for pos := range uint64(8) {
		v, ok, err := c.Get(pos)
		if err != nil {
			t.Fatalf("get %d: %v", pos, err)
		}
		exp, present := want[pos]
		if ok != present {
			t.Fatalf("pos %d present = %v, want %v", pos, ok, present)
		}
		if ok {
			n, _ := v.AsInt()
			if n != exp {
				t.Fatalf("pos %d = %d, want %d", pos, n, exp)
			}
		}
	}
	if got, _ := c.Count(); got != 8 {
		t.Fatalf("Count = %d, want 8", got)
	}
}

// TestReopenSurvives proves the column reads back identically after a commit and a
// reopen from its persisted anchors, the durability the checkpoint relies on.
func TestReopenSurvives(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	c, err := colsegstore.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	must(t, c.Append(0, value.TypeString, []colseg.Cell{present(value.String("ada")), present(value.String("grace"))}))
	must(t, c.Append(2, value.TypeString, []colseg.Cell{present(value.String("lovelace"))}))

	dirHead, dirCount := c.DirHead(), c.DirCount()
	blobHead, blobLen := c.BlobHead(), c.BlobLen()
	commit(t, p)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2 := openPager(t, fsys)
	defer p2.Close()
	c2, err := colsegstore.Open(p2, dirHead, dirCount, blobHead, blobLen)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ada", "grace", "lovelace"}
	for pos, exp := range want {
		v, ok, err := c2.Get(uint64(pos))
		if err != nil || !ok {
			t.Fatalf("reopened get %d: ok=%v err=%v", pos, ok, err)
		}
		if s, _ := v.AsString(); s != exp {
			t.Fatalf("reopened pos %d = %q, want %q", pos, s, exp)
		}
	}
	if c2.SegmentCount() != 2 {
		t.Fatalf("reopened segment count = %d, want 2", c2.SegmentCount())
	}
}

// TestGetPastEnd proves a position past the covered range reads as absent rather
// than erroring.
func TestGetPastEnd(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	c, err := colsegstore.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	must(t, c.Append(0, value.TypeInt, []colseg.Cell{present(value.Int(1))}))

	if _, ok, err := c.Get(5); err != nil || ok {
		t.Fatalf("past-end get: ok=%v err=%v, want absent", ok, err)
	}
	// An empty column reads every position as absent.
	empty, err := colsegstore.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := empty.Get(0); err != nil || ok {
		t.Fatalf("empty get: ok=%v err=%v, want absent", ok, err)
	}
	if cnt, _ := empty.Count(); cnt != 0 {
		t.Fatalf("empty Count = %d, want 0", cnt)
	}
}

// TestRejectsOutOfOrder proves the contiguity invariant: a first segment not at 0,
// a gap, an overlap, and an empty segment are all rejected.
func TestRejectsOutOfOrder(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	c, err := colsegstore.Create(p)
	if err != nil {
		t.Fatal(err)
	}

	one := []colseg.Cell{present(value.Int(1))}
	if err := c.Append(1, value.TypeInt, one); err != colsegstore.ErrUnordered {
		t.Fatalf("first segment not at 0: err = %v, want ErrUnordered", err)
	}
	must(t, c.Append(0, value.TypeInt, []colseg.Cell{present(value.Int(1)), present(value.Int(2))}))
	if err := c.Append(3, value.TypeInt, one); err != colsegstore.ErrUnordered {
		t.Fatalf("gap: err = %v, want ErrUnordered", err)
	}
	if err := c.Append(1, value.TypeInt, one); err != colsegstore.ErrUnordered {
		t.Fatalf("overlap: err = %v, want ErrUnordered", err)
	}
	if err := c.Append(2, value.TypeInt, nil); err != colsegstore.ErrUnordered {
		t.Fatalf("empty segment: err = %v, want ErrUnordered", err)
	}
}

// TestBinarySearchAcrossManySegments proves find lands on the right segment when
// there are many, exercising the binary search rather than a scan.
func TestBinarySearchAcrossManySegments(t *testing.T) {
	fsys := vfs.NewMem()
	p := openPager(t, fsys)
	c, err := colsegstore.Create(p)
	if err != nil {
		t.Fatal(err)
	}

	// 50 segments of 4 positions each, value = position so the read is self-checking.
	const segs, width = 50, 4
	for s := range segs {
		first := uint64(s * width)
		cells := make([]colseg.Cell, width)
		for i := range cells {
			cells[i] = present(value.Int(int64(first) + int64(i)))
		}
		must(t, c.Append(first, value.TypeInt, cells))
	}
	for pos := range uint64(segs * width) {
		v, ok, err := c.Get(pos)
		if err != nil || !ok {
			t.Fatalf("get %d: ok=%v err=%v", pos, ok, err)
		}
		if n, _ := v.AsInt(); uint64(n) != pos {
			t.Fatalf("pos %d = %d, want %d", pos, n, pos)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
