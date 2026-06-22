package plan

import (
	"math"

	"github.com/tamnd/gr/bind"
)

// DefaultDriftFactor is the relative-fraction change at which a cached plan is
// re-planned (doc 11 §7). A referenced label or type whose share of the graph has
// grown or shrunk by more than this multiple since the plan was costed is treated as
// drift, so the default of four lets the data shift a fair amount before paying a
// re-plan, while still catching a label that has changed enough to flip a cost
// decision.
const DefaultDriftFactor = 4.0

// StatsSnapshot records the cardinalities a plan's cost decisions were made against
// (doc 11 §7, §9.3). It holds the graph totals and the per-token counts of every
// label and relationship type the plan references, so a later execution can tell
// whether the data has drifted far enough that a different plan might now be cheaper.
// The zero snapshot is the basis a structurally planned query carries, which never
// drifts.
type StatsSnapshot struct {
	nodes    float64
	rels     float64
	labels   map[uint32]float64
	relTypes map[uint32]float64
}

// Snapshot records the statistics a plan's cost decisions depend on: the total node
// and relationship counts, and the per-token count of every label and relationship
// type the plan references. A nil Statistics yields the zero snapshot, the basis a
// query planned without a cost model carries.
func Snapshot(o Op, st Statistics) StatsSnapshot {
	snap := StatsSnapshot{labels: map[uint32]float64{}, relTypes: map[uint32]float64{}}
	if st == nil {
		return snap
	}
	snap.nodes = st.NodeCount()
	snap.rels = st.RelCount()
	collectStats(o, st, &snap)
	return snap
}

// collectStats walks the plan recording the live count of every label and type its
// scans, seeks, and expands reference, the inputs the cost model ordered the plan on.
func collectStats(o Op, st Statistics, snap *StatsSnapshot) {
	switch x := o.(type) {
	case *NodeScan:
		recordLabels(x.Labels, st, snap)
	case *NodeIndexSeek:
		recordLabel(x.Label, st, snap)
		recordLabels(x.Rest, st, snap)
	case *Expand:
		recordTypes(x.Types, st, snap)
		recordLabels(x.FromLabels, st, snap)
		recordLabels(x.ToLabels, st, snap)
	case *Intersect:
		recordTypes(x.Legs[0].Types, st, snap)
		recordTypes(x.Legs[1].Types, st, snap)
		recordLabels(x.Labels, st, snap)
	case *ShortestPath:
		recordTypes(x.Types, st, snap)
	}
	for _, c := range nodeChildren(o) {
		collectStats(c, st, snap)
	}
}

func recordLabels(refs []bind.NameRef, st Statistics, snap *StatsSnapshot) {
	for _, r := range refs {
		recordLabel(r, st, snap)
	}
}

func recordLabel(ref bind.NameRef, st Statistics, snap *StatsSnapshot) {
	if ref.Known {
		snap.labels[uint32(ref.Token)] = st.LabelCount(uint32(ref.Token))
	}
}

func recordTypes(refs []bind.NameRef, st Statistics, snap *StatsSnapshot) {
	for _, r := range refs {
		if r.Known {
			snap.relTypes[uint32(r.Token)] = st.RelTypeCount(uint32(r.Token))
		}
	}
}

// Drifted reports whether the live statistics have moved far enough from a plan's
// snapshot that its cost decisions may no longer hold (doc 11 §7). It compares each
// recorded count as a fraction of its graph total, so uniform growth, where every
// count scales together, is not drift: only a change in the relative sizes the
// planner ordered the plan on counts. A factor of one or less, or nil statistics,
// disables re-planning. Re-planning on a false positive is never wrong, only a wasted
// compile, so the test errs toward the cheaper side of leaving a plan in place.
func Drifted(snap StatsSnapshot, st Statistics, factor float64) bool {
	if st == nil || factor <= 1 {
		return false
	}
	if driftedSet(snap.labels, snap.nodes, st.NodeCount(), st.LabelCount, factor) {
		return true
	}
	return driftedSet(snap.relTypes, snap.rels, st.RelCount(), st.RelTypeCount, factor)
}

func driftedSet(recorded map[uint32]float64, oldTotal, newTotal float64, count func(uint32) float64, factor float64) bool {
	for tok, oldCount := range recorded {
		if fracDrift(oldCount, oldTotal, count(tok), newTotal) > factor {
			return true
		}
	}
	return false
}

// fracDrift is the ratio, at least one, between a token's old and new share of the
// graph. A share that fell to zero (the token's nodes are all gone) or rose from zero
// is unbounded drift; two zero shares are no drift.
func fracDrift(oldCount, oldTotal, newCount, newTotal float64) float64 {
	o := frac(oldCount, oldTotal)
	n := frac(newCount, newTotal)
	switch {
	case o == 0 && n == 0:
		return 1
	case o == 0 || n == 0:
		return math.Inf(1)
	case o > n:
		return o / n
	default:
		return n / o
	}
}

func frac(count, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return count / total
}
