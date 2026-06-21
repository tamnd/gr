package plan

import (
	"testing"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/parse"
)

// fakeCatalog is a fixed name→token resolver, the binder's catalog seam.
type fakeCatalog struct {
	labels, relTypes, propKeys map[string]engine.Token
}

func newCatalog() *fakeCatalog {
	return &fakeCatalog{
		labels:   map[string]engine.Token{"Person": 1, "Movie": 2},
		relTypes: map[string]engine.Token{"KNOWS": 1, "ACTED_IN": 2},
		propKeys: map[string]engine.Token{"name": 1, "age": 2, "title": 3},
	}
}

func (c *fakeCatalog) LabelToken(n string) (engine.Token, bool) { t, ok := c.labels[n]; return t, ok }
func (c *fakeCatalog) RelTypeToken(n string) (engine.Token, bool) {
	t, ok := c.relTypes[n]
	return t, ok
}
func (c *fakeCatalog) PropKeyToken(n string) (engine.Token, bool) {
	t, ok := c.propKeys[n]
	return t, ok
}

func bound(t *testing.T, src string) *bind.Bound {
	t.Helper()
	q, err := parse.Parse(src)
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	b, err := bind.Bind(q, newCatalog(), false)
	if err != nil {
		t.Fatalf("bind(%q): %v", src, err)
	}
	return b
}

func eq(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", label, got, want)
	}
}

