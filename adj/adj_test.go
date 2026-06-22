package adj_test

import (
	"slices"
	"testing"

	"github.com/tamnd/gr/adj"
	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

const path = "adj.gr"

// open opens the pager, sections, rel store, node store, and adjacency, creating
// the stores if create is true.
func open(t *testing.T, fsys vfs.VFS, p string, create bool) (*pager.Pager, *node.Store, *rel.Store, *adj.Adj) {
	t.Helper()
	pg, err := pager.Open(fsys, p, pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	var secs *store.Sections
	var ns *node.Store
	var rs *rel.Store
	var a *adj.Adj
	if create {
		secs, _ = store.CreateSections(pg)
		ns, err = node.Create(pg, secs)
		if err != nil {
			t.Fatal(err)
		}
		rs, err = rel.Create(pg, secs)
		if err != nil {
			t.Fatal(err)
		}
		a, err = adj.Create(pg, secs, rs)
	} else {
		secs, err = store.OpenSections(pg)
		if err != nil {
			t.Fatal(err)
		}
		ns, err = node.Open(pg, secs)
		if err != nil {
			t.Fatal(err)
		}
		rs, err = rel.Open(pg, secs)
		if err != nil {
			t.Fatal(err)
		}
		a, err = adj.Open(pg, secs, rs)
	}
	if err != nil {
		t.Fatal(err)
	}
	return pg, ns, rs, a
}

// addEdge creates a relationship record and indexes it in the adjacency.
func addEdge(t *testing.T, rs *rel.Store, a *adj.Adj, relType uint32, src, dst uint64) uint64 {
	t.Helper()
	pos, err := rs.Create(relType, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	a.Insert(relType, src, dst, pos)
	return pos
}

func nodes(out []adj.Neighbor) []uint64 {
	ns := make([]uint64, len(out))
	for i, n := range out {
		ns[i] = n.Node
	}
	return ns
}

// TestExpandDeltaThenBase builds a small typed graph, expands from the delta,
// checkpoints, and expands again from the base, asserting identical results and
// that the delta empties at checkpoint.
func TestExpandDeltaThenBase(t *testing.T) {
	fsys := vfs.NewMem()
	pg, ns, rs, a := open(t, fsys, path, true)

	for range 4 {
		if _, err := ns.Create(nil); err != nil {
			t.Fatal(err)
		}
	}
	// KNOWS(=1): 0->1, 0->2, 2->3 ; RATED(=2): 0->3
	addEdge(t, rs, a, 1, 0, 1)
	addEdge(t, rs, a, 1, 0, 2)
	addEdge(t, rs, a, 1, 2, 3)
	addEdge(t, rs, a, 2, 0, 3)

	check := func(stage string) {
		out, _ := a.Expand(0, 1, adj.Out)
		if got := nodes(out); !slices.Equal(got, []uint64{1, 2}) {
			t.Fatalf("%s: expand(0,KNOWS,out) = %v, want [1 2]", stage, got)
		}
		out, _ = a.Expand(2, 1, adj.In)
		if got := nodes(out); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("%s: expand(2,KNOWS,in) = %v, want [0]", stage, got)
		}
		// Type selectivity: RATED out of 0 is only node 3, not the KNOWS edges.
		out, _ = a.Expand(0, 2, adj.Out)
		if got := nodes(out); !slices.Equal(got, []uint64{3}) {
			t.Fatalf("%s: expand(0,RATED,out) = %v, want [3]", stage, got)
		}
		out, _ = a.Expand(3, 2, adj.In)
		if got := nodes(out); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("%s: expand(3,RATED,in) = %v, want [0]", stage, got)
		}
	}

	check("delta")
	if a.DeltaLen() == 0 {
		t.Fatal("delta should be non-empty before checkpoint")
	}
	if err := a.Checkpoint(uint64(ns.Count())); err != nil {
		t.Fatal(err)
	}
	if a.DeltaLen() != 0 {
		t.Fatalf("delta should be empty after checkpoint, got %d", a.DeltaLen())
	}
	check("base")
	if err := pg.Commit(); err != nil {
		t.Fatal(err)
	}
	pg.Close()

	// Reopen: the base is durable, the delta rebuilds empty.
	pg2, _, _, a2 := open(t, fsys, path, false)
	defer pg2.Close()
	out, _ := a2.Expand(0, 1, adj.Out)
	if got := nodes(out); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("reopen: expand(0,KNOWS,out) = %v, want [1 2]", got)
	}
	if a2.DeltaLen() != 0 {
		t.Fatalf("reopen delta should be empty (all folded), got %d", a2.DeltaLen())
	}
}

