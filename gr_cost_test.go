package gr

import (
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/plan"
)

// TestEngineStatsCounts confirms the engineStats adapter reports the real catalog
// counts a write left behind: the totals and the per-label count, mapped to the
// float64 the cost model works in. It is the bridge the planner will cost plans
// through, so it must read the same numbers the engine maintains.
func TestEngineStatsCounts(t *testing.T) {
	db := openMem(t, "enginestats.gr")
	defer func() { _ = db.Close() }()

	// Three Person nodes, two of them linked by a KNOWS relationship.
	mustExec(t, db, "CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil)
	mustExec(t, db, "CREATE (:Person {name: 'C'})", nil)

	st := engineStats{eng: db.eng}

	if got := st.NodeCount(); got != 3 {
		t.Fatalf("NodeCount = %v, want 3", got)
	}
	if got := st.RelCount(); got != 1 {
		t.Fatalf("RelCount = %v, want 1", got)
	}

	person, ok := db.eng.Lookup(catalog.KindLabel, "Person")
	if !ok {
		t.Fatal("Person label was not interned")
	}
	if got := st.LabelCount(uint32(person)); got != 3 {
		t.Fatalf("LabelCount(Person) = %v, want 3", got)
	}

	knows, ok := db.eng.Lookup(catalog.KindRelType, "KNOWS")
	if !ok {
		t.Fatal("KNOWS type was not interned")
	}
	if got := st.RelTypeCount(uint32(knows)); got != 1 {
		t.Fatalf("RelTypeCount(KNOWS) = %v, want 1", got)
	}
}

// TestEngineStatsDrivesEstimate confirms the adapter and the estimator compose: an
// all-nodes scan estimate equals the live node count read through the adapter.
func TestEngineStatsDrivesEstimate(t *testing.T) {
	db := openMem(t, "enginestatsest.gr")
	defer func() { _ = db.Close() }()

	for range 5 {
		mustExec(t, db, "CREATE (:Person)", nil)
	}
	st := engineStats{eng: db.eng}
	if got := plan.EstimateRows(&plan.NodeScan{Var: "n"}, st); got != 5 {
		t.Fatalf("estimate of an all-nodes scan = %v, want 5", got)
	}
}
