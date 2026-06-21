package exec

import (
	"math/rand"
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// TestShortestBidiMatchesUnidirectional is the differential gate for the
// bidirectional shortest-path search: over many random graphs and every endpoint
// pair and direction, the bidirectional single-path search must agree with the
// trusted one-directional BFS on whether a path exists and on its length, and the
// path it returns must be a valid simple walk with the right endpoints. The
// one-directional search is the M2 code the openCypher TCK already passes, so it
// is the oracle and the bidirectional search is the optimization under test.
func TestShortestBidiMatchesUnidirectional(t *testing.T) {
	dirs := []ast.Direction{ast.DirOut, ast.DirIn, ast.DirBoth}
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := 4 + rng.Intn(9) // 4..12 nodes
		e, nodes, adjOut := randomGraph(t, rng, n)
		rtx, err := e.Begin(false)
		if err != nil {
			t.Fatal(err)
		}
		ctx := &Ctx{Tx: rtx}
		for _, dir := range dirs {
			for si := range nodes {
				for di := range nodes {
					if si == di {
						continue
					}
					src, dst := nodes[si], nodes[di]
					refLen, refFound := refShortest(nodes, adjOut, src, dst, dir)
					// Unbounded, plus a few hop bounds that exercise the min/max guards.
					// The shortest distance is fixed; a bound only changes whether that
					// one shortest path qualifies, so the oracle result is refLen filtered
					// by the range.
					for _, b := range []struct{ min, max int }{{1, -1}, {1, 1}, {1, 2}, {2, 3}} {
						wantFound := refFound && refLen >= b.min && (b.max < 0 || refLen <= b.max)
						gotLen, gotFound, gotNodes, gotRels := runBidi(t, ctx, src, dst, dir, b.min, b.max)
						if gotFound != wantFound {
							t.Fatalf("seed %d dir %v %d->%d range %d..%d: bidi found=%v, want %v (oracle len=%d found=%v)",
								seed, dir, src, dst, b.min, b.max, gotFound, wantFound, refLen, refFound)
						}
						if !wantFound {
							continue
						}
						if gotLen != refLen {
							t.Fatalf("seed %d dir %v %d->%d range %d..%d: bidi len=%d, oracle len=%d",
								seed, dir, src, dst, b.min, b.max, gotLen, refLen)
						}
						validatePath(t, adjOut, src, dst, dir, gotNodes, gotRels, seed)
					}
				}
			}
		}
		_ = rtx.Abort()
	}
}

// randomGraph builds n nodes and a random set of directed KNOWS edges, returning
// the engine, the node ids, and a forward adjacency (src -> list of (dst, rel))
// the reference BFS and the path validator read without touching the engine.
func randomGraph(t *testing.T, rng *rand.Rand, n int) (*engine.MemEngine, []engine.NodeID, map[engine.NodeID][]edge) {
	t.Helper()
	e := engine.NewMemEngine()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]engine.NodeID, n)
	for i := range nodes {
		id, err := tx.CreateNode([]engine.Token{lblPerson})
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = id
	}
	adjOut := map[engine.NodeID][]edge{}
	// Each ordered pair gets an edge with some probability, so the graph spans the
	// range from sparse-and-disconnected to dense.
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			if a == b {
				continue
			}
			if rng.Intn(100) < 30 {
				r, err := tx.CreateRel(nodes[a], nodes[b], typKnows)
				if err != nil {
					t.Fatal(err)
				}
				adjOut[nodes[a]] = append(adjOut[nodes[a]], edge{nodes[b], r})
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return e, nodes, adjOut
}

type edge struct {
	to  engine.NodeID
	rel engine.RelID
}

// neighbors returns the nodes reachable from n along the given direction, as the
// engine's expand would, computed from the forward adjacency: outgoing follows
// edges as stored, incoming follows them reversed, both follows either.
func neighbors(adjOut map[engine.NodeID][]edge, n engine.NodeID, dir ast.Direction) []engine.NodeID {
	var out []engine.NodeID
	if dir == ast.DirOut || dir == ast.DirBoth {
		for _, e := range adjOut[n] {
			out = append(out, e.to)
		}
	}
	if dir == ast.DirIn || dir == ast.DirBoth {
		for src, es := range adjOut {
			for _, e := range es {
				if e.to == n {
					out = append(out, src)
				}
			}
		}
	}
	return out
}

