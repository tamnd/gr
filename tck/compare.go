package tck

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	gr "github.com/tamnd/gr"
)

// Ordering controls whether a result table comparison is positional or multiset.
type Ordering int

const (
	AnyOrder Ordering = iota // rows compared as an unordered multiset
	InOrder                  // rows must match positionally (for ORDER BY queries)
)

// compareResultTable compares a *gr.Result against an expected Gherkin table
// (where the first row is the column header).  It drains the result.
// Returns "" on match, a diff string on mismatch.
func compareResultTable(res *gr.Result, table [][]string, ord Ordering) string {
	if len(table) == 0 {
		return compareEmptyResult(res)
	}
	headers := table[0]
	wantRows := table[1:]

	// Drain the result into a slice.
	var gotRows []map[string]gr.Value
	for res.Next() {
		rec := res.Record()
		row := make(map[string]gr.Value, len(headers))
		for _, col := range headers {
			v, _ := rec.Get(col)
			row[col] = v
		}
		gotRows = append(gotRows, row)
	}
	if err := res.Err(); err != nil {
		return fmt.Sprintf("result error: %v", err)
	}

	if len(gotRows) != len(wantRows) {
		return fmt.Sprintf("row count: got %d, want %d", len(gotRows), len(wantRows))
	}
	if len(wantRows) == 0 {
		return ""
	}

	switch ord {
	case InOrder:
		for i := range gotRows {
			if diff := rowDiff(headers, gotRows[i], wantRows[i]); diff != "" {
				return fmt.Sprintf("row %d: %s", i, diff)
			}
		}
	case AnyOrder:
		return multisetDiff(headers, gotRows, wantRows)
	}
	return ""
}

// compareEmptyResult checks that the result has no rows.
func compareEmptyResult(res *gr.Result) string {
	count := 0
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		return fmt.Sprintf("result error: %v", err)
	}
	if count != 0 {
		return fmt.Sprintf("expected no results, got %d rows", count)
	}
	return ""
}

// rowDiff compares one got row to one want row, both keyed by column headers.
// Returns "" on match, a descriptive diff otherwise.
func rowDiff(headers []string, got map[string]gr.Value, want []string) string {
	for i, col := range headers {
		if i >= len(want) {
			break
		}
		gotV := got[col]
		wantS := want[i]
		if !matchValue(gotV, wantS) {
			return fmt.Sprintf("column %q: got %s, want %s", col, formatValue(gotV), wantS)
		}
	}
	return ""
}

// multisetDiff verifies that gotRows and wantRows are equal as multisets.
// Returns "" on match, a diff string otherwise.
func multisetDiff(headers []string, gotRows []map[string]gr.Value, wantRows [][]string) string {
	matched := make([]bool, len(gotRows))
	for _, want := range wantRows {
		found := false
		for gi, got := range gotRows {
			if !matched[gi] && rowDiff(headers, got, want) == "" {
				matched[gi] = true
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("expected row %v not found in result", want)
		}
	}
	return ""
}

// matchValue reports whether the actual gr value matches the TCK cell string.
// gr.Value is any; the concrete types are: nil, bool, int64, float64, string,
// []byte, []any, map[string]any, gr.Node, gr.Relationship, gr.Path.
func matchValue(got gr.Value, want string) bool {
	want = strings.TrimSpace(want)

	if want == "null" {
		return got == nil
	}
	if want == "true" {
		b, ok := got.(bool)
		return ok && b
	}
	if want == "false" {
		b, ok := got.(bool)
		return ok && !b
	}

	// String literal: 'text' or "text".
	if (strings.HasPrefix(want, "'") && strings.HasSuffix(want, "'")) ||
		(strings.HasPrefix(want, `"`) && strings.HasSuffix(want, `"`)) {
		s := want[1 : len(want)-1]
		gs, ok := got.(string)
		return ok && gs == s
	}

	// Integer.
	if n, err := strconv.ParseInt(want, 10, 64); err == nil {
		return matchInteger(got, n)
	}

	// Float.
	if f, err := strconv.ParseFloat(want, 64); err == nil {
		return matchFloat(got, f)
	}

	// List: [...]
	if strings.HasPrefix(want, "[") && strings.HasSuffix(want, "]") {
		// Could be a list or a relationship pattern — check.
		inner := strings.TrimSpace(want[1 : len(want)-1])
		if strings.HasPrefix(inner, ":") {
			// Relationship: [:TYPE] or [:TYPE {k:v}]
			return matchRel(got, want)
		}
		return matchList(got, want)
	}

	// Map: {...}
	if strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}") {
		return matchMap(got, want)
	}

	// Node: (:Label {k:v}) or (:Label) or ()
	if strings.HasPrefix(want, "(") && strings.HasSuffix(want, ")") {
		return matchNode(got, want)
	}

	// Fallback: compare string representations.
	return formatValue(got) == want
}

func matchInteger(got gr.Value, want int64) bool {
	switch v := got.(type) {
	case int64:
		return v == want
	case int:
		return int64(v) == want
	}
	return false
}

func matchFloat(got gr.Value, want float64) bool {
	switch v := got.(type) {
	case float64:
		return v == want
	case float32:
		return float64(v) == want
	}
	return false
}

