package wal

import (
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
)

const ps = 4096

func page(b byte) []byte {
	p := make([]byte, ps)
	for i := range p {
		p[i] = b
	}
	return p
}

func openWAL(t *testing.T, fsys vfs.VFS) (*WAL, vfs.File) {
	t.Helper()
	f, err := fsys.Open("x-wal", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(f, ps, SyncFull, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Init(); err != nil {
		t.Fatal(err)
	}
	return w, f
}

func TestWALAppendAndRecover(t *testing.T) {
	fsys := vfs.NewMem()
	w, f := openWAL(t, fsys)
	if _, err := w.Append([]Frame{
		{PageID: 1, Image: page(0x11)},
		{PageID: 2, Image: page(0x22)},
	}, true, 3); err != nil {
		t.Fatal(err)
	}

	res, err := Recover(f, ps)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || len(res.Frames) != 2 || res.DBPages != 3 {
		t.Fatalf("recover = %+v", res)
	}
	if res.Frames[0].PageID != 1 || res.Frames[1].PageID != 2 {
		t.Fatal("frame order")
	}
}

// TestWALUncommittedDropped: frames after the last commit frame are not recovered.
func TestWALUncommittedDropped(t *testing.T) {
	fsys := vfs.NewMem()
	w, f := openWAL(t, fsys)
	// One committed transaction, then an uncommitted frame.
	w.Append([]Frame{{PageID: 1, Image: page(0xAA)}}, true, 2)
	w.Append([]Frame{{PageID: 2, Image: page(0xBB)}}, false, 0)

	res, err := Recover(f, ps)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || len(res.Frames) != 1 || res.Frames[0].PageID != 1 {
		t.Fatalf("uncommitted frame should be dropped, got %+v", res)
	}
}

// TestWALTornFrameEndsRecovery: a frame whose checksum is wrong ends the valid
// region, so a commit after it does not count (durable prefix).
func TestWALTornFrameEndsRecovery(t *testing.T) {
	fsys := vfs.NewMem()
	w, f := openWAL(t, fsys)
	w.Append([]Frame{{PageID: 1, Image: page(0x01)}}, true, 2)
	w.Append([]Frame{{PageID: 2, Image: page(0x02)}}, true, 3)

	// Corrupt the second frame's image in the media.
	off := int64(walHeaderSize) + frameSize2() + frameHeaderLen
	bad := []byte{0xFF}
	cur := make([]byte, 1)
	f.ReadAt(cur, off)
	bad[0] = cur[0] ^ 0xFF
	f.WriteAt(bad, off)

	res, err := Recover(f, ps)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first commit survives.
	if !res.Committed || len(res.Frames) != 1 || res.DBPages != 2 {
		t.Fatalf("torn frame should cap recovery at first commit, got %+v", res)
	}
}

func frameSize2() int64 { return int64(frameHeaderLen) + int64(ps) }

func TestWALResetRejectsStaleFrames(t *testing.T) {
	fsys := vfs.NewMem()
	w, f := openWAL(t, fsys)
	w.Append([]Frame{{PageID: 1, Image: page(0x01)}}, true, 2)
	if err := w.Reset(0xCAFEBABE); err != nil {
		t.Fatal(err)
	}
	// After reset the WAL is empty (header only); recovery finds nothing.
	res, err := Recover(f, ps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed {
		t.Fatalf("reset WAL should recover nothing, got %+v", res)
	}
	_ = format.NoPage
}

func TestRecoverEmptyWAL(t *testing.T) {
	fsys := vfs.NewMem()
	f, _ := fsys.Open("empty-wal", true)
	res, err := Recover(f, ps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed {
		t.Fatal("empty WAL recovered something")
	}
}
