package bind

import (
	"strings"
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/parse"
)

// fakeCatalog is a fixed in-memory catalog seam: a name is known iff it is
// present in its dictionary, mapped to a one-based token like the real engine.
type fakeCatalog struct {
	labels   map[string]engine.Token
	relTypes map[string]engine.Token
	propKeys map[string]engine.Token
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

func bindStr(t *testing.T, src string, strict bool) (*Bound, error) {
	t.Helper()
	q, err := parse.Parse(src)
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	return Bind(q, newCatalog(), strict)
}

func mustBind(t *testing.T, src string) *Bound {
	t.Helper()
	b, err := bindStr(t, src, false)
	if err != nil {
		t.Fatalf("bind(%q): %v", src, err)
	}
	return b
}

func TestBindFriendsOfFriends(t *testing.T) {
	b := mustBind(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c) RETURN c.name AS name")
	if got := strings.Join(b.Columns, ","); got != "name" {
		t.Fatalf("columns = %q, want name", got)
	}
	// The leading label resolved to its catalog token.
	m := b.Query.First.Clauses[0].(*ast.Match)
	refs := b.NodeLabels(m.Patterns[0].Start)
	if len(refs) != 1 || !refs[0].Known || refs[0].Token != 1 {
		t.Fatalf("label Person should resolve to token 1, got %+v", refs)
	}
}

func TestResolutionLenient(t *testing.T) {
	b := mustBind(t, "MATCH (a:Ghost)-[:HAUNTS]->(b) WHERE a.spooky = true RETURN a")
	// Unknown label and type resolve to the schema-optional sentinel.
	m := b.Query.First.Clauses[0].(*ast.Match)
	if refs := b.NodeLabels(m.Patterns[0].Start); len(refs) != 1 || refs[0].Known {
		t.Fatalf("unknown label should resolve to an unknown ref, got %+v", refs)
	}
	// Unknown property key recorded as not-known (reads null at runtime).
	if pk := b.PropKey("spooky"); pk.Known {
		t.Fatalf("unknown property key should be unknown, got %+v", pk)
	}
}

func TestResolutionStrict(t *testing.T) {
	cases := []string{
		"MATCH (a:Ghost) RETURN a",
		"MATCH (a)-[:HAUNTS]->(b) RETURN a",
		"MATCH (a:Person) RETURN a.spooky",
	}
	for _, src := range cases {
		if _, err := bindStr(t, src, true); err == nil {
			t.Fatalf("strict bind(%q) should fail on the unknown name", src)
		}
	}
	// A fully-known query passes in strict mode.
	if _, err := bindStr(t, "MATCH (a:Person) WHERE a.age > 18 RETURN a.name", true); err != nil {
		t.Fatalf("strict bind of a known query: %v", err)
	}
}

func TestUnboundVariable(t *testing.T) {
	cases := []string{
		"MATCH (a:Person) RETURN b",
		"MATCH (a:Person) WHERE x.age > 1 RETURN a",
		"MATCH (a:Person) WITH a RETURN b",       // b dropped by WITH
		"RETURN x",                               // never bound
		"UNWIND xs AS x RETURN x",                // xs unbound
		"MATCH (a:Person) RETURN a ORDER BY z.x", // z not in scope
	}
	for _, src := range cases {
		if _, err := bindStr(t, src, false); err == nil {
			t.Fatalf("bind(%q) should report an unbound variable", src)
		}
	}
}

func TestScopeCarriesKind(t *testing.T) {
	// A node carried through WITH by name stays usable as a node in a later
	// pattern (its role survives the projection).
	mustBind(t, "MATCH (a:Person) WITH a MATCH (a)-[:KNOWS]->(b) RETURN b")
	mustBind(t, "MATCH (a:Person) WITH a AS p MATCH (p)-[:KNOWS]->(b) RETURN b")
}

func TestRoleConflict(t *testing.T) {
	// Re-using a node name as a relationship in one MATCH is a role conflict.
	if _, err := bindStr(t, "MATCH (a:Person)-[a]->(b) RETURN b", false); err == nil {
		t.Fatal("reusing a node name as a relationship should fail")
	}
}

func TestAggregatePlacement(t *testing.T) {
	mustBind(t, "MATCH (a:Person) RETURN a.name, count(*)")
	mustBind(t, "MATCH (a:Person) RETURN count(DISTINCT a.name) AS n")
	// An aggregate in WHERE is illegal.
	if _, err := bindStr(t, "MATCH (a:Person) WHERE count(*) > 1 RETURN a", false); err == nil {
		t.Fatal("aggregate in WHERE should fail")
	}
	// A nested aggregate is illegal.
	if _, err := bindStr(t, "MATCH (a:Person) RETURN sum(count(a)) AS n", false); err == nil {
		t.Fatal("nested aggregate should fail")
	}
}

func TestWithAliasRequired(t *testing.T) {
	// A WITH item that is not a bare variable needs an alias.
	if _, err := bindStr(t, "MATCH (a:Person) WITH a.age RETURN a", false); err == nil {
		t.Fatal("unaliased non-variable WITH item should fail")
	}
	mustBind(t, "MATCH (a:Person) WITH a.age AS age RETURN age")
}

func TestUnionColumns(t *testing.T) {
	mustBind(t, "MATCH (a:Person) RETURN a.name AS name UNION MATCH (m:Movie) RETURN m.title AS name")
	if _, err := bindStr(t,
		"MATCH (a:Person) RETURN a.name AS name UNION MATCH (m:Movie) RETURN m.title AS title", false); err == nil {
		t.Fatal("mismatched UNION column names should fail")
	}
	if _, err := bindStr(t,
		"MATCH (a:Person) RETURN a.name AS x, a.age AS y UNION MATCH (m:Movie) RETURN m.title AS x", false); err == nil {
		t.Fatal("mismatched UNION column count should fail")
	}
}

func TestVarLengthBounds(t *testing.T) {
	mustBind(t, "MATCH (a:Person)-[:KNOWS*1..3]->(b) RETURN b")
	mustBind(t, "MATCH (a:Person)-[:KNOWS*2]->(b) RETURN b")
	if _, err := bindStr(t, "MATCH (a:Person)-[:KNOWS*3..1]->(b) RETURN b", false); err == nil {
		t.Fatal("inverted variable-length bounds should fail")
	}
}

func TestColumnNames(t *testing.T) {
	b := mustBind(t, "MATCH (a:Person) RETURN a.name, a.age AS years, a")
	got := strings.Join(b.Columns, "|")
	if got != "a.name|years|a" {
		t.Fatalf("columns = %q, want a.name|years|a", got)
	}
}

func TestStarColumns(t *testing.T) {
	b := mustBind(t, "MATCH (a:Person)-[r:KNOWS]->(b) RETURN *")
	got := strings.Join(b.Columns, ",")
	if got != "a,b,r" {
		t.Fatalf("star columns = %q, want a,b,r", got)
	}
}

func TestBindNamedPath(t *testing.T) {
	// A named path over fixed-length steps binds the path variable.
	mustBind(t, "MATCH p = (a:Person)-[:KNOWS]->(b) RETURN p")
	// A named path over a variable-length step is rejected for now.
	if _, err := bindStr(t, "MATCH p = (a:Person)-[:KNOWS*]->(b) RETURN p", false); err == nil {
		t.Fatal("named var-length path should be rejected")
	}
}

func TestBindShortestPath(t *testing.T) {
	// A named shortest path over a variable-length step binds, unlike an ordinary
	// named variable-length path: the shortest-path operator records the full walk.
	mustBind(t, "MATCH (a:Person), (b:Person) MATCH p = shortestPath((a)-[:KNOWS*]-(b)) RETURN p")
	mustBind(t, "MATCH (a:Person), (b:Person) MATCH allShortestPaths((a)-[:KNOWS*]-(b)) RETURN a")
	// A shortest-path pattern must carry exactly one relationship.
	if _, err := bindStr(t, "MATCH (a), (b), (c) MATCH p = shortestPath((a)-[:KNOWS*]-(b)-[:KNOWS*]-(c)) RETURN p", false); err == nil {
		t.Fatal("multi-relationship shortestPath should be rejected")
	}
}

func TestBindCreate(t *testing.T) {
	// CREATE is a write clause: a query may end with it, with no RETURN.
	mustBind(t, "CREATE (a:Person {name: $n})")
	mustBind(t, "CREATE (a:Person)-[:KNOWS]->(b:Person)")
	// CREATE after a MATCH references the matched node and creates the rest.
	mustBind(t, "MATCH (a:Person) CREATE (a)-[:KNOWS]->(b)")
	// A created relationship must have a direction and exactly one type.
	if _, err := bindStr(t, "CREATE (a)-[:KNOWS]-(b)", false); err == nil {
		t.Fatal("undirected relationship in CREATE should be rejected")
	}
	if _, err := bindStr(t, "CREATE (a)-[r]->(b)", false); err == nil {
		t.Fatal("relationship without a type in CREATE should be rejected")
	}
	if _, err := bindStr(t, "CREATE (a)-[:KNOWS|LIKES]->(b)", false); err == nil {
		t.Fatal("relationship with two types in CREATE should be rejected")
	}
	// A variable-length relationship cannot be created.
	if _, err := bindStr(t, "CREATE (a)-[:KNOWS*]->(b)", false); err == nil {
		t.Fatal("variable-length relationship in CREATE should be rejected")
	}
	// shortestPath is a read construct, not creatable.
	if _, err := bindStr(t, "CREATE p = shortestPath((a)-[:KNOWS*]-(b))", false); err == nil {
		t.Fatal("shortestPath in CREATE should be rejected")
	}
}

func TestBindSetRemove(t *testing.T) {
	// SET and REMOVE bind their target against an already-bound variable.
	mustBind(t, "MATCH (a) SET a.name = 'x'")
	mustBind(t, "MATCH (a) SET a.name = 'x', a.age = 1")
	mustBind(t, "MATCH (a) SET a:Person")
	mustBind(t, "MATCH (a)-[r:KNOWS]->(b) SET r.since = 2020")
	mustBind(t, "MATCH (a) REMOVE a.name")
	mustBind(t, "MATCH (a) REMOVE a:Person")
	// The targeted variable must be in scope.
	if _, err := bindStr(t, "SET a.name = 'x'", false); err == nil {
		t.Fatal("SET of an unbound variable should be rejected")
	}
	if _, err := bindStr(t, "MATCH (a) REMOVE b.name", false); err == nil {
		t.Fatal("REMOVE of an unbound variable should be rejected")
	}
	// A label set or removal targets a node, not a relationship.
	if _, err := bindStr(t, "MATCH (a)-[r:KNOWS]->(b) SET r:Person", false); err == nil {
		t.Fatal("SET label on a relationship should be rejected")
	}
	if _, err := bindStr(t, "MATCH (a)-[r:KNOWS]->(b) REMOVE r:Person", false); err == nil {
		t.Fatal("REMOVE label on a relationship should be rejected")
	}
	// The map forms of SET bind: a map merge, a map replace, and an
	// element-to-element copy onto a node or relationship.
	mustBind(t, "MATCH (a) SET a += {x: 1}")
	mustBind(t, "MATCH (a) SET a = {x: 1}")
	mustBind(t, "MATCH (a)-[r:KNOWS]->(b) SET r += $m")
	mustBind(t, "MATCH (a), (b) SET a = b")
	// A map-form target must be a node or relationship, not a value.
	if _, err := bindStr(t, "MATCH (a) WITH a.name AS n SET n += {x: 1}", false); err == nil {
		t.Fatal("map-form SET onto a value should be rejected")
	}
}

func TestBindDelete(t *testing.T) {
	// DELETE and DETACH DELETE bind their targets against bound variables.
	mustBind(t, "MATCH (a) DELETE a")
	mustBind(t, "MATCH (a) DETACH DELETE a")
	mustBind(t, "MATCH (a)-[r:KNOWS]->(b) DELETE r")
	mustBind(t, "MATCH (a)-[r:KNOWS]->(b) DELETE r, a, b")
	// The targeted variable must be in scope.
	if _, err := bindStr(t, "DELETE a", false); err == nil {
		t.Fatal("DELETE of an unbound variable should be rejected")
	}
	if _, err := bindStr(t, "MATCH (a) DELETE b", false); err == nil {
		t.Fatal("DELETE of an unbound variable should be rejected")
	}
	// A bare-variable target must be a node or relationship.
	if _, err := bindStr(t, "MATCH (a) WITH a.name AS n DELETE n", false); err == nil {
		t.Fatal("DELETE of a value should be rejected")
	}
}
