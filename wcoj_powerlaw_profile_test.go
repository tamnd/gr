package gr

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime/pprof"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// buildCappedPowerLaw loads a directed power-law graph whose out-degrees are capped,
// the shape graph-bench's micro-powerlaw generator feeds the triangle counts (gamma
// 2.5, a hub tail bounded well below the node count). It returns the db and the edge
// count so a profile reports against a representative graph, not the unbounded-hub
// zipf the gr fixtures draw.
func buildCappedPowerLaw(tb testing.TB, nodes, maxDeg int, exponent float64, seed int64) (*DB, int) {
	tb.Helper()
	db, err := Open("wcojpl.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	rng := rand.New(rand.NewSource(seed))
	zipf := rand.NewZipf(rng, exponent, 1, uint64(maxDeg))

	for i := range nodes {
		if _, err := db.Exec(fmt.Sprintf("CREATE (:N {i:%d})", i), nil); err != nil {
			tb.Fatalf("node %d: %v", i, err)
		}
	}
	edges := 0
	for i := range nodes {
		degree := int(zipf.Uint64())
		seen := map[int]bool{i: true}
		for range degree {
			t := rng.Intn(nodes)
			if seen[t] {
				continue
			}
			seen[t] = true
			if _, err := db.Exec(fmt.Sprintf("MATCH (a:N {i:%d}), (b:N {i:%d}) CREATE (a)-[:R]->(b)", i, t), nil); err != nil {
				tb.Fatalf("edge %d->%d: %v", i, t, err)
			}
			edges++
		}
	}
	return db, edges
}

const plTriangleDirected = "MATCH (a:N)-[:R]->(b:N)-[:R]->(c:N)-[:R]->(a) RETURN count(*) AS n"

// TestWcojFusedTriangleCount checks the fused triangle count against a brute-force
// enumeration over a real skewed graph, so the zero-materialization anchor path
// (intersectCountOp's direct-from-scan merge-intersect) is exercised on the same degree distribution graph-bench
// feeds it, hubs and all, not just the tiny hand-built fixture. It reads every edge
// out through the public API, builds the adjacency in the test, counts directed
// triangles the obvious way, and asserts the engine's count matches.
func TestWcojFusedTriangleCount(t *testing.T) {
	db, _ := buildCappedPowerLaw(t, 400, 150, 1.35, 7)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// Pull every edge as (a,b) id pairs through the engine, then count a->b->c->a
	// directed triangles by hand over the adjacency those pairs form.
	out := map[int64]map[int64]bool{}
	res, err := db.Run(ctx, "MATCH (a:N)-[:R]->(b:N) RETURN id(a) AS a, id(b) AS b", nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		row, ok, err := res.Row()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		a, _ := row[0].AsInt()
		b, _ := row[1].AsInt()
		if out[a] == nil {
			out[a] = map[int64]bool{}
		}
		out[a][b] = true
	}
	_ = res.Close()

	var want int64
	for a := range out {
		for b := range out[a] {
			for c := range out[b] {
				if out[c][a] {
					want++
				}
			}
		}
	}

	res, err = db.Run(ctx, plTriangleDirected, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := res.Row()
	if err != nil || !ok {
		t.Fatalf("no count row: ok=%v err=%v", ok, err)
	}
	got, _ := row[0].AsInt()
	_ = res.Close()
	if got != want {
		t.Fatalf("fused triangle count = %d, brute force = %d", got, want)
	}
	if want == 0 {
		t.Fatal("graph has no triangles, test proves nothing")
	}
	t.Logf("directed triangles: %d", want)
}

const plTriangleUndirected = "MATCH (a:N)-[:R]-(b:N)-[:R]-(c:N)-[:R]-(a) WHERE id(a) < id(b) AND id(b) < id(c) RETURN count(*) AS n"

// TestWcojFusedUndirectedTriangleCount checks the undirected triangle count, the one
// whose `id(a) < id(b) < id(c)` ordering used to block the factorization, against a
// brute-force enumeration over the same skewed graph. The anchor half of the ordering
// (id(a) < id(b)) is pushed into the fused operator as the anchor predicate and the
// apex half (id(b) < id(c)) as the apex predicate, so this exercises both pushed-down
// predicates on the zero-materialization path. The undirected pattern matches each
// relationship between two nodes once per direction, so a bidirectional pair counts
// twice; the brute force mirrors that with a per-pair multiplicity, m(x,y) being the
// number of directed edges between x and y in either direction.
func TestWcojFusedUndirectedTriangleCount(t *testing.T) {
	db, _ := buildCappedPowerLaw(t, 400, 150, 1.35, 7)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	out := map[int64]map[int64]bool{}
	res, err := db.Run(ctx, "MATCH (a:N)-[:R]->(b:N) RETURN id(a) AS a, id(b) AS b", nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		row, ok, err := res.Row()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		a, _ := row[0].AsInt()
		b, _ := row[1].AsInt()
		if out[a] == nil {
			out[a] = map[int64]bool{}
		}
		out[a][b] = true
	}
	_ = res.Close()

	// m(x,y): relationships between x and y in either direction, the multiplicity the
	// undirected pattern matches. Build it symmetric, then sum the product over each
	// ordered triple a < b < c whose three sides are all present.
	mult := func(x, y int64) int64 {
		var n int64
		if out[x][y] {
			n++
		}
		if out[y][x] {
			n++
		}
		return n
	}
	nodes := map[int64]bool{}
	for a := range out {
		nodes[a] = true
		for b := range out[a] {
			nodes[b] = true
		}
	}
	var want int64
	for a := range nodes {
		for b := range nodes {
			if b <= a {
				continue
			}
			mab := mult(a, b)
			if mab == 0 {
				continue
			}
			for c := range nodes {
				if c <= b {
					continue
				}
				mbc := mult(b, c)
				if mbc == 0 {
					continue
				}
				want += mab * mbc * mult(a, c)
			}
		}
	}

	res, err = db.Run(ctx, plTriangleUndirected, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := res.Row()
	if err != nil || !ok {
		t.Fatalf("no count row: ok=%v err=%v", ok, err)
	}
	got, _ := row[0].AsInt()
	_ = res.Close()
	if got != want {
		t.Fatalf("undirected triangle count = %d, brute force = %d", got, want)
	}
	if want == 0 {
		t.Fatal("graph has no undirected triangles, test proves nothing")
	}
	t.Logf("undirected triangles: %d", want)
}

// TestWcojProfileTriangle counts the directed triangle many times over a
// representative capped power-law graph, with a CPU profile scoped to the count loop
// so the graph build is out of the profile and the cost center is the query. It is a
// manual probe, skipped unless GR_WCOJ_PROFILE names a profile path.
func TestWcojProfileTriangle(t *testing.T) {
	out := os.Getenv("GR_WCOJ_PROFILE")
	if out == "" {
		t.Skip("set GR_WCOJ_PROFILE to a path to capture a CPU profile")
	}
	db, edges := buildCappedPowerLaw(t, 5000, 500, 2.5, 1)
	defer func() { _ = db.Close() }()
	t.Logf("graph: 5000 nodes, %d edges", edges)

	ctx := context.Background()
	for range 5 {
		res, _ := db.Run(ctx, plTriangleDirected, nil)
		_, _, _ = res.Row()
		_ = res.Close()
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	for range 400 {
		res, err := db.Run(ctx, plTriangleDirected, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := res.Row(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
	}
	pprof.StopCPUProfile()
}
