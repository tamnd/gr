package plan

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// Statistics is the planner's read-only view of the catalog's cardinality
// counters, the numbers the cost model turns into per-operator row estimates (doc
// 11 §2.2). NodeCount and RelCount are the live totals; LabelCount and RelTypeCount
// are the per-token counts the storage layer maintains on the write path (doc 04
// §19.1). All four are counts of rows, returned as float64 so the estimator can
// multiply them by fractional selectivities without converting at every step.
//
// The counts are the latest committed totals, not a snapshot read, so an estimate
// is built against the catalog as it stands when the plan is built. That is the
// right basis for planning: the plan cache already keys on the catalog version, so
// a plan and the counts it was costed against move together.
type Statistics interface {
	// NodeCount is the total number of nodes.
	NodeCount() float64
	// RelCount is the total number of relationships.
	RelCount() float64
	// LabelCount is the number of nodes carrying a label token.
	LabelCount(label uint32) float64
	// RelTypeCount is the number of relationships of a type token.
	RelTypeCount(relType uint32) float64
}

// The default selectivities are the fractions the cost model falls back to when it
// has no distribution to read (doc 11 §2.3). The storage layer keeps no per-value
// distinct-counts or histograms yet, so every predicate uses one of these magic
// constants; when those statistics land, the equality and range cases read the real
// distribution and only the unclassified case keeps a constant. They are the one
// place to tune the model's pessimism, so they are named rather than inlined.
const (
	// DefaultEqualitySelectivity is the fraction an equality predicate keeps when the
	// property's distinct-value count is unknown.
	DefaultEqualitySelectivity = 0.1
	// DefaultRangeSelectivity is the fraction a range, string, or membership
	// predicate keeps when the value distribution is unknown.
	DefaultRangeSelectivity = 0.3
	// DefaultPredSelectivity is the fraction any other predicate keeps when nothing
	// about it can be classified.
	DefaultPredSelectivity = 0.25
	// DefaultListLength is the assumed length of the list an UNWIND expands when the
	// length cannot be read from the expression.
	DefaultListLength = 10.0
)

// EstimateRows estimates how many rows an operator tree produces, the cardinality
// the cost-based planner compares access paths and join orders on (doc 11 §2.2). It
// is a pure function of the tree and the statistics, computed bottom-up: a leaf's
// estimate comes from the counts, and each operator scales its input's estimate by
// the selectivity or fan-out it applies. The result is a float, never rounded here,
// so a chain of small selectivities does not collapse to zero before the planner can
// compare two chains; a caller that displays it rounds at the edge.
//
// Several cases are deliberate approximations the spec calls out as refinable once
// richer statistics exist (doc 11 §2.3, §2.4): an equality keeps a constant fraction
// rather than one over the distinct count, a grouped aggregate is bounded by its
// input rather than estimated from group counts, and a variable-length expand is
// costed as a single hop. Each is an over-estimate, never an under-estimate, so the
// planner stays conservative until the statistics that sharpen it arrive.
func EstimateRows(o Op, st Statistics) float64 {
	switch x := o.(type) {
	case *Unit:
		return 1
	case *Argument:
		return 1
	case *NodeScan:
		return scanRows(x.Labels, st)
	case *NodeIndexSeek:
		rows := labelRows(x.Label, st) * DefaultEqualitySelectivity
		for range x.Rest {
			rows *= DefaultPredSelectivity
		}
		return rows
	case *Expand:
		rows := EstimateRows(x.Input, st) * avgDegree(x.Types, st)
		if x.ToBound {
			// An expand-into keeps only the edges that reach an already-bound node, far
			// fewer than the full fan-out.
			rows *= DefaultEqualitySelectivity
		}
		for range x.ToLabels {
			rows *= DefaultPredSelectivity
		}
		return rows
	case *Filter:
		return EstimateRows(x.Input, st) * selectivity(x.Pred)
	case *BindPath:
		return EstimateRows(x.Input, st)
	case *ShortestPath:
		// One shortest path per input row, the common case; allShortestPaths can yield
		// more, refined when traversal statistics land.
		return EstimateRows(x.Input, st)
	case *Project:
		return EstimateRows(x.Input, st)
	case *Aggregate:
		if len(x.GroupKeys) == 0 {
			return 1
		}
		// Without per-key distinct counts the group count is bounded by the input.
		return EstimateRows(x.Input, st)
	case *Join:
		return joinRows(x, st)
	case *Optional:
		// A left-outer join keeps every input row; inner fan-out is ignored until
		// correlated estimation lands, so this is a lower bound on a fan-out match.
		return EstimateRows(x.Input, st)
	case *Unwind:
		if x.Input == nil {
			return DefaultListLength
		}
		return EstimateRows(x.Input, st) * DefaultListLength
	case *Sort:
		return EstimateRows(x.Input, st)
	case *Skip:
		in := EstimateRows(x.Input, st)
		if n, ok := litCount(x.N); ok {
			return max0(in - n)
		}
		return in
	case *Limit:
		in := EstimateRows(x.Input, st)
		if n, ok := litCount(x.N); ok && n < in {
			return n
		}
		return in
	case *Union:
		return EstimateRows(x.Left, st) + EstimateRows(x.Right, st)
	case *Create:
		return EstimateRows(x.Input, st)
	case *Merge:
		return EstimateRows(x.Input, st)
	case *Foreach:
		return EstimateRows(x.Input, st)
	case *Set:
		return EstimateRows(x.Input, st)
	case *Remove:
		return EstimateRows(x.Input, st)
	case *Delete:
		return EstimateRows(x.Input, st)
	default:
		return 1
	}
}