// TestDeleteDropsEdge verifies a deleted edge disappears from expand whether it
// lives in the delta or the base, because the merge filters by liveness.
func TestDeleteDropsEdge(t *testing.T) {
	fsys := vfs.NewMem()
	pg, ns, rs, a := open(t, fsys, path, true)
	defer pg.Close()
	for range 3 {
		ns.Create(nil)
	}
	e0 := addEdge(t, rs, a, 1, 0, 1)
	addEdge(t, rs, a, 1, 0, 2)

	// Delete e0 while it is in the delta.
	if err := rs.Delete(e0); err != nil {
		t.Fatal(err)
	}
	out, _ := a.Expand(0, 1, adj.Out)
	if got := nodes(out); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("after delta delete: %v, want [2]", got)
	}

	// Add a fresh edge, checkpoint (folds the live ones into the base), then
	// delete a base edge and confirm it drops too.
	e2 := addEdge(t, rs, a, 1, 0, 2)
	_ = e2
	if err := a.Checkpoint(uint64(ns.Count())); err != nil {
		t.Fatal(err)
	}
	// Now delete one of the base edges (0->2 via e2 is in base now).
	if err := rs.Delete(e2); err != nil {
		t.Fatal(err)
	}
	out, _ = a.Expand(0, 1, adj.Out)
	// e0 deleted, e2 deleted; the original 0->2 (second addEdge) remains.
	if got := nodes(out); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("after base delete: %v, want [2]", got)
	}
}

// TestDenseNodeDegree builds a supernode with high degree across two types and
// checks the dense-node mechanism (doc 04 §12): a typed, directed expand touches
// only that type's run, degree is reported without materializing the run, and the
// count stays correct across deletes, a checkpoint, and a reopen.
func TestDenseNodeDegree(t *testing.T) {
	const typeA, typeB = 1, 2
	const degA, degB = 2000, 500
	fsys := vfs.NewMem()
	pg, ns, rs, a := open(t, fsys, path, true)

	// One supernode (position 0) plus degA distinct neighbors.
	for range degA + 1 {
		if _, err := ns.Create(nil); err != nil {
			t.Fatal(err)
		}
	}
	var firstA uint64
	for k := uint64(1); k <= degA; k++ {
		pos := addEdge(t, rs, a, typeA, 0, k)
		if k == 1 {
			firstA = pos
		}
	}
	for k := uint64(1); k <= degB; k++ {
		addEdge(t, rs, a, typeB, 0, k)
	}

	// Typed selectivity: each type's expand and degree see only that type.
	check := func(when string, wantA, wantB int64) {
		if d, _ := a.Degree(0, typeA, adj.Out); d != wantA {
			t.Fatalf("%s: degree(A,out) = %d, want %d", when, d, wantA)
		}
		if d, _ := a.Degree(0, typeB, adj.Out); d != wantB {
			t.Fatalf("%s: degree(B,out) = %d, want %d", when, d, wantB)
		}
		if d, _ := a.Degree(0, typeA, adj.In); d != 0 {
			t.Fatalf("%s: degree(A,in) = %d, want 0", when, d)
		}
		outA, _ := a.Expand(0, typeA, adj.Out)
		if int64(len(outA)) != wantA {
			t.Fatalf("%s: expand(A,out) = %d edges, want %d", when, len(outA), wantA)
		}
		outB, _ := a.Expand(0, typeB, adj.Out)
		if int64(len(outB)) != wantB {
			t.Fatalf("%s: expand(B,out) = %d edges, want %d", when, len(outB), wantB)
		}
	}
	check("delta", degA, degB)

	// Each neighbor carries exactly one incoming type-A edge from the supernode.
	if d, _ := a.Degree(1, typeA, adj.In); d != 1 {
		t.Fatalf("neighbor in-degree = %d, want 1", d)
	}

	// Delete one type-A edge from the delta: degree drops and expand omits it.
	if err := rs.Delete(firstA); err != nil {
		t.Fatal(err)
	}
	a.Remove(typeA, 0, 1)
	check("after delta delete", degA-1, degB)

	// Checkpoint folds the live edges into the base; degree comes from the offset
	// array now, and the tail adjustment resets.
	if err := a.Checkpoint(uint64(ns.Count())); err != nil {
		t.Fatal(err)
	}
	check("after checkpoint", degA-1, degB)

	// Delete a base edge (a type-B edge, now folded) and confirm the count tracks it.
	bEdge := uint64(degA + 5) // a type-B edge position created after the degA type-A ones
	r, err := rs.Get(bEdge)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Delete(bEdge); err != nil {
		t.Fatal(err)
	}
	a.Remove(r.Type, r.Src, r.Dst)
	check("after base delete", degA-1, degB-1)

	// Reopen and confirm the base degree survives (folded edges, minus the
	// pre-reopen base delete healed by a checkpoint).
	if err := a.Checkpoint(uint64(ns.Count())); err != nil {
		t.Fatal(err)
	}
	if err := pg.Commit(); err != nil {
		t.Fatal(err)
	}
	pg.Close()
	pg, _, _, a = open(t, fsys, path, false)
	defer pg.Close()
	if d, _ := a.Degree(0, typeA, adj.Out); d != degA-1 {
		t.Fatalf("after reopen: degree(A,out) = %d, want %d", d, degA-1)
	}
	if d, _ := a.Degree(0, typeB, adj.Out); d != degB-1 {
		t.Fatalf("after reopen: degree(B,out) = %d, want %d", d, degB-1)
	}
}

