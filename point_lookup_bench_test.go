package gr

import (
	"context"
	"fmt"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// buildPointDB creates n :Node{id} nodes, stands up the label-property index
// graph-bench's loader builds before serving point reads, then checkpoints so
// the data lives in the segmented base. This is the exact shape micro-point and
// micro-point-miss probe: a single index seek plus (on a hit) one property read.
func buildPointDB(tb testing.TB, n int) *DB {
	tb.Helper()
	db, err := Open("point.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	for i := range n {
		if _, err := db.Exec(fmt.Sprintf("CREATE (:Node {id:%d})", i), nil); err != nil {
			tb.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS FOR (n:Node) ON (n.id)", nil); err != nil {
		tb.Fatalf("index: %v", err)
	}
	if err := db.eng.Checkpoint(); err != nil {
		tb.Fatalf("checkpoint: %v", err)
	}
	return db
}

// BenchmarkPointHit is the gr-side reproduction of micro-point: a parameterized
// index seek that finds one node and reads one property. At micro scale the query
// does almost no work, so this benchmark is dominated by gr's fixed per-query cost
// (parse-cache lookup, plan, transaction begin/commit, result setup), which is the
// P2 target. Run with -cpuprofile / -memprofile to see where the fixed time goes.
func BenchmarkPointHit(b *testing.B) {
	const n = 10000
	db := buildPointDB(b, n)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	const q = "MATCH (n:Node {id: $id}) RETURN n.id AS id"
	params := map[string]any{"id": int64(4242)}

	b.ResetTimer()
	for range b.N {
		res, err := db.Run(ctx, q, params)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}

// BenchmarkPointMiss is the negative variant (micro-point-miss): the same seek
// against an id that is absent, so it returns zero rows. The miss skips the
// property read a hit does, so the gap from BenchmarkPointHit is that read; what
// they share is the fixed per-query cost.
func BenchmarkPointMiss(b *testing.B) {
	const n = 10000
	db := buildPointDB(b, n)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	const q = "MATCH (n:Node {id: $id}) RETURN n.id AS id"
	params := map[string]any{"id": int64(n + 1)}

	b.ResetTimer()
	for range b.N {
		res, err := db.Run(ctx, q, params)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		_ = res.Next()
		_ = res.Close()
	}
}
