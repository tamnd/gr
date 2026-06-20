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

func (c *fakeCatalog) LabelToken(n string) (engine.Token, bool)   { t, ok := c.labels[n]; return t, ok }
func (c *fakeCatalog) RelTypeToken(n string) (engine.Token, bool) { t, ok := c.relTypes[n]; return t, ok }
func (c *fakeCatalog) PropKeyToken(n string) (engine.Token, bool) { t, ok := c.propKeys[n]; return t, ok }

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

func TestProjectionTail(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p.name AS n ORDER BY p.age DESC SKIP 5 LIMIT 10")
	eq(t, "raw", String(Build(b)), `Limit 10
  Skip 5
    Sort p.age DESC
      Project p.name AS n
        NodeScan p:#1
`)
}
