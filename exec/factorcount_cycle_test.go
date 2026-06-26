package exec

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// cycleIntersectCount hand-builds the IntersectCount a directed n-cycle count plans to:
// a scan of a0 under n-2 plain KNOWS hops a0->a1->...->a_{n-2} (the anchor path), closed
// by two legs at the apex a_{n-1}, one out of a_{n-2} and one into a0. n=3 is the
// triangle (one hop), n=4 the four-cycle (two hops), and so on. This is the exact shape
// FusePolygonAnchor recognizes, so compiling it must yield the fused operator.
func cycleIntersectCount(n int) *plan.IntersectCount {
	knows := []bind.NameRef{{Token: typKnows, Known: true}}
	person := []bind.NameRef{{Token: lblPerson, Known: true}}
	vars := make([]string, n)
	for i := range vars {
		vars[i] = fmt.Sprintf("a%d", i)
	}
	var input plan.Op = &plan.NodeScan{Var: vars[0], Labels: person}
	for i := 0; i < n-2; i++ {
		input = &plan.Expand{
			Input: input, From: vars[i], To: vars[i+1], Rel: fmt.Sprintf("r%d", i),
			Types: knows, Dir: ast.DirOut,
		}
	}
	return &plan.IntersectCount{
		Input:  input,
		Var:    vars[n-1],
		Labels: person,
		Col:    "n",
		Legs: [2]plan.IntersectLeg{
			{From: vars[n-2], Rel: fmt.Sprintf("r%d", n-2), Types: knows, Dir: ast.DirOut},
			{From: vars[0], Rel: fmt.Sprintf("r%d", n-1), Types: knows, Dir: ast.DirIn},
		},
	}
}

// randomDirected builds n Person nodes and a set of distinct, loopless, random directed
// KNOWS edges, returning the engine and the adjacency the brute force reads. The same
// edges the engine sees are the edges the brute force counts over, so the two counts are
// comparable.
func randomDirected(t *testing.T, n, edges int, seed int64) (*engine.MemEngine, []engine.NodeID, map[engine.NodeID]map[engine.NodeID]bool) {
	t.Helper()
	e := engine.NewMemEngine()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]engine.NodeID, n)
	for i := range ids {
		id, err := tx.CreateNode([]engine.Token{lblPerson})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	adj := map[engine.NodeID]map[engine.NodeID]bool{}
	rng := rand.New(rand.NewSource(seed))
	for added := 0; added < edges; {
		s, d := ids[rng.Intn(n)], ids[rng.Intn(n)]
		if s == d {
			continue
		}
		if adj[s][d] {
			continue
		}
		if _, err := tx.CreateRel(s, d, typKnows); err != nil {
			t.Fatal(err)
		}
		if adj[s] == nil {
			adj[s] = map[engine.NodeID]bool{}
		}
		adj[s][d] = true
		added++
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return e, ids, adj
}

// bruteCycles counts directed n-cycles under Cypher relationship-isomorphism: tuples of n
// nodes whose n consecutive edges (wrapping) are all present and are n distinct edges. On
// a simple loopless graph a relationship is its ordered endpoint pair, so distinct
// relationships means distinct ordered pairs, which distinctEdges checks.
func bruteCycles(adj map[engine.NodeID]map[engine.NodeID]bool, ids []engine.NodeID, n int) int64 {
	var total int64
	path := make([]engine.NodeID, n)
	var rec func(level int)
	rec = func(level int) {
		switch {
		case level == n:
			if adj[path[n-1]][path[0]] && distinctEdges(path) {
				total++
			}
		case level == 0:
			for _, v := range ids {
				path[0] = v
				rec(1)
			}
		default:
			for nb := range adj[path[level-1]] {
				path[level] = nb
				rec(level + 1)
			}
		}
	}
	rec(0)
	return total
}

// distinctEdges reports whether the n wrapping edges of a cycle path are pairwise distinct
// ordered pairs, the relationship-isomorphism the engine enforces on a simple graph.
func distinctEdges(path []engine.NodeID) bool {
	n := len(path)
	for i := 0; i < n; i++ {
		ai, bi := path[i], path[(i+1)%n]
		for j := i + 1; j < n; j++ {
			if ai == path[j] && bi == path[(j+1)%n] {
				return false
			}
		}
	}
	return true
}

// TestFusedCycleCountsMatchBruteForce builds the IntersectCount for the four- and
// five-cycle, asserts each compiles to the fused operator (so the generalized anchor walk
// is the path under test, not the materialized fallback), executes it over a random
// directed graph, and checks the tally against a brute-force enumeration. The triangle is
// the one-hop control on the same machinery.
func TestFusedCycleCountsMatchBruteForce(t *testing.T) {
	for _, n := range []int{3, 4, 5} {
		t.Run(fmt.Sprintf("cycle%d", n), func(t *testing.T) {
			e, ids, adj := randomDirected(t, 40, 200, int64(n*101+7))
			ic := cycleIntersectCount(n)

			op, err := compile(ic)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if _, ok := op.(*fusedIntersectCountOp); !ok {
				t.Fatalf("%d-cycle compiled to %T, want *fusedIntersectCountOp", n, op)
			}

			got := runCount(t, e, ic)
			want := bruteCycles(adj, ids, n)
			if want == 0 {
				t.Fatalf("graph has no %d-cycles, test proves nothing", n)
			}
			if got != want {
				t.Fatalf("%d-cycle fused count = %d, brute force = %d", n, got, want)
			}
			t.Logf("%d-cycle: %d", n, want)
		})
	}
}

// runCount opens a hand-built count plan against the engine and returns its single tally.
func runCount(t *testing.T, e *engine.MemEngine, root plan.Op) int64 {
	t.Helper()
	tx, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()
	cur, err := Open(root, &Ctx{
		Tx:          tx,
		Params:      map[string]value.Value{},
		LabelName:   labelName,
		RelTypeName: relTypeName,
		PropKeyName: propKeyName,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cur.Close()
	row, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("no count row: ok=%v err=%v", ok, err)
	}
	v, _ := row["n"].AsInt()
	return v
}
