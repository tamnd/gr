package exec

import (
	"sort"
	"testing"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// --- catalog and tokens shared by the test graph ---

const (
	lblPerson engine.Token = 1
	lblCity   engine.Token = 2
	typKnows  engine.Token = 1
	typLikes  engine.Token = 2
	keyName   engine.Token = 1
	keyAge    engine.Token = 2
	keySince  engine.Token = 3
)

// labelName, relTypeName, and propKeyName are the reverse resolvers (token to
// name) the entity functions read; they invert the testCatalog tokens.
func labelName(t engine.Token) (string, bool) {
	switch t {
	case lblPerson:
		return "Person", true
	case lblCity:
		return "City", true
	}
	return "", false
}

func relTypeName(t engine.Token) (string, bool) {
	switch t {
	case typKnows:
		return "KNOWS", true
	case typLikes:
		return "LIKES", true
	}
	return "", false
}

func propKeyName(t engine.Token) (string, bool) {
	switch t {
	case keyName:
		return "name", true
	case keyAge:
		return "age", true
	case keySince:
		return "since", true
	}
	return "", false
}

// testCatalog resolves the names the test queries use to the tokens the test graph
// is built with.
type testCatalog struct{}

func (testCatalog) LabelToken(n string) (engine.Token, bool) {
	switch n {
	case "Person":
		return lblPerson, true
	case "City":
		return lblCity, true
	}
	return 0, false
}
func (testCatalog) RelTypeToken(n string) (engine.Token, bool) {
	switch n {
	case "KNOWS":
		return typKnows, true
	case "LIKES":
		return typLikes, true
	}
	return 0, false
}
func (testCatalog) PropKeyToken(n string) (engine.Token, bool) {
	switch n {
	case "name":
		return keyName, true
	case "age":
		return keyAge, true
	case "since":
		return keySince, true
	}
	return 0, false
}

// graph builds a small social graph: Alice(30) -KNOWS-> Bob(25), Bob -KNOWS->
// Carol(35), Alice -KNOWS-> Carol. Returns the engine and the node ids by name.
func graph(t *testing.T) (*engine.MemEngine, map[string]engine.NodeID) {
	t.Helper()
	e := engine.NewMemEngine()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]engine.NodeID{}
	mk := func(name string, age int64) engine.NodeID {
		id, err := tx.CreateNode([]engine.Token{lblPerson})
		if err != nil {
			t.Fatal(err)
		}
		tx.SetNodeProperty(id, keyName, value.String(name))
		tx.SetNodeProperty(id, keyAge, value.Int(age))
		ids[name] = id
		return id
	}
	a, b, c := mk("Alice", 30), mk("Bob", 25), mk("Carol", 35)
	knows := func(s, d engine.NodeID, since int64) {
		r, err := tx.CreateRel(s, d, typKnows)
		if err != nil {
			t.Fatal(err)
		}
		tx.SetRelProperty(r, keySince, value.Int(since))
	}
	knows(a, b, 2015)
	knows(b, c, 2016)
	knows(a, c, 2017)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return e, ids
}

// run executes a query against the graph and returns its rows.
func run(t *testing.T, e *engine.MemEngine, cypher string, params map[string]value.Value) ([]eval.Row, []string) {
	t.Helper()
	q, err := parse.Parse(cypher)
	if err != nil {
		t.Fatalf("parse %q: %v", cypher, err)
	}
	b, err := bind.Bind(q, testCatalog{}, false)
	if err != nil {
		t.Fatalf("bind %q: %v", cypher, err)
	}
	root := plan.Plan(b)
	tx, err := e.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()
	if params == nil {
		params = map[string]value.Value{}
	}
	cur, err := Open(root, &Ctx{
		Tx:          tx,
		Params:      params,
		Resolve:     ResolverFromBound(b),
		LabelName:   labelName,
		RelTypeName: relTypeName,
		PropKeyName: propKeyName,
	})
	if err != nil {
		t.Fatalf("open %q: %v", cypher, err)
	}
	defer cur.Close()
	var rows []eval.Row
	for {
		row, ok, err := cur.Next()
		if err != nil {
			t.Fatalf("next %q: %v", cypher, err)
		}
		if !ok {
			break
		}
		rows = append(rows, row)
	}
	return rows, cur.Cols()
}