// --- headline crash campaign over the adjacency ---

const cpath = "adjcrash.gr"

func buildClean(t *testing.T) *vfs.Mem {
	t.Helper()
	fsys := vfs.NewMem()
	pg, _, _, _ := open(t, fsys, cpath, true)
	if err := pg.Commit(); err != nil {
		t.Fatal(err)
	}
	pg.Close()
	return fsys
}

// runWorkload builds a path graph of typed edges with a checkpoint partway
// through, one commit per step, so crashes land at every boundary including
// during the checkpoint commit.
func runWorkload(fsys vfs.VFS, T int) (err error) {
	pg, e := pager.Open(fsys, cpath, pager.Options{Sync: wal.SyncFull, SaltSeed: 7})
	if e != nil {
		return e
	}
	defer func() { _ = pg.Close() }()
	secs, e := store.OpenSections(pg)
	if e != nil {
		return e
	}
	ns, e := node.Open(pg, secs)
	if e != nil {
		return e
	}
	rs, e := rel.Open(pg, secs)
	if e != nil {
		return e
	}
	a, e := adj.Open(pg, secs, rs)
	if e != nil {
		return e
	}
	var prev uint64
	for j := range T {
		pos, e := ns.Create(nil)
		if e != nil {
			return e
		}
		if j > 0 {
			rp, e := rs.Create(1, pos, prev)
			if e != nil {
				return e
			}
			a.Insert(1, pos, prev, rp)
		}
		prev = pos
		if j == T/2 {
			if e := a.Checkpoint(uint64(ns.Count())); e != nil {
				return e
			}
		}
		if e := pg.Commit(); e != nil {
			return e
		}
	}
	return nil
}

