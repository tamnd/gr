package plan

import (
	"testing"

	"github.com/tamnd/gr/ast"
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
		labels:   map[string]engine.Token{"Person": 1, "Movie": 2, "Genre": 3},
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

// TestBuildExpandCarriesSourceLabels confirms the builder threads each expand's
// source-node labels onto the Expand, the start node's for the first hop and the
// previous reached node's thereafter, so the cost model can condition the fan-out
// on the source population. The labels are cost-model metadata and do not show in
// the plan's String form, so this checks the field directly.
func TestBuildExpandCarriesSourceLabels(t *testing.T) {
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c) RETURN c")
	var expands []*Expand
	var walk func(o Op)
	walk = func(o Op) {
		if e, ok := o.(*Expand); ok {
			expands = append(expands, e)
		}
		for _, c := range nodeChildren(o) {
			walk(c)
		}
	}
	walk(Build(b))
	if len(expands) != 2 {
		t.Fatalf("found %d expands, want 2", len(expands))
	}
	// The walk visits the outer (b->c) expand before its (a->b) child.
	bc, ab := expands[0], expands[1]
	if ab.From != "a" || bc.From != "b" {
		t.Fatalf("expands in unexpected order: %q then %q", bc.From, ab.From)
	}
	// a is :Person, so the first hop's source labels name Person; b is unlabeled, so
	// the second hop carries none.
	if len(ab.FromLabels) != 1 || !ab.FromLabels[0].Known {
		t.Fatalf("a->b FromLabels = %+v, want one known label", ab.FromLabels)
	}
	if len(bc.FromLabels) != 0 {
		t.Fatalf("b->c FromLabels = %+v, want none (b is unlabeled)", bc.FromLabels)
	}
}

// TestPlanKeepsSourceLabels guards that the source-label cost metadata survives the
// whole planning pipeline, not just Build. The optimizer and join-order passes
// rebuild operators through mapChildren, and an Expand rebuild that listed its
// fields by hand used to drop FromLabels, silently reverting the source-label
// degree conditioning to the all-node average before the cost model ever read it.
// Person is the only labeled node here, so the anchor stays put and the natural
// chain reaches join ordering and drift with its source label intact.
func TestPlanKeepsSourceLabels(t *testing.T) {
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b) RETURN b")
	st := fakeStats{nodes: 1000, rels: 300, label: map[uint32]float64{1: 200}, relType: map[uint32]float64{1: 300}}
	var exp *Expand
	var walk func(o Op)
	walk = func(o Op) {
		if e, ok := o.(*Expand); ok {
			exp = e
		}
		for _, c := range nodeChildren(o) {
			walk(c)
		}
	}
	walk(PlanWithStats(b, st))
	if exp == nil || exp.From != "a" {
		t.Fatalf("expand = %+v, want one anchored at a", exp)
	}
	if len(exp.FromLabels) != 1 || !exp.FromLabels[0].Known || exp.FromLabels[0].Token != 1 {
		t.Fatalf("planned expand FromLabels = %+v, want [Person(#1)] to survive the pipeline", exp.FromLabels)
	}
}

// findExpandCount returns the single ExpandCount in a plan, or nil when there is
// none, so a rewrite test can assert the factorized operator is present (or absent)
// without depending on the printed plan's column naming.
func findExpandCount(o Op) *ExpandCount {
	if ec, ok := o.(*ExpandCount); ok {
		return ec
	}
	for _, c := range nodeChildren(o) {
		if ec := findExpandCount(c); ec != nil {
			return ec
		}
	}
	return nil
}

// TestFactorizeCountFires checks a grouping-free count(*) directly over a plain
// expand is rewritten to an ExpandCount that counts the expand's edges without
// materializing a row per edge. The rewritten operator keeps the source variable,
// relationship variable, type, and direction the expand carried, the inputs its
// executor needs to count exactly the edges the expand would have emitted.
func TestFactorizeCountFires(t *testing.T) {
	b := bound(t, "MATCH (a:Person)-[r:KNOWS]->(b) RETURN count(*)")
	ec := findExpandCount(Plan(b))
	if ec == nil {
		t.Fatalf("count(*) over an expand did not factorize:\n%s", String(Plan(b)))
	}
	if ec.From != "a" || ec.Rel != "r" || ec.Dir != ast.DirOut {
		t.Fatalf("ExpandCount = %+v, want From=a Rel=r Dir=out", ec)
	}
	if len(ec.Types) != 1 || ec.Types[0].Token != 1 {
		t.Fatalf("ExpandCount types = %+v, want [KNOWS(#1)]", ec.Types)
	}
	// The factorized plan must not also keep the Expand it replaced.
	if e := findFirstExpand(Plan(b)); e != nil {
		t.Fatalf("plan still has an Expand after factorizing: %+v", e)
	}
}