// strCol pulls a string column from every row, sorted, for set comparison.
func strCol(rows []eval.Row, col string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if s, ok := r[col].AsString(); ok {
			out = append(out, s)
		} else if r[col].IsNull() {
			out = append(out, "<null>")
		}
	}
	sort.Strings(out)
	return out
}

func eqStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNodeScan(t *testing.T) {
	e, _ := graph(t)
	rows, cols := run(t, e, "MATCH (n:Person) RETURN n.name AS name", nil)
	if len(cols) != 1 || cols[0] != "name" {
		t.Fatalf("cols = %v", cols)
	}
	eqStrings(t, strCol(rows, "name"), []string{"Alice", "Bob", "Carol"})
}

func TestNodeScanUnknownLabel(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:Ghost) RETURN n.name AS name", nil)
	if len(rows) != 0 {
		t.Fatalf("unknown label should scan nothing, got %d rows", len(rows))
	}
}

func TestExpand(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (a:Person)-[:KNOWS]->(b) RETURN a.name AS a, b.name AS b", nil)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	pairs := map[string]bool{}
	for _, r := range rows {
		a, _ := r["a"].AsString()
		b, _ := r["b"].AsString()
		pairs[a+"->"+b] = true
	}
	for _, want := range []string{"Alice->Bob", "Bob->Carol", "Alice->Carol"} {
		if !pairs[want] {
			t.Fatalf("missing pair %s in %v", want, pairs)
		}
	}
}

func TestFilter(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:Person) WHERE n.age > 28 RETURN n.name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Alice", "Carol"})
}

func TestFilterParam(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:Person) WHERE n.age >= $min RETURN n.name AS name",
		map[string]value.Value{"min": value.Int(30)})
	eqStrings(t, strCol(rows, "name"), []string{"Alice", "Carol"})
}

func TestTwoHopRelUniqueness(t *testing.T) {
	e, _ := graph(t)
	// a-KNOWS->b-KNOWS->c with distinct relationships. Only Alice-Bob-Carol fits.
	rows, _ := run(t, e, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c) RETURN a.name AS a, b.name AS b, c.name AS c", nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %v", len(rows), rows)
	}
	if a, _ := rows[0]["a"].AsString(); a != "Alice" {
		t.Fatalf("a = %v", rows[0]["a"])
	}
	if c, _ := rows[0]["c"].AsString(); c != "Carol" {
		t.Fatalf("c = %v", rows[0]["c"])
	}
}

func TestPropertyConstraint(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, `MATCH (n:Person {name: "Bob"}) RETURN n.age AS age`, nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if a, _ := rows[0]["age"].AsInt(); a != 25 {
		t.Fatalf("age = %v", rows[0]["age"])
	}
}

func TestAggregateCount(t *testing.T) {
	e, _ := graph(t)
	rows, cols := run(t, e, "MATCH (a:Person)-[:KNOWS]->(b) RETURN a.name AS name, count(*) AS c ORDER BY name", nil)
	if len(cols) != 2 || cols[0] != "name" || cols[1] != "c" {
		t.Fatalf("cols = %v", cols)
	}
	want := map[string]int64{"Alice": 2, "Bob": 1}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		n, _ := r["name"].AsString()
		c, _ := r["c"].AsInt()
		if want[n] != c {
			t.Fatalf("%s: got %d, want %d", n, c, want[n])
		}
	}
}

func TestAggregateGlobalCountOverEmpty(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:City) RETURN count(*) AS c", nil)
	if len(rows) != 1 {
		t.Fatalf("global count should yield one row, got %d", len(rows))
	}
	if c, _ := rows[0]["c"].AsInt(); c != 0 {
		t.Fatalf("count = %v, want 0", rows[0]["c"])
	}
}

