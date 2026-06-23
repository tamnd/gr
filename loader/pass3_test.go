package loader

import (
	"io"
	"strings"
	"testing"
)

// --- CSRBuilder unit tests ---

func TestCSRBuilderEmpty(t *testing.T) {
	b := newCSRBuilder(3)
	b.PrefixSum()
	if b.EdgeCount() != 0 {
		t.Errorf("EdgeCount: got %d, want 0", b.EdgeCount())
	}
	off := b.Offsets()
	if len(off) != 4 {
		t.Fatalf("Offsets len: got %d, want 4", len(off))
	}
	for i, v := range off {
		if v != 0 {
			t.Errorf("Offsets[%d]: got %d, want 0", i, v)
		}
	}
}

func TestCSRBuilderSingleEdge(t *testing.T) {
	// 3 nodes: edge 0→2.
	b := newCSRBuilder(3)
	b.Count(0) // out-degree of node 0 = 1
	b.PrefixSum()
	b.Scatter(0, 2, 99)
	b.SortWithinRuns()

	off := b.Offsets() // [0, 1, 1, 1]
	nbr := b.Neighbors()
	eid := b.Edges()

	if off[0] != 0 || off[1] != 1 || off[2] != 1 || off[3] != 1 {
		t.Errorf("Offsets: %v, want [0 1 1 1]", off)
	}
	if len(nbr) != 1 || nbr[0] != 2 {
		t.Errorf("Neighbors: %v, want [2]", nbr)
	}
	if len(eid) != 1 || eid[0] != 99 {
		t.Errorf("Edges: %v, want [99]", eid)
	}
}

func TestCSRBuilderMultipleEdges(t *testing.T) {
	// 4 nodes; edges: 0→3, 0→1, 2→0, 2→1, 3→2.
	edges := [][2]uint64{{0, 3}, {0, 1}, {2, 0}, {2, 1}, {3, 2}}
	b := newCSRBuilder(4)
	for _, e := range edges {
		b.Count(e[0])
	}
	b.PrefixSum()
	for i, e := range edges {
		b.Scatter(e[0], e[1], uint64(i))
	}
	b.SortWithinRuns()

	off := b.Offsets() // node 0: 2, node 1: 0, node 2: 2, node 3: 1 → [0,2,2,4,5]
	if len(off) != 5 {
		t.Fatalf("Offsets len: %d", len(off))
	}
	wantOff := []uint64{0, 2, 2, 4, 5}
	for i, w := range wantOff {
		if off[i] != w {
			t.Errorf("Offsets[%d]: got %d, want %d", i, off[i], w)
		}
	}

	// Node 0's run: neighbors 1 and 3 (sorted).
	nbr := b.Neighbors()
	run0 := nbr[off[0]:off[1]]
	if len(run0) != 2 || run0[0] != 1 || run0[1] != 3 {
		t.Errorf("node 0 run: %v, want [1 3]", run0)
	}
	// Node 1 has no out-edges.
	run1 := nbr[off[1]:off[2]]
	if len(run1) != 0 {
		t.Errorf("node 1 run: %v, want []", run1)
	}
	// Node 2's run: neighbors 0 and 1 (sorted).
	run2 := nbr[off[2]:off[3]]
	if len(run2) != 2 || run2[0] != 0 || run2[1] != 1 {
		t.Errorf("node 2 run: %v, want [0 1]", run2)
	}
}

func TestCSRBuilderSelfLoop(t *testing.T) {
	// Self-loop: 1→1.
	b := newCSRBuilder(2)
	b.Count(1)
	b.PrefixSum()
	b.Scatter(1, 1, 0)
	b.SortWithinRuns()

	off := b.Offsets() // [0, 0, 1]
	nbr := b.Neighbors()
	if off[2] != 1 {
		t.Errorf("Offsets[2]: got %d, want 1", off[2])
	}
	if len(nbr) != 1 || nbr[0] != 1 {
		t.Errorf("Neighbors: %v, want [1]", nbr)
	}
}

// --- Pass 3 integration tests ---

// runPass13 runs passes 1+3 over the given node and relationship CSVs and
// returns the loader and fileBuilder for inspection.
func runPass13(t *testing.T, nodeCSV, relCSV string) (*Loader, *fileBuilder) {
	t.Helper()
	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		Relationships: []RelSource{
			{readers: []io.Reader{strings.NewReader(relCSV)}},
		},
	})
	fb, err := l.Pass3BuildCSRFull(memFS(), "test.gr")
	if err != nil {
		t.Fatalf("Pass3BuildCSRFull: %v", err)
	}
	return l, fb
}

func TestPass3SimpleForwardCSR(t *testing.T) {
	// 3 Person nodes; 2 KNOWS relationships: p1→p2, p1→p3.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\np3,Person\n"
	relCSV := ":START_ID(p),:END_ID(p),:TYPE\np1,p2,KNOWS\np1,p3,KNOWS\n"

	l, fb := runPass13(t, nodeCSV, relCSV)
	defer fb.Close()

	// Expect 2 relationships.
	if l.Stats().Rels != 2 {
		t.Errorf("Rels: got %d, want 2", l.Stats().Rels)
	}

	knowsTok := uint32(l.catalog.RelTypeToken("KNOWS"))
	fwd, ok := fb.relCSR[csrKey{knowsTok, csrFwd}]
	if !ok {
		t.Fatal("no forward CSR for KNOWS")
	}

	// p1 (denseID=0) has out-degree 2.
	off := fwd.Offsets()
	if off[1]-off[0] != 2 {
		t.Errorf("p1 out-degree: got %d, want 2", off[1]-off[0])
	}
	// p2 (denseID=1) and p3 (denseID=2) have out-degree 0.
	if off[2]-off[1] != 0 {
		t.Errorf("p2 out-degree: got %d, want 0", off[2]-off[1])
	}
	// p1's neighbors should be [1, 2] (sorted: p2=1, p3=2).
	nbr := fwd.Neighbors()
	run := nbr[off[0]:off[1]]
	if len(run) != 2 || run[0] != 1 || run[1] != 2 {
		t.Errorf("p1 fwd neighbors: %v, want [1 2]", run)
	}
}

