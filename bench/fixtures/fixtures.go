// Package fixtures provides deterministic graph generators for gr micro-benchmarks
// (doc 22 §8.5). Each generator produces a gr.DB on an in-memory VFS from a
// seed and a parameter struct, so a micro-benchmark fixture is reproducible from
// three numbers: generator name, seed, parameters.
//
// The generators are:
//   - Uniform: every node has the same out-degree d.
//   - PowerLaw: degree distribution follows a power law with configurable exponent.
//   - ErdosRenyi: random graph with edge probability p (known triangle count).
//   - Grid: 2-D grid graph with known diameter and shortest paths.
//
// Each generator also returns the fixture's ground truth (known node count,
// known edge count, known triangle count where applicable) so the calling
// benchmark can validate correctness as well as measure performance.
package fixtures

import (
	"fmt"
	"math"
	"math/rand"

	gr "github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// GraphFixture is a loaded graph database plus its ground truth.
type GraphFixture struct {
	DB            *gr.DB
	Nodes         int
	Edges         int
	TriangleCount int64 // -1 if not computed
	Diameter      int   // -1 if not computed
}

// Close releases the fixture database.
func (f *GraphFixture) Close() error {
	if f.DB == nil {
		return nil
	}
	return f.DB.Close()
}

// UniformParams configures the uniform-degree generator.
type UniformParams struct {
	Nodes  int // number of nodes
	Degree int // out-degree of every node
	Seed   int64
}

// BuildUniform creates a graph where every node has the same out-degree.
// Edges are drawn without self-loops; the adjacency is directed.
func BuildUniform(p UniformParams) (*GraphFixture, error) {
	db, err := openMemDB()
	if err != nil {
		return nil, err
	}

	rng := rand.New(rand.NewSource(p.Seed)) //nolint:gosec

	// Create all nodes.
	if err := bulkNodes(db, p.Nodes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("fixtures: uniform nodes: %w", err)
	}

	// Create edges: each node i points to d distinct random targets ≠ i.
	edges := 0
	for i := range p.Nodes {
		targets := randomTargets(rng, i, p.Nodes, p.Degree)
		for _, t := range targets {
			q := fmt.Sprintf("MATCH (a:N {i:%d}), (b:N {i:%d}) CREATE (a)-[:R]->(b)", i, t)
			if _, err := db.Exec(q, nil); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("fixtures: uniform edge (%d->%d): %w", i, t, err)
			}
			edges++
		}
	}

	return &GraphFixture{
		DB:            db,
		Nodes:         p.Nodes,
		Edges:         edges,
		TriangleCount: -1,
		Diameter:      -1,
	}, nil
}

// PowerLawParams configures the power-law graph generator.
type PowerLawParams struct {
	Nodes    int     // number of nodes
	Exponent float64 // Zipf exponent (> 1); higher = more skewed
	Seed     int64
}

// BuildPowerLaw creates a directed graph with a power-law out-degree distribution.
// Degrees are drawn from a Zipf distribution; each node gets that many random targets.
func BuildPowerLaw(p PowerLawParams) (*GraphFixture, error) {
	db, err := openMemDB()
	if err != nil {
		return nil, err
	}

	rng := rand.New(rand.NewSource(p.Seed)) //nolint:gosec
	zipf := rand.NewZipf(rng, p.Exponent, 1, uint64(p.Nodes/2))

	if err := bulkNodes(db, p.Nodes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("fixtures: power-law nodes: %w", err)
	}

	edges := 0
	for i := range p.Nodes {
		degree := int(zipf.Uint64()) + 1
		if degree > p.Nodes-1 {
			degree = p.Nodes - 1
		}
		targets := randomTargets(rng, i, p.Nodes, degree)
		for _, t := range targets {
			q := fmt.Sprintf("MATCH (a:N {i:%d}), (b:N {i:%d}) CREATE (a)-[:R]->(b)", i, t)
			if _, err := db.Exec(q, nil); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("fixtures: power-law edge (%d->%d): %w", i, t, err)
			}
			edges++
		}
	}

	return &GraphFixture{
		DB:            db,
		Nodes:         p.Nodes,
		Edges:         edges,
		TriangleCount: -1,
		Diameter:      -1,
	}, nil
}

// ErdosRenyiParams configures the Erdős–Rényi generator.
type ErdosRenyiParams struct {
	Nodes int     // number of nodes
	P     float64 // edge probability in [0,1]
	Seed  int64
}