func TestAggregateSumAvgCollect(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:Person) RETURN sum(n.age) AS s, avg(n.age) AS a, count(n) AS c", nil)
	r := rows[0]
	if s, _ := r["s"].AsInt(); s != 90 {
		t.Fatalf("sum = %v, want 90", r["s"])
	}
	if a, _ := r["a"].AsFloat(); a != 30 {
		t.Fatalf("avg = %v, want 30", r["a"])
	}
	if c, _ := r["c"].AsInt(); c != 3 {
		t.Fatalf("count = %v, want 3", r["c"])
	}
}

func TestCollectDistinct(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (a:Person)-[:KNOWS]->(b) RETURN collect(DISTINCT b.name) AS names", nil)
	lst, ok := rows[0]["names"].AsList()
	if !ok {
		t.Fatalf("names not a list: %v", rows[0]["names"])
	}
	got := make([]string, 0, len(lst))
	for _, v := range lst {
		s, _ := v.AsString()
		got = append(got, s)
	}
	sort.Strings(got)
	eqStrings(t, got, []string{"Bob", "Carol"})
}

func TestOrderSkipLimit(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:Person) RETURN n.name AS name, n.age AS age ORDER BY age DESC LIMIT 2", nil)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if n, _ := rows[0]["name"].AsString(); n != "Carol" {
		t.Fatalf("first = %v, want Carol", rows[0]["name"])
	}
	if n, _ := rows[1]["name"].AsString(); n != "Alice" {
		t.Fatalf("second = %v, want Alice", rows[1]["name"])
	}
}

func TestDistinct(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (a:Person)-[:KNOWS]->(b) RETURN DISTINCT a.name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Alice", "Bob"})
}

func TestUnwind(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "UNWIND [10, 20, 30] AS x RETURN x AS x", nil)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	var sum int64
	for _, r := range rows {
		i, _ := r["x"].AsInt()
		sum += i
	}
	if sum != 60 {
		t.Fatalf("sum = %d, want 60", sum)
	}
}

func TestOptionalMatch(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "MATCH (n:Person) OPTIONAL MATCH (n)-[:KNOWS]->(m) RETURN n.name AS n, m.name AS m", nil)
	// Alice->Bob, Alice->Carol, Bob->Carol, Carol->(null) = 4 rows.
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4: %v", len(rows), rows)
	}
	var nullCount int
	for _, r := range rows {
		if r["m"].IsNull() {
			nullCount++
			if n, _ := r["n"].AsString(); n != "Carol" {
				t.Fatalf("null match on %v, want Carol", r["n"])
			}
		}
	}
	if nullCount != 1 {
		t.Fatalf("got %d null matches, want 1", nullCount)
	}
}

func TestUnion(t *testing.T) {
	e, _ := graph(t)
	all, _ := run(t, e, "RETURN 1 AS x UNION ALL RETURN 1 AS x", nil)
	if len(all) != 2 {
		t.Fatalf("UNION ALL got %d rows, want 2", len(all))
	}
	dedup, _ := run(t, e, "RETURN 1 AS x UNION RETURN 1 AS x", nil)
	if len(dedup) != 1 {
		t.Fatalf("UNION got %d rows, want 1", len(dedup))
	}
}

