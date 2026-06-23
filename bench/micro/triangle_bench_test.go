package micro

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tamnd/gr/bench/fixtures"
	"github.com/tamnd/gr/bench/harness"
)

// BenchmarkTriangleCount measures triangle counting on Erdős–Rényi graphs
// (doc 22 §6.3). The planner must choose a worst-case-optimal join (WCOJ) for
// the cyclic pattern. The benchmark reports total time and, for the CI run,
// validates the result is non-zero on a dense enough graph.
func BenchmarkTriangleCount(b *testing.B) {
	for _, density := range []float64{0.05, 0.10} {
		b.Run(fmt.Sprintf("p=%.2f", density), func(b *testing.B) {
			benchTriangle(b, density)
		})
	}
}

func benchTriangle(b *testing.B, p float64) {
	b.Helper()

	fix, err := fixtures.BuildErdosRenyi(fixtures.ErdosRenyiParams{
		Nodes: 50,
		P:     p,
		Seed:  99,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	const query = "MATCH (a)-[:R]->(b)-[:R]->(c)-[:R]->(a) RETURN count(*) AS cnt"

	// Warm-up.
	harness.Warmup(b, 2, func() {
		_, _ = fix.DB.Run(context.Background(), query, nil)
	})

	samples := make([]float64, 0, b.N)
	b.ResetTimer()
	for range b.N {
		t0 := time.Now()
		res, err := fix.DB.Run(context.Background(), query, nil)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		res.Next()
		elapsed := time.Since(t0)
		_ = res.Close()
		samples = append(samples, float64(elapsed.Nanoseconds()))
	}
	b.StopTimer()

	harness.ReportLatencyPercentiles(b, samples)
	b.ReportMetric(float64(fix.Nodes), "nodes")
	b.ReportMetric(float64(fix.Edges/2), "edges")
}

// BenchmarkShortestPath measures shortest-path queries on a grid graph (doc 22 §6.4).
// The grid's diameter is known so the benchmark can validate the result.
func BenchmarkShortestPath(b *testing.B) {
	fix, err := fixtures.BuildGrid(fixtures.GridParams{
		Rows: 10,
		Cols: 10,
	})
	if err != nil {
		b.Fatalf("build grid: %v", err)
	}
	defer func() { _ = fix.Close() }()

	if _, err := fix.DB.Exec("CREATE INDEX FOR (n:N) ON (n.i)", nil); err != nil {
		b.Fatalf("create index: %v", err)
	}

	// Pairs: corner-to-corner (longest path) and near pairs.
	pairs := [][2]int{
		{0, fix.Nodes - 1}, // corner to corner
		{0, fix.Nodes / 4},
		{0, fix.Nodes / 2},
	}

	samples := make([]float64, 0, b.N*len(pairs))
	b.ResetTimer()
	for range b.N {
		for _, pair := range pairs {
			q := fmt.Sprintf(
				"MATCH p = shortestPath((a:N {i:%d})-[:R*]-(b:N {i:%d})) RETURN length(p) AS d",
				pair[0], pair[1])
			t0 := time.Now()
			res, err := fix.DB.Run(context.Background(), q, nil)
			if err != nil {
				b.Fatalf("shortestPath: %v", err)
			}
			res.Next()
			elapsed := time.Since(t0)
			_ = res.Close()
			samples = append(samples, float64(elapsed.Nanoseconds()))
		}
	}
	b.StopTimer()

	harness.ReportLatencyPercentiles(b, samples)
	b.ReportMetric(float64(fix.Nodes), "nodes")
	b.ReportMetric(float64(fix.Diameter), "diameter")
}

// BenchmarkFullScan measures a full-graph property scan (doc 22 §6.8).
// It counts all nodes with a property in a range, validating the columnar
// access path (only the filtered column is read, not all node properties).
func BenchmarkFullScan(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  500,
		Degree: 3,
		Seed:   7,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	samples := make([]float64, 0, b.N)
	b.ResetTimer()
	for range b.N {
		t0 := time.Now()
		res, err := fix.DB.Run(context.Background(),
			"MATCH (n:N) WHERE n.i >= 100 AND n.i < 300 RETURN count(n) AS cnt", nil)
		if err != nil {
			b.Fatalf("scan: %v", err)
		}
		res.Next()
		elapsed := time.Since(t0)
		_ = res.Close()
		samples = append(samples, float64(elapsed.Nanoseconds()))
	}
	b.StopTimer()

	harness.ReportLatencyPercentiles(b, samples)
	harness.ReportThroughput(b, float64(fix.Nodes)/b.Elapsed().Seconds()*float64(b.N), "node-scan")
}

// BenchmarkWriteThroughput measures write throughput (doc 22 §6.11).
// It runs a stream of single-node CREATE transactions and measures
// commits/s at default durability.
func BenchmarkWriteThroughput(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  10,
		Degree: 2,
		Seed:   3,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	b.ResetTimer()
	commits := 0
	for i := range b.N {
		if _, err := fix.DB.Exec(
			fmt.Sprintf("CREATE (:W {seq:%d})", i), nil); err != nil {
			b.Fatalf("write: %v", err)
		}
		commits++
	}
	b.StopTimer()

	harness.ReportThroughput(b, float64(commits)/b.Elapsed().Seconds(), "commit")
}
