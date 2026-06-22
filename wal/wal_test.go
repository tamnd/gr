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

func TestWALStats(t *testing.T) {
	fsys := vfs.NewMem()
	w, _ := openWAL(t, fsys)

	// A fresh WAL has its header on disk and nothing appended.
	st := w.Stats()
	if st.BytesWritten != 0 || st.FramesPage != 0 || st.FramesCommit != 0 {
		t.Fatalf("fresh stats = %+v, want all-zero counters", st)
	}
	if st.SizeBytes != walHeaderSize {
		t.Fatalf("fresh size = %d, want header size %d", st.SizeBytes, walHeaderSize)
	}

	// A commit batch of two frames: the last is the commit frame, the first a page frame.
	if _, err := w.Append([]Frame{
		{PageID: 1, Image: page(0x11)},
		{PageID: 0, Image: page(0x00)},
	}, true, 2); err != nil {
		t.Fatal(err)
	}
	st = w.Stats()
	if st.FramesPage != 1 || st.FramesCommit != 1 {
		t.Fatalf("after a two-frame commit, frames = page %d commit %d, want 1 and 1", st.FramesPage, st.FramesCommit)
	}
	if want := uint64(2 * w.frameSize()); st.BytesWritten != want {
		t.Fatalf("bytes written = %d, want %d (two frames)", st.BytesWritten, want)
	}
	if want := uint64(walHeaderSize) + uint64(2*w.frameSize()); st.SizeBytes != want {
		t.Fatalf("size after append = %d, want %d", st.SizeBytes, want)
	}

	// Reset truncates the WAL back to the header, so the size falls but the cumulative counters hold.
	if err := w.Reset(123); err != nil {
		t.Fatal(err)
	}
	st = w.Stats()
	if st.SizeBytes != walHeaderSize {
		t.Fatalf("size after reset = %d, want header size %d", st.SizeBytes, walHeaderSize)
	}
	if st.BytesWritten == 0 || st.FramesCommit == 0 {
		t.Fatalf("cumulative counters reset to %+v, want them to hold across a reset", st)
	}
}

func TestWALFsyncMetrics(t *testing.T) {
	fsys := vfs.NewMem()
	w, _ := openWAL(t, fsys)

	// Init does not fsync, so a fresh WAL has issued no barriers and buffered no samples.
	if st := w.Stats(); st.FsyncTotal != 0 || st.FsyncErrors != 0 {
		t.Fatalf("fresh fsync counts = total %d errors %d, want 0 and 0", st.FsyncTotal, st.FsyncErrors)
	}
	if d := w.DrainFsyncDurations(); d != nil {
		t.Fatalf("fresh drain = %v, want nil", d)
	}

	// A commit fsyncs once (the WAL opened at SyncFull), so the count rises and one sample buffers.
	if _, err := w.Append([]Frame{{PageID: 1, Image: page(0x11)}}, true, 2); err != nil {
		t.Fatal(err)
	}
	if st := w.Stats(); st.FsyncTotal != 1 || st.FsyncErrors != 0 {
		t.Fatalf("after one commit, fsync counts = total %d errors %d, want 1 and 0", st.FsyncTotal, st.FsyncErrors)
	}

	// Reset fsyncs again, so by the drain two barriers have run and two samples wait.
	if err := w.Reset(7); err != nil {
		t.Fatal(err)
	}
	if st := w.Stats(); st.FsyncTotal != 2 {
		t.Fatalf("after commit then reset, fsync total = %d, want 2", st.FsyncTotal)
	}
	d := w.DrainFsyncDurations()
	if len(d) != 2 {
		t.Fatalf("drained %d samples, want 2", len(d))
	}
	for _, s := range d {
		if s < 0 {
			t.Fatalf("negative fsync duration %v", s)
		}
	}

	// The drain clears the buffer, so a second drain with no fsync in between returns nothing, but the
	// cumulative count holds: the counter is monotonic, only the sample buffer empties.
	if d := w.DrainFsyncDurations(); d != nil {
		t.Fatalf("second drain = %v, want nil after the buffer cleared", d)
	}
	if st := w.Stats(); st.FsyncTotal != 2 {
		t.Fatalf("fsync total after drain = %d, want it to hold at 2", st.FsyncTotal)
	}
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