func TestReturnConstant(t *testing.T) {
	e, _ := graph(t)
	rows, _ := run(t, e, "RETURN 1 + 2 AS x", nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if x, _ := rows[0]["x"].AsInt(); x != 3 {
		t.Fatalf("x = %v, want 3", rows[0]["x"])
	}
}

func TestVarLengthRange(t *testing.T) {
	e, _ := graph(t)
	// From Alice over 1..2 KNOWS hops: Alice->Bob, Alice->Carol, Alice->Bob->Carol.
	// Each trail is one row, so Carol appears twice.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'Alice'})-[:KNOWS*1..2]->(b) RETURN b.name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Bob", "Carol", "Carol"})
}

func TestVarLengthZeroHop(t *testing.T) {
	e, _ := graph(t)
	// *0..1 includes the zero-hop path (a equals b), so Alice reaches herself.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'Alice'})-[:KNOWS*0..1]->(b) RETURN b.name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Alice", "Bob", "Carol"})
}

func TestVarLengthExact(t *testing.T) {
	e, _ := graph(t)
	// Exactly two hops from Alice: only Alice->Bob->Carol, and the relationship
	// variable binds the two-edge path.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'Alice'})-[r:KNOWS*2..2]->(b) RETURN b.name AS name, size(r) AS hops", nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if name, _ := rows[0]["name"].AsString(); name != "Carol" {
		t.Fatalf("name = %v, want Carol", rows[0]["name"])
	}
	if hops, _ := rows[0]["hops"].AsInt(); hops != 2 {
		t.Fatalf("hops = %v, want 2", rows[0]["hops"])
	}
}

func TestVarLengthUnbounded(t *testing.T) {
	e, _ := graph(t)
	// Unbounded * terminates because relationship-uniqueness forbids reusing an
	// edge: Alice->Bob, Alice->Carol, Alice->Bob->Carol.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'Alice'})-[:KNOWS*]->(b) RETURN b.name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Bob", "Carol", "Carol"})
}

func TestVarLengthCycleTerminates(t *testing.T) {
	// A two-node cycle: Ann -KNOWS-> Ben -KNOWS-> Ann (two distinct edges).
	e := engine.NewMemEngine()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	ann, _ := tx.CreateNode([]engine.Token{lblPerson})
	ben, _ := tx.CreateNode([]engine.Token{lblPerson})
	tx.SetNodeProperty(ann, keyName, value.String("Ann"))
	tx.SetNodeProperty(ben, keyName, value.String("Ben"))
	r1, _ := tx.CreateRel(ann, ben, typKnows)
	r2, _ := tx.CreateRel(ben, ann, typKnows)
	_, _ = r1, r2
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Unbounded from Ann: Ann->Ben, then Ben->Ann (the other edge); the first edge
	// cannot be reused, so the walk stops. Two trails: to Ben, back to Ann.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'Ann'})-[:KNOWS*]->(b) RETURN b.name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Ann", "Ben"})
}

func TestEntityFunctions(t *testing.T) {
	e, _ := graph(t)
	// labels() names a node's labels through the reverse resolver.
	rows, _ := run(t, e, "MATCH (n:Person {name: 'Alice'}) RETURN labels(n)[0] AS l", nil)
	eqStrings(t, strCol(rows, "l"), []string{"Person"})

	// type() names a relationship's type.
	rows, _ = run(t, e, "MATCH (:Person {name: 'Alice'})-[r:KNOWS]->() RETURN type(r) AS t", nil)
	eqStrings(t, strCol(rows, "t"), []string{"KNOWS", "KNOWS"})

	// keys() returns a node's property keys, sorted.
	rows, _ = run(t, e, "MATCH (n:Person {name: 'Bob'}) RETURN keys(n)[0] AS k", nil)
	eqStrings(t, strCol(rows, "k"), []string{"age"})

	// properties() returns a node's property map, read back through indexing.
	rows, _ = run(t, e, "MATCH (n:Person {name: 'Carol'}) RETURN properties(n).name AS name", nil)
	eqStrings(t, strCol(rows, "name"), []string{"Carol"})
}

