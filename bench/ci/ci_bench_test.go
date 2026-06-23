// Package ci contains the fast CI regression benchmark subset (doc 22 §11).
// These benchmarks are a small, fast subset of the full micro-benchmark suite,
// sized to run in < 30 seconds on a CI machine. They set performance budgets
// that fail the CI gate when crossed.
//
// Usage in CI:
//
//	go test -bench=. -benchtime=3s ./bench/ci/ -count=3
//
// The output is compared to a baseline via benchstat to detect regressions.
package ci

import (
	"context"
	"fmt"
	"testing"

	"github.com/tamnd/gr/bench/fixtures"
)

// BenchmarkCI_Khop1 measures 1-hop expansion on a small uniform graph.
// Budget: median < 5 ms on any CI hardware.
func BenchmarkCI_Khop1(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  100,
		Degree: 5,
		Seed:   1,
	})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	defer func() { _ = fix.Close() }()

	if _, err := fix.DB.Exec("CREATE INDEX FOR (n:N) ON (n.i)", nil); err != nil {
		b.Fatalf("index: %v", err)
	}

	b.ResetTimer()
	for i := range b.N {
		res, err := fix.DB.Run(context.Background(),
			fmt.Sprintf("MATCH (a:N {i:%d})-[:R]->(b) RETURN count(b) AS cnt", i%50), nil)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}

// BenchmarkCI_PointLookup measures single-node lookup via a unique index.
// Budget: median < 1 ms on any CI hardware.
func BenchmarkCI_PointLookup(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  200,
		Degree: 3,
		Seed:   2,
	})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	defer func() { _ = fix.Close() }()

	if _, err := fix.DB.Exec("CREATE INDEX FOR (n:N) ON (n.i)", nil); err != nil {
		b.Fatalf("index: %v", err)
	}

	b.ResetTimer()
	for i := range b.N {
		res, err := fix.DB.Run(context.Background(),
			fmt.Sprintf("MATCH (n:N {i:%d}) RETURN n.i AS v", i%100), nil)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}

// BenchmarkCI_Write measures single-node CREATE throughput.
// Budget: > 500 commits/s on any CI hardware.
func BenchmarkCI_Write(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  5,
		Degree: 1,
		Seed:   3,
	})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	defer func() { _ = fix.Close() }()

	b.ResetTimer()
	for i := range b.N {
		if _, err := fix.DB.Exec(fmt.Sprintf("CREATE (:CI {n:%d})", i), nil); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
}

// BenchmarkCI_FullScan measures a full label scan with a filter.
// Budget: < 50 ms for 500 nodes on any CI hardware.
func BenchmarkCI_FullScan(b *testing.B) {
	fix, err := fixtures.BuildUniform(fixtures.UniformParams{
		Nodes:  500,
		Degree: 2,
		Seed:   4,
	})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	defer func() { _ = fix.Close() }()

	b.ResetTimer()
	for range b.N {
		res, err := fix.DB.Run(context.Background(),
			"MATCH (n:N) WHERE n.i > 100 RETURN count(n) AS cnt", nil)
		if err != nil {
			b.Fatalf("scan: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}
