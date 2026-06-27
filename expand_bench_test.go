package gr

import (
	"context"
	"testing"
)

// BenchmarkExpandTwoHop drives a two-hop path count over a skewed power-law graph.
// Each match expands b's out-neighbors for every a->b edge, so the query is a
// direct stress of the traversal SPI (Tx.Expand) and exposes the per-call
// adjacency cost. It is a plain path count, not a closed cycle, so the planner
// cannot collapse it into a fused intersect-count; it walks the edges, which is
// what the read path does in the SNB and LSQB traversal queries.
func BenchmarkExpandTwoHop(b *testing.B) {
	db, _ := buildCappedPowerLaw(b, 400, 150, 1.35, 7)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	const q = "MATCH (a:N)-[:R]->(b:N)-[:R]->(c:N) RETURN count(*) AS n"

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		res, err := db.Run(ctx, q, nil)
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, ok, err := res.Row()
			if err != nil {
				b.Fatal(err)
			}
			if !ok {
				break
			}
		}
		_ = res.Close()
	}
}
