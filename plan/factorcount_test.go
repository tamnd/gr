package plan

import "testing"

// TestFusePolygonAnchorFourCycle confirms the directed four-cycle count plans to an
// IntersectCount over a two-hop anchor path and that FusePolygonAnchor recognizes it,
// returning the scan and both hops so the fused operator drives them inline. The triangle
// is the one-hop control.
func TestFusePolygonAnchorFourCycle(t *testing.T) {
	st := fakeStats{nodes: 1000, rels: 5000, label: map[uint32]float64{1: 10}, relType: map[uint32]float64{1: 5000}}

	fb := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(d)-[:KNOWS]->(a) RETURN count(*) AS n")
	four := FactorizeCount(PlanWithStats(fb, st))
	ic, ok := four.(*IntersectCount)
	if !ok {
		t.Fatalf("four-cycle did not factorize to IntersectCount, got %T:\n%s", four, String(four))
	}
	ns, hops, anchor := FusePolygonAnchor(ic)
	if ns == nil {
		t.Fatalf("FusePolygonAnchor did not match the four-cycle:\n%s", String(ic))
	}
	if len(hops) != 2 {
		t.Fatalf("four-cycle anchor wants 2 hops, got %d", len(hops))
	}
	if anchor != nil {
		t.Fatalf("directed four-cycle has no anchor predicate, got one")
	}
	if hops[0].From != ns.Var {
		t.Fatalf("first hop should leave the scan root %q, leaves %q", ns.Var, hops[0].From)
	}
	if hops[1].From != hops[0].To {
		t.Fatalf("second hop should leave the first hop's target %q, leaves %q", hops[0].To, hops[1].From)
	}

	tb := bound(t, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN count(*) AS n")
	tic, ok := FactorizeCount(PlanWithStats(tb, st)).(*IntersectCount)
	if !ok {
		t.Fatal("triangle did not factorize to IntersectCount")
	}
	if _, thops, _ := FusePolygonAnchor(tic); len(thops) != 1 {
		t.Fatalf("triangle anchor wants 1 hop, got %d", len(thops))
	}
}
