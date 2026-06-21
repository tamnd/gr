package adj

import (
	"testing"

	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// TestAdjacencyCompresses checks that the checkpoint stores the CSR compressed:
// a regular, clustered graph (constant degree, contiguous neighbor blocks) should
// pack into a small fraction of the raw eight-bytes-per-entry the arrays would
// take, because the offsets fall to delta-FOR at width zero, the neighbor gaps to
// delta, and the edge ids to FOR (doc 15 §15).
func TestAdjacencyCompresses(t *testing.T) {
	fsys := vfs.NewMem()
	pg, err := pager.Open(fsys, "csr.gr", pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	secs, err := store.CreateSections(pg)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := node.Create(pg, secs)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rel.Create(pg, secs)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Create(pg, secs, rs)
	if err != nil {
		t.Fatal(err)
	}

	const n = 1000
	const degree = 8
	for range n {
		if _, err := ns.Create(nil); err != nil {
			t.Fatal(err)
		}
	}
	// Each node points at the next `degree` nodes, a contiguous, sorted run, so the
	// neighbor gaps are all one and the degree is constant.
	for i := range uint64(n) {
		for k := range uint64(degree) {
			dst := (i + 1 + k) % n
			pos, err := rs.Create(0, i, dst)
			if err != nil {
				t.Fatal(err)
			}
			a.Insert(0, i, dst, pos)
		}
	}
	if err := a.Checkpoint(uint64(ns.Count())); err != nil {
		t.Fatal(err)
	}

	// Sum the compressed CSR blob bytes across every slot.
	var got uint64
	for s := range a.dir.Count() {
		c, err := a.readDir(uint32(s))
		if err != nil {
			t.Fatal(err)
		}
		got += c.offBytes + c.nbrBytes + c.edgBytes
	}

	// Raw would be eight bytes per offset entry (nodeCount+1 per direction) plus
	// eight per neighbor and per edge id (one of each per directed edge), over both
	// directions.
	edges := uint64(n) * degree
	rawEntries := 2*(uint64(n)+1) + 2*edges + 2*edges
	raw := rawEntries * 8
	if got >= raw/4 {
		t.Fatalf("CSR did not compress: %d bytes vs raw %d (want < raw/4)", got, raw)
	}
	t.Logf("CSR compressed to %d bytes from raw %d (%.1f%%)", got, raw, 100*float64(got)/float64(raw))
}
