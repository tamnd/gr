package engine

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/vfs"
)

// TestNeighborReaderNoAllocPerCall pins the per-call no-allocation property of the
// neighbor read path, which the fused triangle count drives once per edge. After a
// checkpoint folds the adjacency delta into the base CSR, three things keep a
// NeighborsByPos into a reused buffer allocation-free: baseRun returns a zero-copy
// view of the cached run, appendDirs/appendTypes fill stack-backed arrays rather
// than returning heap slices, and the read guard no longer hands back an unlock
// closure. A regression that puts a per-call allocation back on the hot path (a
// run copy, a direction or type slice, a closure) shows up here as a non-zero
// allocs/op, with no timing and no loaded-box sensitivity.
func TestNeighborReaderNoAllocPerCall(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "alloc.gr")
	defer e.Close()

	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	person, _ := e.Intern(catalog.KindLabel, "Person")

	const spokes = 64
	tx, _ := e.Begin(true)
	hub, _ := tx.CreateNode([]Token{person})
	for i := 0; i < spokes; i++ {
		s, _ := tx.CreateNode([]Token{person})
		if _, err := tx.CreateRel(hub, s, knows); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Fold the delta into the base CSR so ExpandWith takes its zero-copy path, the
	// steady state a bulk-loaded benchmark graph reads in.
	if err := e.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()

	ar, ok := rx.(AdjacencyReader)
	if !ok {
		t.Fatal("read tx does not expose AdjacencyReader")
	}
	r := ar.NewNeighborReader()
	defer r.Close()

	buf := make([]PosNeighbor, 0, spokes)
	// Warm the cursor and the buffer once so first-touch growth is not counted.
	if got, err := r.NeighborsByPos(hub, knows, Outgoing, buf); err != nil {
		t.Fatal(err)
	} else if len(got) != spokes {
		t.Fatalf("degree: got %d want %d", len(got), spokes)
	}

	allocs := testing.AllocsPerRun(100, func() {
		got, err := r.NeighborsByPos(hub, knows, Outgoing, buf)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != spokes {
			t.Fatalf("degree: got %d want %d", len(got), spokes)
		}
		buf = got[:0]
	})
	if allocs != 0 {
		t.Fatalf("reader NeighborsByPos allocated %.0f objects/call; the per-edge read path must be allocation-free", allocs)
	}
}