// scanRows estimates a node scan's output. An all-nodes scan (no labels) is the
// whole node count; a labeled scan is the smallest of its labels' counts, since a
// node carrying every label is at most as common as its rarest one. An unknown
// label matches nothing (the schema-optional rule, doc 08 §5.3), so it yields zero.
func scanRows(labels []bind.NameRef, st Statistics) float64 {
	if len(labels) == 0 {
		return st.NodeCount()
	}
	rows := labelRows(labels[0], st)
	for _, l := range labels[1:] {
		if r := labelRows(l, st); r < rows {
			rows = r
		}
	}
	return rows
}

// labelRows is the node count for one label reference, zero for a label the catalog
// never interned (it matches nothing).
func labelRows(ref bind.NameRef, st Statistics) float64 {
	if !ref.Known {
		return 0
	}
	return st.LabelCount(uint32(ref.Token))
}

// avgDegree estimates the average number of relationships a node expands along. With
// no types it is the whole relationship count over the node count; with types it is
// the summed counts of those types over the node count. An unknown type contributes
// nothing, and a graph with no nodes has degree zero rather than a divide by zero.
func avgDegree(types []bind.NameRef, st Statistics) float64 {
	n := st.NodeCount()
	if n <= 0 {
		return 0
	}
	if len(types) == 0 {
		return st.RelCount() / n
	}
	var rels float64
	for _, t := range types {
		if t.Known {
			rels += st.RelTypeCount(uint32(t.Token))
		}
	}
	return rels / n
}

// joinRows estimates a join's output. An empty key set is a cartesian product, the
// product of the two sides; an equijoin on shared keys is estimated as the larger
// side, the foreign-key-to-primary-key shape where each row on the many side matches
// about one row on the one side. This is refined to a distinct-count division when
// per-key statistics land (doc 11 §2.2).
func joinRows(x *Join, st Statistics) float64 {
	l := EstimateRows(x.Left, st)
	r := EstimateRows(x.Right, st)
	if len(x.On) == 0 {
		return l * r
	}
	if l > r {
		return l
	}
	return r
}

// selectivity estimates the fraction of rows a predicate keeps (doc 11 §2.3).
// Conjunction multiplies under an independence assumption, disjunction combines by
// inclusion-exclusion, and negation is the complement; a comparison reads its
// default fraction by operator class, and anything unclassified keeps the catch-all
// fraction. The result is clamped to [0,1] so a composed estimate stays a fraction.
func selectivity(pred ast.Expr) float64 {
	return clamp01(rawSelectivity(pred))
}

func rawSelectivity(pred ast.Expr) float64 {
	switch p := pred.(type) {
	case *ast.Binary:
		switch p.Op {
		case ast.OpEq:
			return DefaultEqualitySelectivity
		case ast.OpNe:
			return 1 - DefaultEqualitySelectivity
		case ast.OpLt, ast.OpLe, ast.OpGt, ast.OpGe,
			ast.OpStartsWith, ast.OpEndsWith, ast.OpContains, ast.OpRegex, ast.OpIn:
			return DefaultRangeSelectivity
		case ast.OpAnd:
			return rawSelectivity(p.L) * rawSelectivity(p.R)
		case ast.OpOr:
			a, b := rawSelectivity(p.L), rawSelectivity(p.R)
			return a + b - a*b
		case ast.OpXor:
			a, b := rawSelectivity(p.L), rawSelectivity(p.R)
			return a + b - 2*a*b
		default:
			return DefaultPredSelectivity
		}
	case *ast.Unary:
		if p.Op == ast.OpNot {
			return 1 - rawSelectivity(p.X)
		}
		return DefaultPredSelectivity
	case *ast.IsNull:
		return DefaultRangeSelectivity
	default:
		return DefaultPredSelectivity
	}
}

// litCount reads an integer literal row count from a Skip or Limit argument,
// reporting false for a parameter or any non-literal whose value is unknown until
// run time. A negative literal is treated as zero rows.
func litCount(e ast.Expr) (float64, bool) {
	lit, ok := e.(*ast.Literal)
	if !ok {
		return 0, false
	}
	if n, ok := lit.Value.AsInt(); ok {
		return max0(float64(n)), true
	}
	return 0, false
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func max0(x float64) float64 {
	if x < 0 {
		return 0
	}
	return x
}
