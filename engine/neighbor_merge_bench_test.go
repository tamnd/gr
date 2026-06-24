package engine

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
)

// BenchmarkNeighborsBothMergeVsSort isolates the undirected read path's per-call
// cost: assembling one node's two directional runs into a single position-sorted
// slice. Both sub-benchmarks run the same neighborsByPosVia on the same checkpointed
// graph and the same bound cursor, differing only in the final assembly: "sort"
// concatenates and sorts (the old path, scratch nil), "merge" linearly merges the two
// pre-sorted runs (the new path, scratch supplied). Because both run on the same core
// under whatever load the box carries, their ratio is contention-invariant, which is
// what makes this measurable where the end-to-end co-run is not. Degree is set to a
// realistic spread so the n log n versus n gap is visible.
func BenchmarkNeighborsBothMergeVsSort(b *testing.B) {
	fsys := vfs.NewMem()
	e, err := Open(fsys, "merge_bench.gr", pager.Options{Sync: wal.SyncFull, SaltSeed: 1})
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()

	knows, _ := e.Intern(catalog.KindRelType, "KNOWS")
	person, _ := e.Intern(catalog.KindLabel, "Person")

	const outs = 64
	const ins = 64
	tx, _ := e.Begin(true)
	hub, _ := tx.CreateNode([]Token{person})
	for i := 0; i < outs; i++ {
		s, _ := tx.CreateNode([]Token{person})
		if _, err := tx.CreateRel(hub, s, knows); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < ins; i++ {
		s, _ := tx.CreateNode([]Token{person})
		if _, err := tx.CreateRel(s, hub, knows); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if err := e.Checkpoint(); err != nil {
		b.Fatal(err)
	}

	rx, _ := e.Begin(false)
	defer rx.Abort()
	t := rx.(*diskTx)
	var rc store.Cursor
	defer t.e.rels.ReleaseCursor(&rc)
	visible := func(edge uint64) bool { return t.relLiveVia(edge, &rc) }
	buf := make([]PosNeighbor, 0, outs+ins)

	b.Run("sort", func(b *testing.B) {
		t.rlock()
		defer t.runlock()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			got, err := t.neighborsByPosVia(hub, knows, Both, buf, visible, nil)
			if err != nil {
				b.Fatal(err)
			}
			buf = got[:0]
		}
	})

	b.Run("merge", func(b *testing.B) {
		var scratch []PosNeighbor
		t.rlock()
		defer t.runlock()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			got, err := t.neighborsByPosVia(hub, knows, Both, buf, visible, &scratch)
			if err != nil {
				b.Fatal(err)
			}
			buf = got[:0]
		}
	})
}
