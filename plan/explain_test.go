package plan

import (
	"strings"
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// eqFilter builds a Filter of x = 1 over an input, the shape several of these tests
// reuse.
func eqFilter(in Op) *Filter {
	return &Filter{
		Input: in,
		Pred:  &ast.Binary{Op: ast.OpEq, L: &ast.Variable{Name: "x"}, R: lit(1)},
	}
}

// TestStringWithRowsAnnotatesEveryOperator confirms the annotated renderer keeps the
// tree String produces and appends an estimate to each line. The plan is a filter
// over a labeled scan, so the listing names both operators and carries a row count on
// each.
func TestStringWithRowsAnnotatesEveryOperator(t *testing.T) {
	st := fakeStats{nodes: 1000, label: map[uint32]float64{1: 200}}
	tree := eqFilter(&NodeScan{Var: "n", Labels: []bind.NameRef{known(1)}})

	got := StringWithRows(tree, st)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("annotated listing = %d lines, want 2:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "Filter ") || !strings.Contains(lines[0], "(est. rows ") {
		t.Fatalf("filter line not annotated: %q", lines[0])
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "NodeScan ") || !strings.Contains(lines[1], "(est. rows ") {
		t.Fatalf("scan line not annotated: %q", lines[1])
	}
}

// TestStringWithRowsMatchesEstimate confirms the number on a line is exactly the cost
// model's estimate for that subtree, rounded to a whole row: a 200-node labeled scan
// shows 200, and the equality filter above it shows the 20 the equality selectivity
// gives.
func TestStringWithRowsMatchesEstimate(t *testing.T) {
	st := fakeStats{nodes: 1000, label: map[uint32]float64{1: 200}}
	tree := eqFilter(&NodeScan{Var: "n", Labels: []bind.NameRef{known(1)}})

	got := StringWithRows(tree, st)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if !strings.HasSuffix(lines[0], "(est. rows 20)") {
		t.Fatalf("filter estimate missing or wrong: %q", lines[0])
	}
	if !strings.Contains(got, "NodeScan n:#1  (est. rows 200)") {
		t.Fatalf("scan estimate missing or wrong:\n%s", got)
	}
}

// TestStringMatchesStringWithRowsShape confirms the annotated renderer changes only
// the per-line suffix: stripping the estimate from every annotated line gives back
// exactly the plain listing String produces, so the two never disagree on the tree.
func TestStringMatchesStringWithRowsShape(t *testing.T) {
	st := fakeStats{nodes: 1000, label: map[uint32]float64{1: 200}}
	tree := eqFilter(&NodeScan{Var: "n", Labels: []bind.NameRef{known(1)}})

	plain := String(tree)
	annotated := StringWithRows(tree, st)
	var stripped strings.Builder
	for ln := range strings.SplitSeq(annotated, "\n") {
		if i := strings.Index(ln, "  (est. rows "); i >= 0 {
			ln = ln[:i]
		}
		stripped.WriteString(ln)
		stripped.WriteString("\n")
	}
	if got := strings.TrimRight(stripped.String(), "\n"); got != strings.TrimRight(plain, "\n") {
		t.Fatalf("stripped annotated listing != plain:\n%s\n---\n%s", got, plain)
	}
}

// TestFormatRows confirms the row formatter rounds to a whole count and never prints
// a kept operator as zero rows.
func TestFormatRows(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{0.4, "1"},   // below one but possible: rounds up to a row
		{20, "20"},   // exact
		{24.6, "25"}, // rounds to nearest
		{-3, "0"},    // never negative
	}
	for _, c := range cases {
		if got := formatRows(c.in); got != c.want {
			t.Fatalf("formatRows(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
