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

// TestNeighborReaderBothDirectionMerge checks the undirected read path: a Both
// request reads the outgoing and incoming runs (each sorted by dense position) and
// merges them into one position-sorted slice in linear time, instead of
// concatenating and sorting. It pins both halves of that change: the merged result
// is correct (every out and in neighbor present, ordered by position, none dropped),
// and the merge stays allocation-free per call once the reader's scratch has grown,
// the same property the directed path holds. The undirected triangle count drives
// exactly this Both path once per edge.
func TestNeighborReaderBothDirectionMerge(t *testing.T) {
	fsys := vfs.NewMem()
	e := openDisk(t, fsys, "both.gr")
	defer e.Close()

	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	person, _ := e.Intern(catalog.KindLabel, "Person")

	const outs = 40
	const ins = 24
	tx, _ := e.Begin(true)
	hub, _ := tx.CreateNode([]Token{person})
	for i := 0; i < outs; i++ {
		s, _ := tx.CreateNode([]Token{person})
		if _, err := tx.CreateRel(hub, s, knows); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < ins; i++ {
		s, _ := tx.CreateNode([]Token{person})
		if _, err := tx.CreateRel(s, hub, knows); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
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

	buf := make([]PosNeighbor, 0, outs+ins)
	got, err := r.NeighborsByPos(hub, knows, Both, buf)
	if err != nil {
		t.Fatal(err)
	}
	// Every directed edge on the hub, both ways, appears exactly once.
	if len(got) != outs+ins {
		t.Fatalf("Both degree: got %d want %d", len(got), outs+ins)
	}
	// The merge must leave the result sorted by dense position, the invariant the
	// merge-intersection in the triangle count relies on.
	for i := 1; i < len(got); i++ {
		if got[i-1].Pos > got[i].Pos {
			t.Fatalf("Both result not sorted by position at %d: %d > %d", i, got[i-1].Pos, got[i].Pos)
		}
	}

	allocs := testing.AllocsPerRun(100, func() {
		got, err := r.NeighborsByPos(hub, knows, Both, buf)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != outs+ins {
			t.Fatalf("Both degree: got %d want %d", len(got), outs+ins)
		}
		buf = got[:0]
	})
	if allocs != 0 {
		t.Fatalf("reader Both NeighborsByPos allocated %.0f objects/call; the undirected merge must be allocation-free", allocs)
	}
}
