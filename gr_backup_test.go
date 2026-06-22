package gr

import (
	"bytes"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// TestDBBackup confirms db.Backup writes an image that reopens as a logically-equal
// database, and that the image is byte-identical to the source file (doc 17 §6.13).
func TestDBBackup(t *testing.T) {
	mem := vfs.NewMem()
	db, err := Open("src.gr", Options{VFS: mem})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE (a:Person {name:'Ada'})-[:KNOWS]->(b:Person {name:'Lin'})", nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := db.Backup(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if int64(buf.Len()) != n {
		t.Errorf("Backup returned %d bytes but wrote %d", n, buf.Len())
	}
	_ = db.Close()

	// The image equals the on-disk source byte for byte (Commit checkpoints into the
	// main file, so the file is the committed image the backup copies).
	f, err := mem.Open("src.gr", false)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := f.Size()
	onDisk := make([]byte, size)
	if _, err := f.ReadAt(onDisk, 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if !bytes.Equal(onDisk, buf.Bytes()) {
		t.Errorf("backup image differs from the source file: file %d bytes, image %d bytes", len(onDisk), buf.Len())
	}

	// The image reopens as the same graph.
	if err := writeFileMem(mem, "copy.gr", buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	cp, err := Open("copy.gr", Options{VFS: mem})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cp.Close() }()
	info, err := cp.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Nodes != 2 || info.Relationships != 1 {
		t.Errorf("restored image = %d nodes, %d rels; want 2, 1", info.Nodes, info.Relationships)
	}
}

// TestDBBackupClosed confirms Backup on a closed database reports ErrClosed.
func TestDBBackupClosed(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := db.Backup(&bytes.Buffer{}); err != ErrClosed {
		t.Errorf("Backup after close = %v, want ErrClosed", err)
	}
}

// writeFileMem writes the whole of b to name in the memory VFS.
func writeFileMem(mem vfs.VFS, name string, b []byte) error {
	f, err := mem.Open(name, true)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(b, 0); err != nil {
		return err
	}
	return f.Sync()
}
