package plan

import (
	"math"
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// fakeStats is a fixed set of catalog counts, the cost model's Statistics seam. The
// maps are keyed by catalog token so a test states exactly the counts an estimate is
// computed from.
type fakeStats struct {
	nodes   float64
	rels    float64
	label   map[uint32]float64
	relType map[uint32]float64
}

func (f fakeStats) NodeCount() float64            { return f.nodes }
func (f fakeStats) RelCount() float64             { return f.rels }
func (f fakeStats) LabelCount(l uint32) float64   { return f.label[l] }
func (f fakeStats) RelTypeCount(t uint32) float64 { return f.relType[t] }

// known builds a resolved name reference on a catalog token.
func known(tok uint32) bind.NameRef {
	return bind.NameRef{Token: engine.Token(tok), Known: true}
}

// unknown is a name the catalog never interned: it matches nothing.
var unknown = bind.NameRef{Known: false}

const eps = 1e-9

func wantRows(t *testing.T, o Op, st Statistics, want float64) {
	t.Helper()
	got := EstimateRows(o, st)
	if math.Abs(got-want) > eps {
		t.Fatalf("EstimateRows = %v, want %v", got, want)
	}
}

// lit builds an integer literal expression, for Skip/Limit counts.
func lit(n int64) ast.Expr { return &ast.Literal{Value: value.Int(n)} }

// TestEstimateScanAllNodes confirms an unlabeled scan is the whole node count.
func TestEstimateScanAllNodes(t *testing.T) {
	st := fakeStats{nodes: 1000}
	wantRows(t, &NodeScan{Var: "n"}, st, 1000)
}

// TestEstimateScanLabeled confirms a labeled scan is the label's count, and a scan
// requiring two labels is the smaller of the two, since a node carrying both is at
// most as common as its rarer label.
func TestEstimateScanLabeled(t *testing.T) {
	st := fakeStats{nodes: 1000, label: map[uint32]float64{1: 200, 2: 50}}
	wantRows(t, &NodeScan{Var: "n", Labels: []bind.NameRef{known(1)}}, st, 200)
	wantRows(t, &NodeScan{Var: "n", Labels: []bind.NameRef{known(1), known(2)}}, st, 50)
}

// TestEstimateScanUnknownLabel confirms a scan on a label the catalog never interned
// estimates zero rows, matching the schema-optional reading rule.
func TestEstimateScanUnknownLabel(t *testing.T) {
	st := fakeStats{nodes: 1000, label: map[uint32]float64{1: 200}}
	wantRows(t, &NodeScan{Var: "n", Labels: []bind.NameRef{unknown}}, st, 0)
}

// TestEstimateIndexSeek confirms a seek is the indexed label's count cut by the
// equality selectivity, far fewer than the label scan it replaces.
func TestEstimateIndexSeek(t *testing.T) {
	st := fakeStats{nodes: 1000, label: map[uint32]float64{1: 200}}
	seek := &NodeIndexSeek{Var: "n", Label: known(1), Prop: known(9), Value: lit(1)}
	wantRows(t, seek, st, 200*DefaultEqualitySelectivity)
}

// TestEstimateExpandTyped confirms an expand scales its input by the average degree
// of the relationship types: input times summed type counts over node count.
func TestEstimateExpandTyped(t *testing.T) {
	st := fakeStats{nodes: 100, rels: 500, relType: map[uint32]float64{3: 300}}
	// 10 source rows, each expanding KNOWS (300 rels over 100 nodes = degree 3).
	exp := &Expand{
		Input: &NodeScan{Var: "a"},
		From:  "a", To: "b", Rel: "r",
		Types: []bind.NameRef{known(3)},
	}
	// NodeScan over no labels = 100 nodes; degree 3; expect 300.
	wantRows(t, exp, st, 300)
}

// TestEstimateExpandTypeless confirms a typeless expand uses the whole relationship
// count over the node count as the degree.
func TestEstimateExpandTypeless(t *testing.T) {
	st := fakeStats{nodes: 100, rels: 250}
	exp := &Expand{Input: &NodeScan{Var: "a"}, From: "a", To: "b", Rel: "r"}
	// 100 nodes, degree 2.5, expect 250.
	wantRows(t, exp, st, 250)
}

// TestEstimateExpandInto confirms an expand-into (target already bound) is far more
// selective than the full fan-out.
func TestEstimateExpandInto(t *testing.T) {
	st := fakeStats{nodes: 100, rels: 250}
	exp := &Expand{Input: &NodeScan{Var: "a"}, From: "a", To: "b", Rel: "r", ToBound: true}
	wantRows(t, exp, st, 250*DefaultEqualitySelectivity)
}

// TestEstimateExpandConditionsOnSourceLabel confirms an expand from a labeled
// source divides the type's edges by the source label's count, not the whole node
// count, so a scan of a rare label expanding a type those nodes own is not
// under-estimated. Person is a fifth of the graph but owns all the KNOWS edges:
// 200 Persons * (300 KNOWS / 200 Persons) = 300, the true edge total, where the
// all-node average would give 200 * 300/1000 = 60.
func TestEstimateExpandConditionsOnSourceLabel(t *testing.T) {
	st := fakeStats{nodes: 1000, rels: 300, label: map[uint32]float64{1: 200}, relType: map[uint32]float64{3: 300}}
	exp := &Expand{
		Input: &NodeScan{Var: "a", Labels: []bind.NameRef{known(1)}},
		From:  "a", To: "b", Rel: "r",
		Types:      []bind.NameRef{known(3)},
		FromLabels: []bind.NameRef{known(1)},
	}
	wantRows(t, exp, st, 300)
}

// TestEstimateExpandTwoSourceLabels confirms a source carrying two labels is
// populated by the rarer one, so the fan-out is the type edges over the smaller
// count, the conservative (larger) degree.
func TestEstimateExpandTwoSourceLabels(t *testing.T) {
	st := fakeStats{nodes: 1000, relType: map[uint32]float64{3: 300}, label: map[uint32]float64{1: 200, 2: 50}}
	exp := &Expand{
		Input: &NodeScan{Var: "a", Labels: []bind.NameRef{known(1), known(2)}},
		From:  "a", To: "b", Rel: "r",
		Types:      []bind.NameRef{known(3)},
		FromLabels: []bind.NameRef{known(1), known(2)},
	}
	// Scan of two labels is the rarer (50); degree is 300/50 = 6; 50 * 6 = 300.
	wantRows(t, exp, st, 300)
}

// TestEstimateVarLenExpand confirms a variable-length expand costs the sum of the
// per-hop fan-out over its range, not a single hop, so a wider range costs more.
func TestEstimateVarLenExpand(t *testing.T) {
	st := fakeStats{nodes: 100, rels: 500, relType: map[uint32]float64{3: 300}}
	// Degree 3 (300 KNOWS over 100 nodes), 100 source rows.
	mk := func(min, max int) *Expand {
		return &Expand{
			Input: &NodeScan{Var: "a"},
			From:  "a", To: "b", Rel: "r",
			Types:  []bind.NameRef{known(3)},
			VarLen: &ast.VarLength{Min: min, Max: max},
		}
	}
	// [*1..2]: per source row d + d^2 = 3 + 9 = 12; 100 rows = 1200.
	wantRows(t, mk(1, 2), st, 1200)
	// [*2..3]: d^2 + d^3 = 9 + 27 = 36; 100 rows = 3600.
	wantRows(t, mk(2, 3), st, 3600)
	// [*0..1]: the zero-hop path plus one hop, 1 + 3 = 4; 100 rows = 400.
	wantRows(t, mk(0, 1), st, 400)
	// A single fixed hop stays the bare degree: 100 rows * 3 = 300, the var-length
	// estimate for [*1..1] must agree with it.
	wantRows(t, mk(1, 1), st, 300)
}

// TestEstimateVarLenUnbounded confirms an omitted upper bound is capped at
// DefaultVarLenMaxHops so the estimate stays finite and still grows with the range.
func TestEstimateVarLenUnbounded(t *testing.T) {
	st := fakeStats{nodes: 100, rels: 200} // typeless degree 2
	exp := &Expand{
		Input: &NodeScan{Var: "a"},
		From:  "a", To: "b", Rel: "r",
		VarLen: &ast.VarLength{Min: 1, Max: -1},
	}
	// Capped at DefaultVarLenMaxHops: sum of 2^k for k in 1..6 = 2+4+...+64 = 126;
	// 100 rows = 12600.
	var perRow float64
	for k := 1; k <= DefaultVarLenMaxHops; k++ {
		term := 1.0
		for i := 0; i < k; i++ {
			term *= 2
		}
		perRow += term
	}
	wantRows(t, exp, st, 100*perRow)
}

// TestEstimateFilterEquality confirms an equality filter keeps the equality fraction
// of its input.
func TestEstimateFilterEquality(t *testing.T) {
	st := fakeStats{nodes: 1000}
	f := &Filter{
		Input: &NodeScan{Var: "n"},
		Pred:  &ast.Binary{Op: ast.OpEq, L: &ast.Variable{Name: "x"}, R: lit(1)},
	}
	wantRows(t, f, st, 1000*DefaultEqualitySelectivity)
}

// TestEstimateFilterRange confirms an ordered comparison keeps the range fraction.
func TestEstimateFilterRange(t *testing.T) {
	st := fakeStats{nodes: 1000}
	f := &Filter{
		Input: &NodeScan{Var: "n"},
		Pred:  &ast.Binary{Op: ast.OpGt, L: &ast.Variable{Name: "x"}, R: lit(1)},
	}
	wantRows(t, f, st, 1000*DefaultRangeSelectivity)
}

// TestEstimateFilterConjunction confirms AND multiplies the conjuncts' selectivities
// under the independence assumption.
func TestEstimateFilterConjunction(t *testing.T) {
	st := fakeStats{nodes: 1000}
	pred := &ast.Binary{
		Op: ast.OpAnd,
		L:  &ast.Binary{Op: ast.OpEq, L: &ast.Variable{Name: "x"}, R: lit(1)},
		R:  &ast.Binary{Op: ast.OpGt, L: &ast.Variable{Name: "y"}, R: lit(2)},
	}
	f := &Filter{Input: &NodeScan{Var: "n"}, Pred: pred}
	wantRows(t, f, st, 1000*DefaultEqualitySelectivity*DefaultRangeSelectivity)
}

// TestEstimateFilterNegation confirms NOT is the complement of its operand.
func TestEstimateFilterNegation(t *testing.T) {
	st := fakeStats{nodes: 1000}
	pred := &ast.Unary{Op: ast.OpNot, X: &ast.Binary{Op: ast.OpEq, L: &ast.Variable{Name: "x"}, R: lit(1)}}
	f := &Filter{Input: &NodeScan{Var: "n"}, Pred: pred}
	wantRows(t, f, st, 1000*(1-DefaultEqualitySelectivity))
}

// TestEstimateLimit confirms a literal Limit caps the estimate, and a Limit above
// the input leaves it unchanged.
func TestEstimateLimit(t *testing.T) {
	st := fakeStats{nodes: 1000}
	wantRows(t, &Limit{Input: &NodeScan{Var: "n"}, N: lit(10)}, st, 10)
	wantRows(t, &Limit{Input: &NodeScan{Var: "n"}, N: lit(5000)}, st, 1000)
}

// TestEstimateLimitParam confirms a non-literal Limit (a parameter) leaves the
// estimate at the input, since the cap is unknown until run time.
func TestEstimateLimitParam(t *testing.T) {
	st := fakeStats{nodes: 1000}
	wantRows(t, &Limit{Input: &NodeScan{Var: "n"}, N: &ast.Param{Name: "n"}}, st, 1000)
}

// TestEstimateSkip confirms a literal Skip drops that many rows, floored at zero.
func TestEstimateSkip(t *testing.T) {
	st := fakeStats{nodes: 100}
	wantRows(t, &Skip{Input: &NodeScan{Var: "n"}, N: lit(30)}, st, 70)
	wantRows(t, &Skip{Input: &NodeScan{Var: "n"}, N: lit(500)}, st, 0)
}

// TestEstimateUnwind confirms a leading UNWIND is the assumed list length, and an
// UNWIND over an input multiplies by it.
func TestEstimateUnwind(t *testing.T) {
	st := fakeStats{nodes: 100}
	wantRows(t, &Unwind{Var: "x", Expr: &ast.Variable{Name: "l"}}, st, DefaultListLength)
	wantRows(t, &Unwind{Input: &NodeScan{Var: "n"}, Var: "x", Expr: &ast.Variable{Name: "l"}}, st, 100*DefaultListLength)
}

// TestEstimateJoin confirms an empty key set is a cartesian product and an equijoin
// is estimated as the larger side.
func TestEstimateJoin(t *testing.T) {
	st := fakeStats{label: map[uint32]float64{1: 20, 2: 5}}
	left := &NodeScan{Var: "a", Labels: []bind.NameRef{known(1)}}
	right := &NodeScan{Var: "b", Labels: []bind.NameRef{known(2)}}
	wantRows(t, &Join{Left: left, Right: right}, st, 20*5)
	wantRows(t, &Join{Left: left, Right: right, On: []string{"a"}}, st, 20)
}

// TestEstimateUnion confirms a union is the sum of its arms.
func TestEstimateUnion(t *testing.T) {
	st := fakeStats{label: map[uint32]float64{1: 20, 2: 5}}
	u := &Union{
		Left:  &NodeScan{Var: "a", Labels: []bind.NameRef{known(1)}},
		Right: &NodeScan{Var: "b", Labels: []bind.NameRef{known(2)}},
	}
	wantRows(t, u, st, 25)
}

// TestEstimateAggregate confirms a groupless aggregate is one row and a grouped one
// is bounded by its input.
func TestEstimateAggregate(t *testing.T) {
	st := fakeStats{nodes: 1000}
	wantRows(t, &Aggregate{Input: &NodeScan{Var: "n"}}, st, 1)
	grouped := &Aggregate{Input: &NodeScan{Var: "n"}, GroupKeys: []Col{{Name: "k"}}}
	wantRows(t, grouped, st, 1000)
}

// TestEstimateWritePassThrough confirms a write operator carries its input's
// estimate, so a leading CREATE over the Unit row is one row.
func TestEstimateWritePassThrough(t *testing.T) {
	st := fakeStats{}
	wantRows(t, &Create{Input: &Unit{}}, st, 1)
}
