package gr

import (
	"testing"

	"github.com/tamnd/gr/vfs"
)

// FuzzFileFormat opens arbitrary bytes as a .gr file and runs a read query
// followed by db.Check(CheckFull). The property: no panic, no out-of-bounds
// access, no corrupted in-memory state. A clean error from Open or Exec is
// acceptable (doc 23 §7.6).
func FuzzFileFormat(f *testing.F) {
	// Seed corpus: a minimal valid .gr file, a zeroed file, and a truncated file.
	seeds := buildFuzzSeeds(f)
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, fileBytes []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("FuzzFileFormat panicked (len=%d): %v", len(fileBytes), r)
			}
		}()

		mem := vfs.NewMem()
		if err := writeMemFile(mem, "fuzz.gr", fileBytes); err != nil {
			return // VFS write error: not a format bug
		}

		db, err := Open("fuzz.gr", Options{VFS: mem, ReadOnly: true})
		if err != nil {
			return // clean rejection of a corrupt or truncated file
		}
		defer func() { _ = db.Close() }()

		// Try a simple read query — ignore errors (e.g. "corrupt page" from the
		// query executor is a clean error, not a crash).
		_, _ = db.Exec("MATCH (n) RETURN n", nil)

		// The integrity checker must also not panic on whatever state the fuzzed
		// file produced.
		_, _ = db.Check(CheckFull)
	})
}

// buildFuzzSeeds returns byte slices to use as initial fuzz seeds.
// These are built by writing a real (valid) database to an in-memory VFS and
// then snapshotting its raw bytes, plus a handful of manually crafted cases.
func buildFuzzSeeds(f *testing.F) [][]byte {
	f.Helper()

	var seeds [][]byte

	// Seed 1: valid empty database.
	{
		mem := vfs.NewMem()
		db, err := Open("seed.gr", Options{VFS: mem})
		if err == nil {
			_ = db.Close()
			if b, err2 := readMemFile(mem, "seed.gr"); err2 == nil {
				seeds = append(seeds, b)
			}
		}
	}

	// Seed 2: database with a few nodes and a relationship.
	{
		mem := vfs.NewMem()
		db, err := Open("seed2.gr", Options{VFS: mem})
		if err == nil {
			_, _ = db.Exec("CREATE (a:Person {name:'Alice'})-[:KNOWS]->(b:Person {name:'Bob'})", nil)
			_ = db.Close()
			if b, err2 := readMemFile(mem, "seed2.gr"); err2 == nil {
				seeds = append(seeds, b)
			}
		}
	}

	// Seed 3: empty byte slice (completely invalid).
	seeds = append(seeds, []byte{})

	// Seed 4: eight zero bytes (shorter than the file header).
	seeds = append(seeds, make([]byte, 8))

	// Seed 5: 4096 zero bytes (one page of zeros).
	seeds = append(seeds, make([]byte, 4096))

	return seeds
}

// writeMemFile writes raw bytes into a named file in mem.
func writeMemFile(mem *vfs.Mem, name string, data []byte) error {
	f, err := mem.Open(name, true)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readMemFile reads all bytes from a named file in mem.
func readMemFile(mem *vfs.Mem, name string) ([]byte, error) {
	f, err := mem.Open(name, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sz, err := f.Size()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sz)
	if _, err := f.ReadAt(buf, 0); err != nil && sz > 0 {
		return nil, err
	}
	return buf, nil
}
