package micro

import (
	"context"
	"testing"

	"github.com/tamnd/gr/bench/fixtures"
	"github.com/tamnd/gr/bench/harness"
)

// BenchmarkTriangleCountParallel counts directed triangles on an Erdős–Rényi
// graph large enough to cross the morsel-parallel threshold (more than two
// morsels of nodes), so it exercises the parallel aggregation path and its
// primaryScan id buffer. Run it with -benchmem to watch allocs/op: it is the
// allocation harness for the presize change, which is contention-independent.
func BenchmarkTriangleCountParallel(b *testing.B) {
	fix, err := fixtures.BuildErdosRenyi(fixtures.ErdosRenyiParams{
		Nodes: 4096,
		P:     0.002,
		Seed:  99,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	const query = "MATCH (a)-[:R]->(b)-[:R]->(c)-[:R]->(a) RETURN count(*) AS cnt"

	harness.Warmup(b, 2, func() {
		_, _ = fix.DB.Run(context.Background(), query, nil)
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		res, err := fix.DB.Run(context.Background(), query, nil)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		res.Next()
		_ = res.Close()
	}
}
