package pager

import (
	"bytes"
	"testing"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// TestCopyImage proves the physical-copy primitive writes every committed page and
// the result equals the on-disk file byte for byte.
func TestCopyImage(t *testing.T) {
	fsys := vfs.NewMem()
	p, err := Open(fsys, "t.gr", Options{Sync: wal.SyncFull})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	allocN(t, p, 6)
	pages := p.Header().PageCount

	var buf bytes.Buffer
	n, err := p.CopyImage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(pages) * int64(p.PageSize())
	if n != want {
		t.Errorf("CopyImage wrote %d bytes, want %d (%d pages)", n, want, pages)
	}

	// The image equals the database file.
	f, err := fsys.Open("t.gr", false)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	onDisk := make([]byte, want)
	if _, err := f.ReadAt(onDisk, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, buf.Bytes()) {
		t.Errorf("image differs from the file")
	}
	// And it is a valid header (the copy opens as a gr file).
	if _, err := format.Unmarshal(buf.Bytes()[:format.HeaderSize]); err != nil {
		t.Errorf("copied image has an invalid header: %v", err)
	}
}
