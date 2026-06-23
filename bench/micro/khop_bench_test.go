// Package micro contains gr micro-benchmarks (doc 22 §6).
// Each benchmark isolates one engine property on a controlled fixture.
package micro

import (
	"context"
	"fmt"
	"testing"
	"time"

	gr "github.com/tamnd/gr"
	"github.com/tamnd/gr/bench/fixtures"
	"github.com/tamnd/gr/bench/harness"
)

// BenchmarkKhop measures k-hop neighborhood expansion on a uniform-degree graph
// (doc 22 §6.2). It validates the index-free adjacency claim: per-hop cost
// should be O(degree), not O(log n).
//
// The fixture is small enough for CI; larger fixtures are for periodic runs.
func BenchmarkKhop(b *testing.B) {
	for _, hops := range []int{1, 2, 3} {
		for _, degree := range []int{5, 10} {
			b.Run(fmt.Sprintf("k=%d/d=%d", hops, degree), func(b *testing.B) {
				benchKhop(b, hops, degree)
			})
		}
	}
}

func benchKhop(b *testing.B, hops, degree int) {
	b.Helper()

	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  200, // small for CI; 1M+ for periodic runs
		Degree: degree,
		Seed:   42,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	// Create an index on :N(i) so the seed lookup is fast.
	if _, err := fix.DB.Exec("CREATE INDEX FOR (n:N) ON (n.i)", nil); err != nil {
		b.Fatalf("create index: %v", err)
	}

	// Run a few iterations as warm-up.
	harness.Warmup(b, 3, func() {
		_, _ = fix.DB.Run(context.Background(),
			fmt.Sprintf("MATCH (a:N {i:0})-[:R*1..%d]->(b) RETURN count(DISTINCT b) AS cnt", hops),
			nil)
	})

	seeds := []int{0, 5, 10, 15, 20, 25, 30}
	samples := make([]float64, 0, b.N)

	b.ResetTimer()
	for range b.N {
		for _, seed := range seeds {
			q := fmt.Sprintf("MATCH (a:N {i:%d})-[:R*1..%d]->(b) RETURN count(DISTINCT b) AS cnt", seed, hops)
			t0 := time.Now()
			res, err := fix.DB.Run(context.Background(), q, nil)
			if err != nil {
				b.Fatalf("query: %v", err)
			}
			_ = res.Next()
			_ = res.Close()
			samples = append(samples, float64(time.Since(t0).Nanoseconds()))
		}
	}
	b.StopTimer()

	harness.ReportLatencyPercentiles(b, samples)
	b.ReportMetric(float64(hops), "hops")
	b.ReportMetric(float64(degree), "degree")
}

// BenchmarkKhopPowerLaw measures k-hop on a power-law graph (doc 22 §6.2).
// It validates that the supernode tail is bounded.
func BenchmarkKhopPowerLaw(b *testing.B) {
	fix, err := fixtures.BuildPowerLaw(fixtures.PowerLawParams{
		Nodes:    200,
		Exponent: 2.0,
		Seed:     42,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	if _, err := fix.DB.Exec("CREATE INDEX FOR (n:N) ON (n.i)", nil); err != nil {
		b.Fatalf("create index: %v", err)
	}

	samples := make([]float64, 0, b.N)
	b.ResetTimer()
	for range b.N {
		t0 := time.Now()
		res, err := fix.DB.Run(context.Background(),
			"MATCH (a:N {i:0})-[:R*1..2]->(b) RETURN count(DISTINCT b) AS cnt", nil)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
		samples = append(samples, float64(time.Since(t0).Nanoseconds()))
	}
	b.StopTimer()

	harness.ReportLatencyPercentiles(b, samples)
}

// BenchmarkPointLookup measures point-lookup latency via a unique index (doc 22 §6.9).
// It validates that a lookup is fast and low-tail: a B-tree descent plus a
// columnar property read.
func BenchmarkPointLookup(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  500,
		Degree: 5,
		Seed:   1,
	})
	if err != nil {
		b.Fatalf("build fixture: %v", err)
	}
	defer func() { _ = fix.Close() }()

	if _, err := fix.DB.Exec("CREATE INDEX FOR (n:N) ON (n.i)", nil); err != nil {
		b.Fatalf("create index: %v", err)
	}

	keys := makeKeys(100, fix.Nodes)
	samples := make([]float64, 0, b.N*len(keys))

	b.ResetTimer()
	for range b.N {
		for _, k := range keys {
			q := fmt.Sprintf("MATCH (n:N {i:%d}) RETURN n.i AS v", k)
			t0 := time.Now()
			res, err := fix.DB.Run(context.Background(), q, nil)
			if err != nil {
				b.Fatalf("lookup: %v", err)
			}
			_ = res.Next()
			_ = res.Close()
			samples = append(samples, float64(time.Since(t0).Nanoseconds()))
		}
	}
	b.StopTimer()

	harness.ReportLatencyPercentiles(b, samples)
	harness.ReportThroughput(b, float64(len(samples))/b.Elapsed().Seconds(), "lookup")
}

func makeKeys(count, max int) []int {
	if count > max {
		count = max
	}
	out := make([]int, count)
	step := max / count
	for i := range count {
		out[i] = i * step
	}
	return out
}

// dbExec is a helper that runs a query and returns the first integer column.
func dbExec(b *testing.B, db *gr.DB, query string) int64 {
	b.Helper()
	res, err := db.Run(context.Background(), query, nil)
	if err != nil {
		b.Fatalf("exec: %v", err)
	}
	defer func() { _ = res.Close() }()
	if res.Next() {
		rec := res.Record()
		v, _ := rec.GetByIndex(0).(int64)
		return v
	}
	return 0
}
