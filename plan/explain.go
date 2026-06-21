package plan

import (
	"math"
	"strconv"
	"strings"
)

// StringWithRows renders the operator tree the way String does, but annotates each
// operator with the rows the cost model estimates it produces (doc 11 §2, doc 25
// §7.2). It is what EXPLAIN shows when the engine can supply statistics, so the
// listing names not just the plan the planner chose but the cardinalities it chose
// it on.
//
// The plain String stays the canonical form the planner golden tests assert
// against; the estimate is additive, appended after the operator's own text, so the
// annotated listing reads as the same tree with a rows column.
func StringWithRows(o Op, st Statistics) string {
	var b strings.Builder
	writeRows(&b, o, st, 0)
	return b.String()
}

func writeRows(b *strings.Builder, o Op, st Statistics, depth int) {
	for range depth {
		b.WriteString("  ")
	}
	b.WriteString(nodeLabel(o))
	b.WriteString("  (est. rows ")
	b.WriteString(formatRows(EstimateRows(o, st)))
	b.WriteString(")\n")
	for _, c := range nodeChildren(o) {
		writeRows(b, c, st, depth+1)
	}
}

// formatRows renders an estimate as a whole number of rows. The cost model works in
// float64 so a chain of selectivities does not truncate mid-computation, but a row
// count is a count, so the listing rounds to the nearest integer; an estimate below
// one still shows as zero rounds up to one, since an operator the planner kept
// produces at least the possibility of a row.
func formatRows(v float64) string {
	if v < 0 {
		v = 0
	}
	n := math.Round(v)
	if n < 1 && v > 0 {
		n = 1
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}