func TestBuildFriendsOfFriends(t *testing.T) {
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c) RETURN c.name AS name")
	want := `Project c.name AS name
  Expand b -[@r1:#1]-> c
    Expand a -[@r0:#1]-> b
      NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestPushdownToScan(t *testing.T) {
	b := bound(t, "MATCH (p:Person)-[:KNOWS]->(f) WHERE p.age > 30 RETURN f")
	eq(t, "raw", String(Build(b)), `Project f
  Filter p.age > 30
    Expand p -[@r0:#1]-> f
      NodeScan p:#1
`)
	eq(t, "normalized", String(Plan(b)), `Project f
  Expand p -[@r0:#1]-> f
    Filter p.age > 30
      NodeScan p:#1
`)
}

func TestSplitAndPush(t *testing.T) {
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b) WHERE a.age > 1 AND b.age > 2 RETURN a")
	// a.age pushes to a's scan; b.age cannot pass the expand that produces b.
	eq(t, "normalized", String(Plan(b)), `Project a
  Filter b.age > 2
    Expand a -[@r0:#1]-> b
      Filter a.age > 1
        NodeScan a:#1
`)
}

func TestNotComparison(t *testing.T) {
	b := bound(t, "MATCH (a:Person) WHERE NOT (a.age > 30) RETURN a")
	eq(t, "normalized", String(Plan(b)), `Project a
  Filter a.age <= 30
    NodeScan a:#1
`)
}

func TestDeMorgan(t *testing.T) {
	b := bound(t, `MATCH (a:Person) WHERE NOT (a.name = "x" AND a.age = 2) RETURN a`)
	eq(t, "normalized", String(Plan(b)), `Project a
  Filter (a.name <> "x") OR (a.age <> 2)
    NodeScan a:#1
`)
}

func TestOptionalMatch(t *testing.T) {
	b := bound(t, "MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a, b")
	eq(t, "raw", String(Build(b)), `Project a, b
  Optional
    NodeScan a:#1
    Expand a -[@r0:#1]-> b
      Argument [a]
`)
}

func TestUnion(t *testing.T) {
	b := bound(t, "MATCH (a:Person) RETURN a.name AS name UNION MATCH (m:Movie) RETURN m.title AS name")
	eq(t, "raw", String(Build(b)), `Union
  Project a.name AS name
    NodeScan a:#1
  Project m.title AS name
    NodeScan m:#2
`)
}

func TestAggregate(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p.name, count(*)")
	eq(t, "raw", String(Build(b)), `Aggregate by[p.name] agg[count(*)]
  NodeScan p:#1
`)
}

func TestLeadingUnwind(t *testing.T) {
	b := bound(t, "UNWIND [1, 2, 3] AS x RETURN x")
	eq(t, "raw", String(Build(b)), `Project x
  Unwind [1, 2, 3] AS x
`)
}

func TestPropertyMap(t *testing.T) {
	b := bound(t, "MATCH (p:Person {name: $n}) RETURN p")
	eq(t, "raw", String(Build(b)), `Project p
  Filter p.name = $n
    NodeScan p:#1
`)
}

func TestExpandInto(t *testing.T) {
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b), (c:Person)-[:KNOWS]->(b) RETURN a")
	// The second pattern's b is already bound, so its expand is an expand-into.
	eq(t, "raw", String(Build(b)), `Project a
  Expand c -[@r1:#1]-> b (into)
    Join on[]
      Expand a -[@r0:#1]-> b
        NodeScan a:#1
      NodeScan c:#1
`)
}

func TestReanchorOnPinnedLabel(t *testing.T) {
	// (a) is unlabeled, (b) is labeled and pinned by name = $x, so the planner
	// anchors at b and reverses the expand to radiate from it; Normalize then
	// pushes the pin to b's scan.
	b := bound(t, "MATCH (a)-[:KNOWS]->(b:Person {name: $x}) RETURN a")
	eq(t, "normalized", String(Plan(b)), `Project a
  Expand b <-[@r0:#1]- a
    Filter b.name = $x
      NodeScan b:#1
`)
}

func TestReanchorLabeledEnd(t *testing.T) {
	// The labeled end is the cheaper scan, so the planner anchors there even with
	// no property pin.
	b := bound(t, "MATCH (a)-[:KNOWS]->(b:Person) RETURN a")
	eq(t, "normalized", String(Plan(b)), `Project a
  Expand b <-[@r0:#1]- a
    NodeScan b:#1
`)
}

func TestNoReanchorWhenLeftmostBest(t *testing.T) {
	// The leftmost node is already the most selective, so the plan is unchanged.
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b) RETURN b")
	eq(t, "normalized", String(Plan(b)), `Project b
  Expand a -[@r0:#1]-> b
    NodeScan a:#1
`)
}

func TestCostAnchorsRarerLabel(t *testing.T) {
	// Both ends are labeled, so the structural proxy ties and Plan keeps the leftmost
	// anchor. With statistics where Movie is far rarer than Person the cost model
	// re-anchors at the Movie end, the scan that reads the fewest nodes, and reverses
	// the expand to radiate from it.
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b:Movie) RETURN a")
	st := fakeStats{nodes: 1010, rels: 2000, label: map[uint32]float64{1: 1000, 2: 10}}
	eq(t, "cost", String(PlanWithStats(b, st)), `Project a
  Expand b <-[@r0:#1]- a:#1
    NodeScan b:#2
`)
}

func TestCostKeepsLeftmostWhenCheaper(t *testing.T) {
	// The same pattern, but now the leftmost Person end is the rarer scan, so the cost
	// model keeps the anchor where Build placed it, the plan structural Plan also
	// produces.
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b:Movie) RETURN a")
	st := fakeStats{nodes: 1010, rels: 2000, label: map[uint32]float64{1: 10, 2: 1000}}
	eq(t, "cost", String(PlanWithStats(b, st)), `Project a
  Expand a -[@r0:#1]-> b:#2
    NodeScan a:#1
`)
}

func TestJoinOrderPutsSmallerSideOnBuild(t *testing.T) {
	// Two disjoint patterns join as a cartesian product, the builder putting the
	// first pattern on the left and the second on the right (the build side). With
	// Person rare and Movie abundant the left side is the smaller, so JoinOrder swaps
	// them to keep the smaller Person scan on the right build side.
	b := bound(t, "MATCH (a:Person), (b:Movie) RETURN a, b")
	st := fakeStats{nodes: 1010, label: map[uint32]float64{1: 10, 2: 1000}}
	eq(t, "join", String(PlanWithStats(b, st)), `Project a, b
  Join on[]
    NodeScan b:#2
    NodeScan a:#1
`)
}

func TestJoinOrderKeepsSmallerRight(t *testing.T) {
	// The same patterns, but now Movie is the rarer scan and already sits on the
	// right build side, so JoinOrder leaves the sides where the builder placed them.
	b := bound(t, "MATCH (a:Person), (b:Movie) RETURN a, b")
	st := fakeStats{nodes: 1010, label: map[uint32]float64{1: 1000, 2: 10}}
	eq(t, "join", String(PlanWithStats(b, st)), `Project a, b
  Join on[]
    NodeScan a:#1
    NodeScan b:#2
`)
}

func TestJoinOrderStructuralKeepsBuildOrder(t *testing.T) {
	// With no statistics the build side is the builder's pattern order, so the plan
	// matches the structural Plan and the goldens do not move.
	b := bound(t, "MATCH (a:Person), (b:Movie) RETURN a, b")
	eq(t, "join", String(PlanWithStats(b, nil)), String(Plan(b)))
}

func TestReanchorBailsVarLength(t *testing.T) {
	// A variable-length step is not safely reversible by the simple subset, so the
	// chain is left anchored as built even though b is the labeled end.
	b := bound(t, "MATCH (a)-[:KNOWS*1..2]->(b:Person) RETURN a")
	eq(t, "normalized", String(Plan(b)), `Project a
  Expand a -[@r0*:#1]-> b:#1
    NodeScan a
`)
}

func TestReanchorBailsCycle(t *testing.T) {
	// The pattern cycles back to a (an expand-into), which the simple subset does
	// not re-anchor; the built anchor stands.
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(a) RETURN a")
	eq(t, "normalized", String(Plan(b)), `Project a
  Expand b -[@r1:#1]-> a (into)
    Expand a -[@r0:#1]-> b
      NodeScan a:#1
`)
}

func TestProjectionTail(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p.name AS n ORDER BY p.age DESC SKIP 5 LIMIT 10")
	eq(t, "raw", String(Build(b)), `Limit 10
  Skip 5
    Sort p.age DESC
      Project p.name AS n
        NodeScan p:#1
`)
}

func TestBuildNamedPath(t *testing.T) {
	b := bound(t, "MATCH p = (a:Person)-[:KNOWS]->(b) RETURN p")
	want := `Project p
  BindPath p = [a,@r0,b]
    Expand a -[@r0:#1]-> b
      NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildCreateNode(t *testing.T) {
	b := bound(t, "CREATE (a:Person {name: $n})")
	want := `Create (a:#1 {#1: $n})
  Unit
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildCreateRel(t *testing.T) {
	// Both endpoints and the relationship are new; the relationship is oriented
	// From a To b regardless of how it was written.
	b := bound(t, "CREATE (a:Person)-[:KNOWS {age: 1}]->(b:Person)")
	want := `Create (a:#1), (b:#1), (a)-[@r0:#1 {#2: 1}]->(b)
  Unit
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildCreateAfterMatch(t *testing.T) {
	// a is already bound by the MATCH, so it is referenced, not created; only b and
	// the relationship are new. The Create sits above the read plan.
	b := bound(t, "MATCH (a:Person) CREATE (a)-[:KNOWS]->(b)")
	want := `Create (b), (a)-[@r0:#1]->(b)
  NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildCreateIncoming(t *testing.T) {
	// An incoming relationship is stored oriented from its real source: (a)<-[r]-(b)
	// creates the edge b -> a.
	b := bound(t, "CREATE (a)<-[:KNOWS]-(b)")
	want := `Create (a), (b), (b)-[@r0:#1]->(a)
  Unit
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildSetProperty(t *testing.T) {
	b := bound(t, "MATCH (a:Person) SET a.name = 'x'")
	want := `Set a.#1 = "x"
  NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildSetMultiple(t *testing.T) {
	b := bound(t, "MATCH (a) SET a.name = 'x', a:Person")
	want := `Set a.#1 = "x", a:#1
  NodeScan a
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildSetMapForms(t *testing.T) {
	b := bound(t, "MATCH (a) SET a += $m, a = $n")
	want := `Set a += $m, a = $n
  NodeScan a
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildRemove(t *testing.T) {
	b := bound(t, "MATCH (a:Person) REMOVE a.name, a:Movie")
	want := `Remove a.#1, a:#2
  NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildDelete(t *testing.T) {
	b := bound(t, "MATCH (a:Person) DELETE a")
	want := `Delete a
  NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildDetachDelete(t *testing.T) {
	b := bound(t, "MATCH (a:Person) DETACH DELETE a")
	want := `Delete DETACH a
  NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildMergeNode(t *testing.T) {
	// A leading MERGE runs over a Unit. The create branch is the node it ensures;
	// the match branch is a correlated scan filtered by the pattern's properties.
	b := bound(t, "MERGE (a:Person {name: 'x'})")
	want := `Merge (a:#1 {#1: "x"})
  Unit
  Filter a.name = "x"
    NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildMergeOnCreateOnMatch(t *testing.T) {
	b := bound(t, "MERGE (a:Person {name: 'x'}) ON CREATE SET a.age = 1 ON MATCH SET a:Movie")
	want := `Merge (a:#1 {#1: "x"}) on-create[a.#2 = 1] on-match[a:#2]
  Unit
  Filter a.name = "x"
    NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildMergeRelAfterMatch(t *testing.T) {
	// a is already bound, so the match branch roots on an Argument carrying it and
	// expands to find an existing edge; the create branch makes b and the edge.
	b := bound(t, "MATCH (a:Person) MERGE (a)-[r:KNOWS]->(b)")
	want := `Merge (b), (a)-[r:#1]->(b)
  NodeScan a:#1
  Expand a -[r:#1]-> b
    Argument [a]
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildMergeCorrelatedProperty(t *testing.T) {
	// The pattern's property references the outer variable e, so the match branch
	// roots on an Argument carrying e, cross-joins a scan, and filters above the
	// join where both e and the scanned node are in scope.
	b := bound(t, "UNWIND [1, 2] AS e MERGE (a:Person {age: e})")
	want := `Merge (a:#1 {#2: e})
  Unwind [1, 2] AS e
  Filter a.age = e
    Join on[]
      Argument [e]
      NodeScan a:#1
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildForeach(t *testing.T) {
	// A leading FOREACH runs over a Unit. The body is a correlated write sub-plan:
	// an Unwind of the list binds the loop variable, with the create stacked on top.
	// With no outer scope the Unwind needs no Argument leaf.
	b := bound(t, "FOREACH (x IN [1, 2, 3] | CREATE (n:Person {age: x}))")
	want := `Foreach
  Unit
  Create (n:#1 {#2: x})
    Unwind [1, 2, 3] AS x
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}

func TestBuildForeachAfterMatch(t *testing.T) {
	// a is already bound, so the body sub-plan roots on an Argument carrying it; the
	// Unwind binds the loop variable and the SET updates the outer node per element.
	b := bound(t, "MATCH (a:Person) FOREACH (x IN [1, 2] | SET a.age = x)")
	want := `Foreach
  NodeScan a:#1
  Set a.#2 = x
    Unwind [1, 2] AS x
      Argument [a]
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildForeachNested(t *testing.T) {
	// A nested FOREACH is the body of the outer one, a Cartesian product of the two
	// loops; the inner body sees the outer loop variable through its Argument.
	b := bound(t, "FOREACH (r IN [1] | FOREACH (c IN [2] | CREATE (n:Person {age: r})))")
	want := `Foreach
  Unit
  Foreach
    Unwind [1] AS r
    Create (n:#1 {#2: r})
      Unwind [2] AS c
        Argument [r]
`
	eq(t, "raw", String(Build(b)), want)
}

func TestBuildShortestPath(t *testing.T) {
	b := bound(t, "MATCH (a:Person), (b:Person) MATCH p = shortestPath((a)-[:KNOWS*]-(b)) RETURN p")
	want := `Project p
  ShortestPath p = shortest a -[@r0*:#1]- b
    Join on[]
      NodeScan a:#1
      NodeScan b:#1
`
	eq(t, "raw", String(Build(b)), want)
	eq(t, "normalized", String(Plan(b)), want)
}