// refShortest is the independent oracle: a plain BFS over the adjacency, returning
// the shortest hop count from src to dst and whether dst is reachable.
func refShortest(nodes []engine.NodeID, adjOut map[engine.NodeID][]edge, src, dst engine.NodeID, dir ast.Direction) (int, bool) {
	dist := map[engine.NodeID]int{src: 0}
	frontier := []engine.NodeID{src}
	for len(frontier) > 0 {
		var next []engine.NodeID
		for _, n := range frontier {
			for _, nb := range neighbors(adjOut, n, dir) {
				if _, seen := dist[nb]; !seen {
					dist[nb] = dist[n] + 1
					if nb == dst {
						return dist[nb], true
					}
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	d, ok := dist[dst]
	return d, ok
}

// runBidi runs the bidirectional search alone for one endpoint pair and returns
// the path length, whether a path was found, and the path's node and relationship
// sequences (read back from the bound path value).
func runBidi(t *testing.T, ctx *Ctx, src, dst engine.NodeID, dir ast.Direction, min, max int) (int, bool, []engine.NodeID, []engine.RelID) {
	t.Helper()
	op := &shortestPathOp{
		spec:   &plan.ShortestPath{From: "a", To: "b", Rel: "r", PathVar: "p", Dir: dir},
		ctx:    ctx,
		relTok: typKnows,
		min:    min,
		max:    max,
	}
	row := eval.Row{"a": value.Node(uint64(src)), "b": value.Node(uint64(dst))}
	if err := op.searchBidi(row, src, dst, nil); err != nil {
		t.Fatal(err)
	}
	if len(op.out) == 0 {
		return 0, false, nil, nil
	}
	if len(op.out) != 1 {
		t.Fatalf("bidi returned %d rows, want at most 1", len(op.out))
	}
	elems, _ := op.out[0]["p"].AsPath()
	var ns []engine.NodeID
	var rs []engine.RelID
	for i, el := range elems {
		if i%2 == 0 {
			id, _ := el.AsNode()
			ns = append(ns, engine.NodeID(id))
		} else {
			id, _ := el.AsRel()
			rs = append(rs, engine.RelID(id))
		}
	}
	return len(rs), true, ns, rs
}

// validatePath asserts the returned path is a simple walk from src to dst whose
// every step is a real edge in the searched direction.
func validatePath(t *testing.T, adjOut map[engine.NodeID][]edge, src, dst engine.NodeID, dir ast.Direction, ns []engine.NodeID, rs []engine.RelID, seed int64) {
	t.Helper()
	if len(ns) != len(rs)+1 {
		t.Fatalf("seed %d: path has %d nodes and %d rels", seed, len(ns), len(rs))
	}
	if ns[0] != src || ns[len(ns)-1] != dst {
		t.Fatalf("seed %d: path endpoints %d..%d, want %d..%d", seed, ns[0], ns[len(ns)-1], src, dst)
	}
	seen := map[engine.NodeID]bool{}
	for _, n := range ns {
		if seen[n] {
			t.Fatalf("seed %d: path repeats node %d", seed, n)
		}
		seen[n] = true
	}
	for i := range rs {
		if !edgeExists(adjOut, ns[i], ns[i+1], rs[i], dir) {
			t.Fatalf("seed %d: step %d (%d-[%d]->%d) is not a real edge", seed, i, ns[i], rs[i], ns[i+1])
		}
	}
}

// edgeExists reports whether rel connects a to b as a step in the given direction.
func edgeExists(adjOut map[engine.NodeID][]edge, a, b engine.NodeID, rel engine.RelID, dir ast.Direction) bool {
	if dir == ast.DirOut || dir == ast.DirBoth {
		for _, e := range adjOut[a] {
			if e.to == b && e.rel == rel {
				return true
			}
		}
	}
	if dir == ast.DirIn || dir == ast.DirBoth {
		for _, e := range adjOut[b] {
			if e.to == a && e.rel == rel {
				return true
			}
		}
	}
	return false
}
