package plan

import (
	"math"
	"testing"
)

func TestSnapshotRecordsReferencedCounts(t *testing.T) {
	b := bound(t, "MATCH (p:Person)-[:KNOWS]->(f) RETURN f")
	st := fakeStats{nodes: 1000, rels: 500, label: map[uint32]float64{1: 200}, relType: map[uint32]float64{1: 300}}
	snap := Snapshot(Plan(b), st)
	if snap.nodes != 1000 || snap.rels != 500 {
		t.Fatalf("snapshot totals = (%v, %v), want (1000, 500)", snap.nodes, snap.rels)
	}
	if snap.labels[1] != 200 {
		t.Fatalf("snapshot Person count = %v, want 200", snap.labels[1])
	}
	if snap.relTypes[1] != 300 {
		t.Fatalf("snapshot KNOWS count = %v, want 300", snap.relTypes[1])
	}
}

func TestSnapshotNilStatsIsZero(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p")
	snap := Snapshot(Plan(b), nil)
	if snap.nodes != 0 || len(snap.labels) != 0 {
		t.Fatalf("nil-stats snapshot is not zero: %+v", snap)
	}
	// A zero snapshot never drifts, whatever the live counts are.
	if Drifted(snap, fakeStats{nodes: 1000, label: map[uint32]float64{1: 999}}, DefaultDriftFactor) {
		t.Fatal("zero snapshot reported drift")
	}
}

func TestDriftedDetectsRelativeChange(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p")
	// Person is a tenth of the graph when the plan is costed.
	snap := Snapshot(Plan(b), fakeStats{nodes: 1000, label: map[uint32]float64{1: 100}})
	// It is now nine tenths: its share grew by nine times, well past the factor.
	now := fakeStats{nodes: 1000, label: map[uint32]float64{1: 900}}
	if !Drifted(snap, now, DefaultDriftFactor) {
		t.Fatal("expected drift when a label's share grows ninefold")
	}
}

func TestDriftedIgnoresUniformGrowth(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p")
	snap := Snapshot(Plan(b), fakeStats{nodes: 1000, label: map[uint32]float64{1: 100}})
	// The graph grew tenfold but Person is still a tenth of it, so no decision drifts.
	now := fakeStats{nodes: 10000, label: map[uint32]float64{1: 1000}}
	if Drifted(snap, now, DefaultDriftFactor) {
		t.Fatal("uniform growth should not count as drift")
	}
}

func TestDriftedLabelVanished(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p")
	snap := Snapshot(Plan(b), fakeStats{nodes: 1000, label: map[uint32]float64{1: 100}})
	// Every Person is gone: an unbounded change, so drift regardless of the factor.
	now := fakeStats{nodes: 1000, label: map[uint32]float64{1: 0}}
	if !Drifted(snap, now, math.MaxFloat64) {
		t.Fatal("a vanished label should always drift")
	}
}

func TestDriftedDisabled(t *testing.T) {
	b := bound(t, "MATCH (p:Person) RETURN p")
	snap := Snapshot(Plan(b), fakeStats{nodes: 1000, label: map[uint32]float64{1: 100}})
	now := fakeStats{nodes: 1000, label: map[uint32]float64{1: 900}}
	if Drifted(snap, now, 1) {
		t.Fatal("a factor of one disables re-planning")
	}
	if Drifted(snap, nil, DefaultDriftFactor) {
		t.Fatal("nil statistics disables re-planning")
	}
}