func findFirstExpand(o Op) *Expand {
	if e, ok := o.(*Expand); ok {
		return e
	}
	for _, c := range nodeChildren(o) {
		if e := findFirstExpand(c); e != nil {
			return e
		}
	}
	return nil
}

// TestFactorizeCountGuards pins the shapes the rewrite must leave alone: a grouped
// count keeps its grouping rows, count(r) skips null edges so it is not the row
// count, a variable-length expand fans out per trail, an expand-into and a
// target-label constraint each drop edges the bare count would keep, and a count
// over a scan has no expand to factor. Each must keep its Aggregate and grow no
// ExpandCount.
func TestFactorizeCountGuards(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"grouped", "MATCH (a:Person)-[r:KNOWS]->(b) RETURN a, count(*)"},
		{"count-var", "MATCH (a:Person)-[r:KNOWS]->(b) RETURN count(r)"},
		{"distinct", "MATCH (a:Person)-[r:KNOWS]->(b) RETURN count(DISTINCT b)"},
		{"varlen", "MATCH (a:Person)-[r:KNOWS*1..2]->(b) RETURN count(*)"},
		{"to-labels", "MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN count(*)"},
		{"scan-only", "MATCH (a:Person) RETURN count(*)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := bound(t, c.src)
			if ec := findExpandCount(Plan(b)); ec != nil {
				t.Fatalf("%s factorized but should not have:\n%s", c.name, String(Plan(b)))
			}
		})
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

func TestJoinOrderReordersCartesianChain(t *testing.T) {
	// Three disjoint patterns join as a left-deep cartesian chain in pattern order.
	// With Genre rarest, Movie middling, and Person abundant, JoinOrder reorders the
	// chain smallest first: the rare Genre is the innermost build side, its small
	// product with Movie is the next build side, and the abundant Person probes from
	// the outside.
	b := bound(t, "MATCH (a:Person), (b:Movie), (c:Genre) RETURN a, b, c")
	st := fakeStats{nodes: 1105, label: map[uint32]float64{1: 1000, 2: 100, 3: 5}}
	eq(t, "join", String(PlanWithStats(b, st)), `Project a, b, c
  Join on[]
    NodeScan a:#1
    Join on[]
      NodeScan b:#2
      NodeScan c:#3
`)
}

func TestJoinOrderReorderStructuralUnchanged(t *testing.T) {
	// With no statistics the cartesian chain keeps the builder's pattern order, so the
	// plan matches the structural Plan and the goldens do not move.
	b := bound(t, "MATCH (a:Person), (b:Movie), (c:Genre) RETURN a, b, c")
	eq(t, "join", String(PlanWithStats(b, nil)), String(Plan(b)))
}

func TestJoinOrderStructuralKeepsBuildOrder(t *testing.T) {
	// With no statistics the build side is the builder's pattern order, so the plan
	// matches the structural Plan and the goldens do not move.
	b := bound(t, "MATCH (a:Person), (b:Movie) RETURN a, b")
	eq(t, "join", String(PlanWithStats(b, nil)), String(Plan(b)))
}

func TestWcojRewritesTriangle(t *testing.T) {
	// A triangle's closing edge is an expand-into over the expand that produces the
	// apex. With a non-trivial average degree the binary plan's intermediate (every
	// neighbor of b as a candidate c) dwarfs the closing matches, so the cost model
	// replaces the two edges with an Intersect that computes c as the neighbors of b
	// that also reach a. The subtree binding a and b is left as the anchored a->b chain.
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN a")
	st := fakeStats{nodes: 1000, rels: 5000, label: map[uint32]float64{1: 10}, relType: map[uint32]float64{1: 5000}}
	eq(t, "wcoj", String(PlanWithStats(b, st)), `Project a
  Intersect c <= b -[@r1:#1]-> & a <-[@r2:#1]-
    Expand a -[@r0:#1]-> b
      NodeScan a:#1
`)
}

func TestWcojStructuralUnchanged(t *testing.T) {
	// With no statistics the triangle keeps the builder's binary expand-into plan, so
	// the structural Plan is unchanged and the planner goldens hold.
	b := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN a")
	eq(t, "wcoj", String(PlanWithStats(b, nil)), String(Plan(b)))
	eq(t, "wcoj", String(Plan(b)), `Project a
  Expand c -[@r2:#1]-> a (into)
    Expand b -[@r1:#1]-> c
      Expand a -[@r0:#1]-> b
        NodeScan a:#1
`)
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