// BuildErdosRenyi creates an undirected Erdős–Rényi G(n,p) random graph.
// The expected number of edges is n*(n-1)/2 * p. The expected triangle count
// is n*(n-1)*(n-2)/6 * p^3; it is stored as TriangleCount for validation.
func BuildErdosRenyi(p ErdosRenyiParams) (*GraphFixture, error) {
	db, err := openMemDB()
	if err != nil {
		return nil, err
	}

	rng := rand.New(rand.NewSource(p.Seed)) //nolint:gosec

	if err := bulkNodes(db, p.Nodes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("fixtures: er nodes: %w", err)
	}

	edges := 0
	for i := range p.Nodes {
		for j := i + 1; j < p.Nodes; j++ {
			if rng.Float64() < p.P {
				q := fmt.Sprintf("MATCH (a:N {i:%d}), (b:N {i:%d}) CREATE (a)-[:R]->(b), (b)-[:R]->(a)", i, j)
				if _, err := db.Exec(q, nil); err != nil {
					_ = db.Close()
					return nil, fmt.Errorf("fixtures: er edge (%d-%d): %w", i, j, err)
				}
				edges += 2
			}
		}
	}

	// Expected triangle count: C(n,3)*p^3.
	n := float64(p.Nodes)
	expectedTriangles := int64(math.Round(n * (n - 1) * (n - 2) / 6 * math.Pow(p.P, 3)))

	return &GraphFixture{
		DB:            db,
		Nodes:         p.Nodes,
		Edges:         edges,
		TriangleCount: expectedTriangles,
		Diameter:      -1,
	}, nil
}

// GridParams configures the 2-D grid generator.
type GridParams struct {
	Rows int
	Cols int
	Seed int64
}

// BuildGrid creates a 2-D grid graph: node (r,c) has edges to (r±1,c) and (r,c±1).
// The diameter is rows+cols-2 (longest shortest path from corner to corner).
func BuildGrid(p GridParams) (*GraphFixture, error) {
	db, err := openMemDB()
	if err != nil {
		return nil, err
	}

	n := p.Rows * p.Cols
	// Create nodes with row/col properties.
	for r := range p.Rows {
		for c := range p.Cols {
			i := r*p.Cols + c
			q := fmt.Sprintf("CREATE (:N {i:%d, r:%d, c:%d})", i, r, c)
			if _, err := db.Exec(q, nil); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("fixtures: grid node (%d,%d): %w", r, c, err)
			}
		}
	}

	// Create edges (directed, both directions to make it undirected).
	edges := 0
	addEdge := func(r1, c1, r2, c2 int) error {
		i1, i2 := r1*p.Cols+c1, r2*p.Cols+c2
		q := fmt.Sprintf("MATCH (a:N {i:%d}), (b:N {i:%d}) CREATE (a)-[:R]->(b)", i1, i2)
		if _, err := db.Exec(q, nil); err != nil {
			return fmt.Errorf("fixtures: grid edge (%d->%d): %w", i1, i2, err)
		}
		edges++
		return nil
	}

	for r := range p.Rows {
		for c := range p.Cols {
			if r+1 < p.Rows {
				if err := addEdge(r, c, r+1, c); err != nil {
					_ = db.Close()
					return nil, err
				}
				if err := addEdge(r+1, c, r, c); err != nil {
					_ = db.Close()
					return nil, err
				}
			}
			if c+1 < p.Cols {
				if err := addEdge(r, c, r, c+1); err != nil {
					_ = db.Close()
					return nil, err
				}
				if err := addEdge(r, c+1, r, c); err != nil {
					_ = db.Close()
					return nil, err
				}
			}
		}
	}

	return &GraphFixture{
		DB:            db,
		Nodes:         n,
		Edges:         edges,
		TriangleCount: 0, // grids have no triangles
		Diameter:      p.Rows + p.Cols - 2,
	}, nil
}

// openMemDB opens a fresh in-memory gr database.
func openMemDB() (*gr.DB, error) {
	return gr.Open("fixture.gr", gr.Options{VFS: vfs.NewMem()})
}

// bulkNodes creates n nodes labelled :N with a sequential integer property i.
// Each is a separate transaction (not a bulk load) so the fixture DB is simple.
func bulkNodes(db *gr.DB, n int) error {
	for i := range n {
		if _, err := db.Exec(fmt.Sprintf("CREATE (:N {i:%d})", i), nil); err != nil {
			return err
		}
	}
	return nil
}

// randomTargets returns `count` distinct random integers in [0, n) excluding `exclude`.
func randomTargets(rng *rand.Rand, exclude, n, count int) []int {
	if count > n-1 {
		count = n - 1
	}
	seen := make(map[int]bool, count)
	out := make([]int, 0, count)
	for len(out) < count {
		t := rng.Intn(n)
		if t != exclude && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