func TestNamedPath(t *testing.T) {
	e, _ := graph(t)
	// A two-hop named path Alice-KNOWS->?-KNOWS->? exists (Alice->Bob->Carol).
	rows, _ := run(t, e, "MATCH p = (:Person {name: 'Alice'})-[:KNOWS]->()-[:KNOWS]->() RETURN length(p) AS len", nil)
	if len(rows) != 1 {
		t.Fatalf("want 1 two-hop path from Alice, got %d", len(rows))
	}
	if l, _ := rows[0]["len"].AsInt(); l != 2 {
		t.Fatalf("length(p) = %s, want 2", rows[0]["len"])
	}

	// nodes(p) and relationships(p) of the one-hop paths from Alice.
	rows, _ = run(t, e, "MATCH p = (:Person {name: 'Alice'})-[:KNOWS]->(b) RETURN size(nodes(p)) AS n, size(relationships(p)) AS r", nil)
	for _, row := range rows {
		if n, _ := row["n"].AsInt(); n != 2 {
			t.Fatalf("size(nodes(p)) = %s, want 2", row["n"])
		}
		if r, _ := row["r"].AsInt(); r != 1 {
			t.Fatalf("size(relationships(p)) = %s, want 1", row["r"])
		}
	}
}

func TestShortestPath(t *testing.T) {
	e, ids := graph(t)
	// Alice reaches Carol directly (length 1) and via Bob (length 2); the shortest
	// is the direct edge, so one path of length 1 is returned.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Carol'}) "+
		"MATCH p = shortestPath((a)-[:KNOWS*]->(b)) RETURN length(p) AS len", nil)
	if len(rows) != 1 {
		t.Fatalf("want 1 shortest path Alice->Carol, got %d", len(rows))
	}
	if l, _ := rows[0]["len"].AsInt(); l != 1 {
		t.Fatalf("length(p) = %s, want 1", rows[0]["len"])
	}

	// The path's endpoints are Alice and Carol.
	rows, _ = run(t, e, "MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Carol'}) "+
		"MATCH p = shortestPath((a)-[:KNOWS*]->(b)) RETURN nodes(p) AS ns", nil)
	ns, _ := rows[0]["ns"].AsList()
	if len(ns) != 2 {
		t.Fatalf("nodes(p) has %d nodes, want 2", len(ns))
	}
	first, _ := ns[0].AsNode()
	last, _ := ns[len(ns)-1].AsNode()
	if engine.NodeID(first) != ids["Alice"] || engine.NodeID(last) != ids["Carol"] {
		t.Fatalf("path endpoints = %d..%d, want %d..%d", first, last, ids["Alice"], ids["Carol"])
	}
}

func TestAllShortestPaths(t *testing.T) {
	// A diamond: A -> B -> D and A -> C -> D. Two distinct shortest paths of
	// length 2 from A to D.
	e := engine.NewMemEngine()
	tx, err := e.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) engine.NodeID {
		id, _ := tx.CreateNode([]engine.Token{lblPerson})
		tx.SetNodeProperty(id, keyName, value.String(name))
		return id
	}
	a, b, c, d := mk("A"), mk("B"), mk("C"), mk("D")
	for _, e := range [][2]engine.NodeID{{a, b}, {a, c}, {b, d}, {c, d}} {
		if _, err := tx.CreateRel(e[0], e[1], typKnows); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// allShortestPaths returns both length-2 paths.
	rows, _ := run(t, e, "MATCH (a:Person {name: 'A'}), (d:Person {name: 'D'}) "+
		"MATCH p = allShortestPaths((a)-[:KNOWS*]->(d)) RETURN length(p) AS len", nil)
	if len(rows) != 2 {
		t.Fatalf("want 2 shortest paths A->D, got %d", len(rows))
	}
	for _, row := range rows {
		if l, _ := row["len"].AsInt(); l != 2 {
			t.Fatalf("length(p) = %s, want 2", row["len"])
		}
	}

	// shortestPath returns just one of them.
	rows, _ = run(t, e, "MATCH (a:Person {name: 'A'}), (d:Person {name: 'D'}) "+
		"MATCH p = shortestPath((a)-[:KNOWS*]->(d)) RETURN length(p) AS len", nil)
	if len(rows) != 1 {
		t.Fatalf("want 1 shortest path A->D, got %d", len(rows))
	}
}
