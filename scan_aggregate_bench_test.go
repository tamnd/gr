package gr

import (
	"context"
	"fmt"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// buildScanDB creates n nodes labelled :N each carrying an integer property i,
// then checkpoints so the property column lives in the segmented base (the read
// path graph-bench's bulk loader produces). Without the checkpoint the values
// stay in the naive delta column and never exercise segGet or the block cache,
// which is the path micro-scan actually hits.
func buildScanDB(tb testing.TB, n int) *DB {
	tb.Helper()
	db, err := Open("bench.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	for i := range n {
		if _, err := db.Exec(fmt.Sprintf("CREATE (:N {i:%d})", i), nil); err != nil {
			tb.Fatalf("create %d: %v", i, err)
		}
	}
	if err := db.eng.Checkpoint(); err != nil {
		tb.Fatalf("checkpoint: %v", err)
	}
	return db
}

// BenchmarkScanAggregate is the gr-side reproduction of graph-bench's micro-scan
// query: a full scan that counts and averages a property. The averaging forces a
// per-node columnar property read, so this benchmark drives the exact read path
// (snapNodeProp -> baseProp -> segGet -> blockcache.GetDecoded) that the
// cross-engine matrix showed gr losing on. Run with -cpuprofile to see where the
// per-row time goes.
func BenchmarkScanAggregate(b *testing.B) {
	const n = 10000
	db := buildScanDB(b, n)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	const q = "MATCH (n:N) RETURN count(n) AS c, avg(n.i) AS avgId"

	b.ResetTimer()
	for range b.N {
		res, err := db.Run(ctx, q, nil)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}

// BenchmarkScanCountOnly isolates the count(n) half: no property read, so the
// gap from BenchmarkScanAggregate to this is the columnar-read cost the vectorized
// path must remove.
func BenchmarkScanCountOnly(b *testing.B) {
	const n = 10000
	db := buildScanDB(b, n)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	const q = "MATCH (n:N) RETURN count(n) AS c"

	b.ResetTimer()
	for range b.N {
		res, err := db.Run(ctx, q, nil)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}
