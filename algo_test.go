package gr

import (
	"math"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// buildAlgoGraph creates a small directed graph for the native graph-algorithm
// functions: a three-node directed cycle 0->1->2->0 and a separate 3->4 edge, so
// the result of every algorithm is small enough to check by hand. The id is
// stored as a string property, the shape the bulk loader produces and the algo
// functions read back.
func buildAlgoGraph(t *testing.T) *DB {
	t.Helper()
	db, err := Open("algo.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	const create = `CREATE (n0:Node {id:'0'}), (n1:Node {id:'1'}), (n2:Node {id:'2'}),
	(n3:Node {id:'3'}), (n4:Node {id:'4'}),
	(n0)-[:EDGE]->(n1), (n1)-[:EDGE]->(n2), (n2)-[:EDGE]->(n0), (n3)-[:EDGE]->(n4)`
	if _, err := db.Exec(create, nil); err != nil {
		_ = db.Close()
		t.Fatalf("create: %v", err)
	}
	return db
}

// algoRows runs a two-column id/metric query and returns the rows as a map from
// the integer id to the metric value, plus the row count.
func algoRows(t *testing.T, db *DB, q string) (map[int64]float64, int) {
	t.Helper()
	res, err := db.Query(q, nil)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	out := map[int64]float64{}
	count := 0
	for {
		row, ok, err := res.Row()
		if err != nil {
			t.Fatalf("rows: %v", err)
		}
		if !ok {
			break
		}
		id, ok := row[0].AsInt()
		if !ok {
			t.Fatalf("id column not an int: %v", row[0])
		}
		var m float64
		if f, ok := row[1].AsFloat(); ok {
			m = f
		} else if n, ok := row[1].AsInt(); ok {
			m = float64(n)
		} else {
			t.Fatalf("metric column not numeric: %v", row[1])
		}
		out[id] = m
		count++
	}
	return out, count
}

func TestAlgoBFS(t *testing.T) {
	db := buildAlgoGraph(t)
	defer func() { _ = db.Close() }()
	got, n := algoRows(t, db,
		`UNWIND algo_bfs('id', '0') AS row RETURN row.id AS id, row.level AS level ORDER BY id`)
	if n != 3 {
		t.Fatalf("bfs row count = %d, want 3 (only the cycle is reachable from 0)", n)
	}
	want := map[int64]float64{0: 0, 1: 1, 2: 2}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("bfs level[%d] = %v, want %v", id, got[id], w)
		}
	}
}

func TestAlgoSSSP(t *testing.T) {
	db := buildAlgoGraph(t)
	defer func() { _ = db.Close() }()
	got, n := algoRows(t, db,
		`UNWIND algo_sssp('id', '0') AS row RETURN row.id AS id, row.distance AS distance ORDER BY id`)
	if n != 3 {
		t.Fatalf("sssp row count = %d, want 3", n)
	}
	want := map[int64]float64{0: 0, 1: 1, 2: 2}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("sssp distance[%d] = %v, want %v", id, got[id], w)
		}
	}
}

func TestAlgoWCC(t *testing.T) {
	db := buildAlgoGraph(t)
	defer func() { _ = db.Close() }()
	got, n := algoRows(t, db,
		`UNWIND algo_wcc('id') AS row RETURN row.id AS id, row.component AS component ORDER BY id`)
	if n != 5 {
		t.Fatalf("wcc row count = %d, want 5", n)
	}
	// The cycle {0,1,2} is one component labeled 0; {3,4} is another labeled 3.
	want := map[int64]float64{0: 0, 1: 0, 2: 0, 3: 3, 4: 3}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("wcc component[%d] = %v, want %v", id, got[id], w)
		}
	}
}

func TestAlgoLCC(t *testing.T) {
	db := buildAlgoGraph(t)
	defer func() { _ = db.Close() }()
	got, n := algoRows(t, db,
		`UNWIND algo_lcc('id') AS row RETURN row.id AS id, row.coefficient AS coefficient ORDER BY id`)
	if n != 5 {
		t.Fatalf("lcc row count = %d, want 5", n)
	}
	// Each cycle node has two undirected neighbors with one directed edge between
	// them, so coefficient 1/(2*1) = 0.5; the 3->4 pair has degree 1, coefficient 0.
	want := map[int64]float64{0: 0.5, 1: 0.5, 2: 0.5, 3: 0, 4: 0}
	for id, w := range want {
		if math.Abs(got[id]-w) > 1e-9 {
			t.Errorf("lcc coefficient[%d] = %v, want %v", id, got[id], w)
		}
	}
}

func TestAlgoCDLP(t *testing.T) {
	db := buildAlgoGraph(t)
	defer func() { _ = db.Close() }()
	_, n := algoRows(t, db,
		`UNWIND algo_cdlp('id', 10) AS row RETURN row.id AS id, row.community AS community ORDER BY id`)
	if n != 5 {
		t.Fatalf("cdlp row count = %d, want 5", n)
	}
}

func TestAlgoPageRank(t *testing.T) {
	db := buildAlgoGraph(t)
	defer func() { _ = db.Close() }()
	got, n := algoRows(t, db,
		`UNWIND algo_pagerank('id', 0.85, 100) AS row RETURN row.id AS id, row.score AS score ORDER BY id`)
	if n != 5 {
		t.Fatalf("pagerank row count = %d, want 5", n)
	}
	var sum float64
	for id, s := range got {
		if s <= 0 {
			t.Errorf("pagerank score[%d] = %v, want positive", id, s)
		}
		sum += s
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Errorf("pagerank scores sum to %v, want 1.0", sum)
	}
	// The three cycle nodes are symmetric, so they share one score, above the two
	// nodes on the pendant edge.
	if math.Abs(got[0]-got[1]) > 1e-9 || math.Abs(got[1]-got[2]) > 1e-9 {
		t.Errorf("cycle scores not equal: %v %v %v", got[0], got[1], got[2])
	}
}