// matchList parses a TCK list cell like "[1, 2, 'x']" and matches against a
// gr list value ([]any).
func matchList(got gr.Value, want string) bool {
	inner := strings.TrimSpace(want[1 : len(want)-1])
	wantElems := splitCommas(inner)

	gotElems, ok := got.([]gr.Value)
	if !ok {
		return false
	}

	if len(gotElems) != len(wantElems) {
		return false
	}
	for i, w := range wantElems {
		if !matchValue(gotElems[i], strings.TrimSpace(w)) {
			return false
		}
	}
	return true
}

// matchMap parses "{k:v, ...}" and matches against a gr map value (map[string]any).
func matchMap(got gr.Value, want string) bool {
	inner := strings.TrimSpace(want[1 : len(want)-1])
	wantPairs := parsePropMap(inner)

	gotMap, ok := got.(map[string]gr.Value)
	if !ok {
		return false
	}

	if len(gotMap) != len(wantPairs) {
		return false
	}
	for k, wv := range wantPairs {
		gv, ok := gotMap[k]
		if !ok || !matchValue(gv, wv) {
			return false
		}
	}
	return true
}

// matchNode matches a gr.Node against a TCK node pattern like (:Label {k:v}).
func matchNode(got gr.Value, want string) bool {
	n, ok := got.(gr.Node)
	if !ok {
		return false
	}
	inner := strings.TrimSpace(want[1 : len(want)-1])
	// Parse labels and optional props.
	labels, props := parseNodePattern(inner)
	gotLabels := n.Labels()
	if !labelSetEqual(gotLabels, labels) {
		return false
	}
	if props != "" {
		gotProps := n.Props()
		wantPairs := parsePropMap(strings.TrimSpace(props[1 : len(props)-1]))
		if len(gotProps) != len(wantPairs) {
			return false
		}
		for k, wv := range wantPairs {
			gv, ok := gotProps[k]
			if !ok || !matchValue(gv, wv) {
				return false
			}
		}
	}
	return true
}

// matchRel matches a gr.Relationship against a TCK relationship cell like [:TYPE].
func matchRel(got gr.Value, want string) bool {
	r, ok := got.(gr.Relationship)
	if !ok {
		return false
	}
	inner := strings.TrimSpace(want[1 : len(want)-1])
	if !strings.HasPrefix(inner, ":") {
		return false
	}
	inner = inner[1:]
	typePart, propsPart := "", ""
	if idx := strings.Index(inner, "{"); idx >= 0 {
		typePart = strings.TrimSpace(inner[:idx])
		propsPart = strings.TrimSpace(inner[idx:])
	} else {
		typePart = strings.TrimSpace(inner)
	}
	if typePart != "" && r.Type() != typePart {
		return false
	}
	if propsPart != "" {
		wantPairs := parsePropMap(strings.TrimSpace(propsPart[1 : len(propsPart)-1]))
		gotProps := r.Props()
		for k, wv := range wantPairs {
			gv, ok := gotProps[k]
			if !ok || !matchValue(gv, wv) {
				return false
			}
		}
	}
	return true
}

// formatValue renders a gr value in TCK-like notation for diff messages.
func formatValue(v gr.Value) string {
	if v == nil {
		return "null"
	}
	switch v := v.(type) {
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case string:
		return "'" + v + "'"
	case []byte:
		return fmt.Sprintf("%x", v)
	case []gr.Value:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = formatValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]gr.Value:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + formatValue(v[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case gr.Node:
		labels := v.Labels()
		sort.Strings(labels)
		lb := ""
		for _, l := range labels {
			lb += ":" + l
		}
		return "(" + lb + " " + formatProps(v.Props()) + ")"
	case gr.Relationship:
		return "[:" + v.Type() + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func formatProps(m map[string]gr.Value) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + formatValue(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// splitCommas splits a comma-separated list, respecting brackets and quotes.
func splitCommas(s string) []string {
	var parts []string
	depth := 0
	inStr := false
	strChar := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == strChar {
				inStr = false
			}
			continue
		}
		if c == '\'' || c == '"' {
			inStr = true
			strChar = c
			continue
		}
		switch c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// parsePropMap parses "k: v, k2: v2" into a map.
func parsePropMap(s string) map[string]string {
	m := make(map[string]string)
	parts := splitCommas(s)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, ":")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		m[k] = v
	}
	return m
}

// parseNodePattern parses ":Label1:Label2 {props}" into labels and props string.
func parseNodePattern(inner string) (labels []string, props string) {
	for strings.HasPrefix(inner, ":") {
		inner = inner[1:]
		idx := strings.IndexAny(inner, ": {}")
		if idx < 0 {
			labels = append(labels, inner)
			return
		}
		lbl := strings.TrimSpace(inner[:idx])
		if lbl != "" {
			labels = append(labels, lbl)
		}
		inner = strings.TrimSpace(inner[idx:])
	}
	if strings.HasPrefix(inner, "{") {
		props = inner
	}
	return
}

// labelSetEqual reports whether two label slices contain the same labels.
func labelSetEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	wset := make(map[string]bool, len(want))
	for _, l := range want {
		wset[l] = true
	}
	for _, l := range got {
		if !wset[l] {
			return false
		}
	}
	return true
}