func TestPass3BackwardCSR(t *testing.T) {
	// p1→p2 and p3→p2: both point to p2 (backward: p2 has in-degree 2).
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\np3,Person\n"
	relCSV := ":START_ID(p),:END_ID(p),:TYPE\np1,p2,KNOWS\np3,p2,KNOWS\n"

	l, fb := runPass13(t, nodeCSV, relCSV)
	defer fb.Close()

	knowsTok := uint32(l.catalog.RelTypeToken("KNOWS"))
	bwd, ok := fb.relCSR[csrKey{knowsTok, csrBwd}]
	if !ok {
		t.Fatal("no backward CSR for KNOWS")
	}

	// p2 (denseID=1) has in-degree 2.
	off := bwd.Offsets()
	if off[2]-off[1] != 2 {
		t.Errorf("p2 in-degree: got %d, want 2", off[2]-off[1])
	}
	// p2's backward neighbors should include p1 (0) and p3 (2).
	nbr := bwd.Neighbors()
	run := nbr[off[1]:off[2]]
	if len(run) != 2 {
		t.Fatalf("p2 bwd neighbors len: %d, want 2", len(run))
	}
	// Sorted: p1=0, p3=2.
	if run[0] != 0 || run[1] != 2 {
		t.Errorf("p2 bwd neighbors: %v, want [0 2]", run)
	}
}

func TestPass3DanglingSkip(t *testing.T) {
	// Relationship referencing nonexistent node "px" — should be skipped.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\n"
	relCSV := ":START_ID(p),:END_ID(p),:TYPE\np1,px,KNOWS\np1,p2,KNOWS\n"

	l, fb := runPass13(t, nodeCSV, relCSV)
	defer fb.Close()

	if l.Stats().DanglingRels != 1 {
		t.Errorf("DanglingRels: got %d, want 1", l.Stats().DanglingRels)
	}
	if l.Stats().Rels != 1 {
		t.Errorf("Rels: got %d, want 1", l.Stats().Rels)
	}
}

func TestPass3EdgeIDsInOrder(t *testing.T) {
	// Three KNOWS edges; edge ids should be 0, 1, 2 in input order.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\np3,Person\n"
	relCSV := ":START_ID(p),:END_ID(p),:TYPE\np1,p2,KNOWS\np2,p3,KNOWS\np1,p3,KNOWS\n"

	l, fb := runPass13(t, nodeCSV, relCSV)
	defer fb.Close()

	knowsTok := uint32(l.catalog.RelTypeToken("KNOWS"))
	// Total edge count should be 3.
	if fb.edgeCnt[knowsTok] != 3 {
		t.Errorf("edgeCnt[KNOWS]: got %d, want 3", fb.edgeCnt[knowsTok])
	}
}

func TestPass3MultipleRelTypes(t *testing.T) {
	// Two relationship types: KNOWS and LIKES.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\n"
	relCSV := ":START_ID(p),:END_ID(p),:TYPE\np1,p2,KNOWS\np2,p1,LIKES\n"

	l, fb := runPass13(t, nodeCSV, relCSV)
	defer fb.Close()

	knowsTok := uint32(l.catalog.RelTypeToken("KNOWS"))
	likesTok := uint32(l.catalog.RelTypeToken("LIKES"))

	// Both types should have their own CSR.
	if _, ok := fb.relCSR[csrKey{knowsTok, csrFwd}]; !ok {
		t.Error("missing KNOWS forward CSR")
	}
	if _, ok := fb.relCSR[csrKey{likesTok, csrFwd}]; !ok {
		t.Error("missing LIKES forward CSR")
	}
	// Edge id counters are per-type; each type has 1 edge.
	if fb.edgeCnt[knowsTok] != 1 {
		t.Errorf("KNOWS edgeCnt: got %d, want 1", fb.edgeCnt[knowsTok])
	}
	if fb.edgeCnt[likesTok] != 1 {
		t.Errorf("LIKES edgeCnt: got %d, want 1", fb.edgeCnt[likesTok])
	}
}

func TestPass3PrefixType(t *testing.T) {
	// Type comes from src.Type prefix when there is no :TYPE column.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\n"
	relCSV := ":START_ID(p),:END_ID(p)\np1,p2\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		Relationships: []RelSource{
			{Type: "KNOWS", readers: []io.Reader{strings.NewReader(relCSV)}},
		},
	})
	fb, err := l.Pass3BuildCSRFull(memFS(), "test.gr")
	if err != nil {
		t.Fatalf("Pass3BuildCSRFull: %v", err)
	}
	defer fb.Close()

	knowsTok := uint32(l.catalog.RelTypeToken("KNOWS"))
	if _, ok := fb.relCSR[csrKey{knowsTok, csrFwd}]; !ok {
		t.Error("missing KNOWS forward CSR")
	}
}