// verifyConsistent reopens a crashed snapshot and asserts the adjacency agrees
// exactly with the durable relationship records: for every live relationship,
// the edge appears in both directions' expand, and expand never yields a dead or
// dangling edge.
func verifyConsistent(t *testing.T, crashed *vfs.Mem, label string) {
	t.Helper()
	pg, err := pager.Open(crashed, cpath, pager.Options{})
	if err != nil {
		t.Fatalf("%s: reopen: %v", label, err)
	}
	defer pg.Close()
	secs, err := store.OpenSections(pg)
	if err != nil {
		t.Fatalf("%s: sections: %v", label, err)
	}
	ns, err := node.Open(pg, secs)
	if err != nil {
		t.Fatalf("%s: node: %v", label, err)
	}
	rs, err := rel.Open(pg, secs)
	if err != nil {
		t.Fatalf("%s: rel: %v", label, err)
	}
	a, err := adj.Open(pg, secs, rs)
	if err != nil {
		t.Fatalf("%s: adj: %v", label, err)
	}

	// Expected adjacency from the durable, live relationship records.
	wantOut := map[uint64][]uint64{}
	wantIn := map[uint64][]uint64{}
	for pos := range uint64(rs.Count()) {
		if !rs.Exists(pos) {
			continue
		}
		r, err := rs.Get(pos)
		if err != nil {
			t.Fatalf("%s: rel %d: %v", label, pos, err)
		}
		wantOut[r.Src] = append(wantOut[r.Src], r.Dst)
		wantIn[r.Dst] = append(wantIn[r.Dst], r.Src)
	}

	for n := range uint64(ns.Count()) {
		out, err := a.Expand(n, 1, adj.Out)
		if err != nil {
			t.Fatalf("%s: expand out %d: %v", label, n, err)
		}
		in, err := a.Expand(n, 1, adj.In)
		if err != nil {
			t.Fatalf("%s: expand in %d: %v", label, n, err)
		}
		// Every yielded edge must be live and connect the right endpoints.
		for _, nb := range out {
			if !rs.Exists(nb.Edge) {
				t.Fatalf("%s: expand out %d yielded dead edge %d", label, n, nb.Edge)
			}
		}
		exp := wantOut[n]
		slices.Sort(exp)
		if got := nodes(out); !slices.Equal(got, exp) {
			t.Fatalf("%s: expand out %d = %v, want %v", label, n, got, exp)
		}
		expIn := wantIn[n]
		slices.Sort(expIn)
		if got := nodes(in); !slices.Equal(got, expIn) {
			t.Fatalf("%s: expand in %d = %v, want %v", label, n, got, expIn)
		}
	}
}

func crashCampaign(t *testing.T, mode vfs.TripMode, label string) {
	const T = 6
	clean := buildClean(t)

	counter := vfs.NewCounter()
	cfs := clean.Snapshot()
	cfs.Attach(counter)
	if err := runWorkload(cfs, T); err != nil {
		t.Fatalf("%s: counting run errored: %v", label, err)
	}
	n := counter.Count()
	if n == 0 {
		t.Fatalf("%s: no fault points", label)
	}
	for trip := range n {
		fs := clean.Snapshot()
		fs.Attach(vfs.NewTrip(trip, mode))
		_ = runWorkload(fs, T)
		verifyConsistent(t, fs.Snapshot(), label)
	}
}

// TestCheckpointReclaimsOldCSR is the W4d gate: a checkpoint frees the old base
// CSR vectors it rebuilds, so a run of checkpoints over an unchanging graph reuses
// those pages instead of growing the file. Without the frees each checkpoint leaks
// a fresh offset, neighbor, and edge vector per slot and the page count climbs.
func TestCheckpointReclaimsOldCSR(t *testing.T) {
	fsys := vfs.NewMem()
	pg, ns, rs, a := open(t, fsys, path, true)
	defer pg.Close()

	// A small fixed graph across two relationship types, so the checkpoint rebuilds
	// several slots and a leak shows up in the page count.
	const n = 32
	for range n {
		if _, err := ns.Create(nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := range uint64(n) {
		addEdge(t, rs, a, 0, i, (i+1)%n)
		addEdge(t, rs, a, 1, i, (i+2)%n)
	}

	// Warm up to steady state, then hold the page count across many more passes.
	for range 3 {
		if err := a.Checkpoint(uint64(ns.Count())); err != nil {
			t.Fatal(err)
		}
	}
	before := pg.Header().PageCount
	for range 20 {
		if err := a.Checkpoint(uint64(ns.Count())); err != nil {
			t.Fatal(err)
		}
	}
	after := pg.Header().PageCount
	if after != before {
		t.Fatalf("page count grew across checkpoints: %d -> %d (old CSR not reclaimed)", before, after)
	}

	// Adjacency still reads back after all the page reuse.
	out, err := a.Expand(0, 0, adj.Out)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodes(out); !slices.Equal(got, []uint64{1}) {
		t.Fatalf("expand after reuse = %v, want [1]", got)
	}
}

func TestAdjCrashCampaignCrash(t *testing.T) { crashCampaign(t, vfs.TripCrash, "crash") }
func TestAdjCrashCampaignTorn(t *testing.T)  { crashCampaign(t, vfs.TripTear, "torn") }
func TestAdjCrashCampaignFsync(t *testing.T) { crashCampaign(t, vfs.TripFsyncFail, "fsync") }
