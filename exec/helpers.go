package exec

import (
	"strconv"
	"strings"

	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// cloneRow copies a row so an operator can extend it without mutating the input
// row it shares with its source.
func cloneRow(r eval.Row) eval.Row {
	out := make(eval.Row, len(r)+1)
	for k, v := range r {
		out[k] = v
	}
	return out
}

// collectArguments walks a compiled operator tree and returns every Argument leaf
// in it, the leaves an Optional feeds with the current outer row.
func collectArguments(op operator) []*argumentOp {
	var out []*argumentOp
	var walk func(operator)
	walk = func(o operator) {
		switch x := o.(type) {
		case *argumentOp:
			out = append(out, x)
		case *nodeScanOp:
		case *unitOp:
		case *expandOp:
			walk(x.input)
		case *createOp:
			walk(x.input)
		case *mergeOp:
			walk(x.input)
			walk(x.match)
		case *foreachOp:
			walk(x.input)
			walk(x.body)
		case *setOp:
			walk(x.input)
		case *removeOp:
			walk(x.input)
		case *deleteOp:
			walk(x.input)
		case *filterOp:
			walk(x.input)
		case *projectOp:
			walk(x.input)
		case *aggregateOp:
			walk(x.input)
		case *unwindOp:
			if x.input != nil {
				walk(x.input)
			}
		case *sortOp:
			walk(x.input)
		case *skipOp:
			walk(x.input)
		case *limitOp:
			walk(x.input)
		case *joinOp:
			walk(x.left)
			walk(x.right)
		case *optionalOp:
			walk(x.input)
			walk(x.inner)
		case *unionOp:
			walk(x.left)
			walk(x.right)
		}
	}
	walk(op)
	return out
}

// diffVars returns the variables the inner subplan produces that the outer input
// does not: the columns an Optional null-pads when its inner finds no match (doc
// 09 §4.2).
func diffVars(inner, input plan.Op) []string {
	outer := make(map[string]bool)
	for _, v := range plan.OutputVars(input) {
		outer[v] = true
	}
	var out []string
	for _, v := range plan.OutputVars(inner) {
		if !outer[v] {
			out = append(out, v)
		}
	}
	return out
}

// rowKey encodes a row's named columns into a canonical string for equality-based
// deduplication and grouping (DISTINCT, UNION, the Aggregate group key). Two rows
// produce the same key exactly when their values compare equal under Cypher
// equality over the given columns, so 1 and 1.0 share a key.
func rowKey(row eval.Row, cols []string) string {
	var b strings.Builder
	for _, c := range cols {
		encodeValue(&b, row[c])
		b.WriteByte(0x1f) // unit separator between columns
	}
	return b.String()
}

// rowKeyAll encodes every column of a row (names sorted) for deduplication when
// the column list is not known to the operator, as in a UNION whose arms share
// their column names.
func rowKeyAll(row eval.Row) string {
	cols := make([]string, 0, len(row))
	for k := range row {
		cols = append(cols, k)
	}
	sortStrings(cols)
	var b strings.Builder
	for _, c := range cols {
		b.WriteString(c)
		b.WriteByte('=')
		encodeValue(&b, row[c])
		b.WriteByte(0x1f)
	}
	return b.String()
}

// valuesKey encodes an ordered slice of values, the group key an Aggregate builds
// from its evaluated grouping expressions.
func valuesKey(vs []value.Value) string {
	var b strings.Builder
	for _, v := range vs {
		encodeValue(&b, v)
		b.WriteByte(0x1f)
	}
	return b.String()
}

// encodeValue writes a type-tagged canonical encoding of one value. Numbers are
// normalized so an integer and its float twin (1 and 1.0) encode identically,
// matching Cypher equality; containers recurse, with map keys sorted.
func encodeValue(b *strings.Builder, v value.Value) {
	switch v.Type() {
	case value.TypeNull:
		b.WriteByte('z')
	case value.TypeBool:
		x, _ := v.AsBool()
		if x {
			b.WriteString("b1")
		} else {
			b.WriteString("b0")
		}
	case value.TypeInt:
		i, _ := v.AsInt()
		b.WriteByte('n')
		b.WriteString(strconv.FormatInt(i, 10))
	case value.TypeFloat:
		encodeNumber(b, v)
	case value.TypeString:
		s, _ := v.AsString()
		b.WriteByte('s')
		b.WriteString(s)
	case value.TypeBytes:
		x, _ := v.AsBytes()
		b.WriteByte('y')
		b.Write(x)
	case value.TypeList:
		lst, _ := v.AsList()
		b.WriteByte('[')
		for _, e := range lst {
			encodeValue(b, e)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	case value.TypeMap:
		m, _ := v.AsMap()
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sortStrings(keys)
		b.WriteByte('{')
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte(':')
			encodeValue(b, m[k])
			b.WriteByte(',')
		}
		b.WriteByte('}')
	case value.TypeNode:
		id, _ := v.AsNode()
		b.WriteByte('N')
		b.WriteString(strconv.FormatUint(id, 10))
	case value.TypeRel:
		id, _ := v.AsRel()
		b.WriteByte('R')
		b.WriteString(strconv.FormatUint(id, 10))
	case value.TypePath:
		elems, _ := v.AsPath()
		b.WriteByte('P')
		for _, e := range elems {
			encodeValue(b, e)
			b.WriteByte(',')
		}
		b.WriteByte('P')
	}
}

// encodeNumber normalizes a float: a whole value that round-trips through int64
// encodes with the integer tag (so 1.0 keys like 1), otherwise it keeps its float
// form, with NaN folded to one token so NaNs group together.
func encodeNumber(b *strings.Builder, v value.Value) {
	f, _ := v.AsFloat()
	switch {
	case f != f: // NaN
		b.WriteString("fNaN")
	case f == float64(int64(f)):
		b.WriteByte('n')
		b.WriteString(strconv.FormatInt(int64(f), 10))
	default:
		b.WriteByte('f')
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	}
}

// sortStrings is an insertion sort, kept local so this file needs no sort import
// for its small key slices.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
