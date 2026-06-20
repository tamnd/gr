package eval

import (
	"bytes"
	"math"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/value"
)

// eq3 is Cypher value equality under three-valued logic (doc 02 §7.2): a null
// operand yields null, numbers compare across int and float (1 = 1.0 is true),
// and unequal types are not equal. NaN is not equal to anything, including
// itself.
func eq3(l, r value.Value) value.Value {
	if l.IsNull() || r.IsNull() {
		return value.Null
	}
	return value.Bool(valuesEqual(l, r))
}

// neg3 negates a three-valued boolean: null stays null, true/false flip. It
// turns = into <> without re-deriving the comparison.
func neg3(v value.Value) value.Value {
	if v.IsNull() {
		return value.Null
	}
	b, _ := v.AsBool()
	return value.Bool(!b)
}

// valuesEqual is the boolean kernel of equality over two non-null values, also
// used by IN membership. Numbers compare numerically; entities by element id;
// lists and maps element-wise. (A null buried inside a list or map is compared by
// identity here rather than propagating to null; full nested-null propagation is
// a later refinement, noted in the implementation doc.)
func valuesEqual(l, r value.Value) bool {
	lt, rt := l.Type(), r.Type()
	if isNumber(lt) && isNumber(rt) {
		if lt == value.TypeInt && rt == value.TypeInt {
			li, _ := l.AsInt()
			ri, _ := r.AsInt()
			return li == ri
		}
		lf, _ := l.AsFloat()
		rf, _ := r.AsFloat()
		return lf == rf // NaN == NaN is false, the intended Cypher result
	}
	if lt != rt {
		return false
	}
	switch lt {
	case value.TypeBool:
		lb, _ := l.AsBool()
		rb, _ := r.AsBool()
		return lb == rb
	case value.TypeString:
		ls, _ := l.AsString()
		rs, _ := r.AsString()
		return ls == rs
	case value.TypeBytes, value.TypeNode, value.TypeRel, value.TypeNull:
		return l.Equal(r)
	case value.TypeList:
		ll, _ := l.AsList()
		rl, _ := r.AsList()
		if len(ll) != len(rl) {
			return false
		}
		for i := range ll {
			if !valuesEqual(ll[i], rl[i]) {
				return false
			}
		}
		return true
	case value.TypeMap:
		lm, _ := l.AsMap()
		rm, _ := r.AsMap()
		if len(lm) != len(rm) {
			return false
		}
		for k, lv := range lm {
			rv, ok := rm[k]
			if !ok || !valuesEqual(lv, rv) {
				return false
			}
		}
		return true
	}
	return false
}

// compare3 evaluates an ordering comparison (<, <=, >, >=) under three-valued
// logic: a null operand, or an incomparable pair (cross-type, or a NaN), yields
// null in predicate context (doc 02 §7.3).
func compare3(op ast.BinaryOp, l, r value.Value) value.Value {
	if l.IsNull() || r.IsNull() {
		return value.Null
	}
	c, ok := orderCompare(l, r)
	if !ok {
		return value.Null
	}
	switch op {
	case ast.OpLt:
		return value.Bool(c < 0)
	case ast.OpLe:
		return value.Bool(c <= 0)
	case ast.OpGt:
		return value.Bool(c > 0)
	case ast.OpGe:
		return value.Bool(c >= 0)
	}
	return value.Null
}

// orderCompare is the predicate-context comparison: it orders only compatible
// values and reports ok=false for pairs that are incomparable in a predicate
// (different types other than numbers, or a NaN). Strings order by Unicode code
// point, which for valid UTF-8 is byte order.
func orderCompare(l, r value.Value) (int, bool) {
	lt, rt := l.Type(), r.Type()
	if isNumber(lt) && isNumber(rt) {
		if lt == value.TypeInt && rt == value.TypeInt {
			li, _ := l.AsInt()
			ri, _ := r.AsInt()
			return cmpInt(li, ri), true
		}
		lf, _ := l.AsFloat()
		rf, _ := r.AsFloat()
		if math.IsNaN(lf) || math.IsNaN(rf) {
			return 0, false
		}
		return cmpFloat(lf, rf), true
	}
	if lt != rt {
		return 0, false
	}
	switch lt {
	case value.TypeString:
		ls, _ := l.AsString()
		rs, _ := r.AsString()
		return strings.Compare(ls, rs), true
	case value.TypeBool:
		lb, _ := l.AsBool()
		rb, _ := r.AsBool()
		return cmpBool(lb, rb), true
	case value.TypeBytes:
		lb, _ := l.AsBytes()
		rb, _ := r.AsBytes()
		return bytes.Compare(lb, rb), true
	default:
		return 0, false
	}
}

// Order is the ORDER BY / DISTINCT total order over values (doc 02 §7.4): every
// pair is ordered. Cross-type pairs order by a fixed type rank, NaN sorts as the
// greatest number, and null sorts last. Within a type it agrees with the
// predicate comparison; lists order lexicographically, maps by sorted key then
// value.
func Order(l, r value.Value) int {
	lr, rr := orderRank(l), orderRank(r)
	if lr != rr {
		return cmpInt(int64(lr), int64(rr))
	}
	switch l.Type() {
	case value.TypeNull:
		return 0
	case value.TypeBool:
		lb, _ := l.AsBool()
		rb, _ := r.AsBool()
		return cmpBool(lb, rb)
	case value.TypeInt, value.TypeFloat:
		return orderNumber(l, r)
	case value.TypeString:
		ls, _ := l.AsString()
		rs, _ := r.AsString()
		return strings.Compare(ls, rs)
	case value.TypeBytes:
		lb, _ := l.AsBytes()
		rb, _ := r.AsBytes()
		return bytes.Compare(lb, rb)
	case value.TypeNode:
		li, _ := l.AsNode()
		ri, _ := r.AsNode()
		return cmpUint(li, ri)
	case value.TypeRel:
		li, _ := l.AsRel()
		ri, _ := r.AsRel()
		return cmpUint(li, ri)
	case value.TypeList:
		ll, _ := l.AsList()
		rl, _ := r.AsList()
		n := len(ll)
		if len(rl) < n {
			n = len(rl)
		}
		for i := 0; i < n; i++ {
			if c := Order(ll[i], rl[i]); c != 0 {
				return c
			}
		}
		return cmpInt(int64(len(ll)), int64(len(rl)))
	case value.TypeMap:
		return orderMap(l, r)
	}
	return 0
}

// orderRank fixes the cross-type sort order (doc 02 §7.4): numbers, then strings,
// booleans, then composites and entities, with null last.
func orderRank(v value.Value) int {
	switch v.Type() {
	case value.TypeInt, value.TypeFloat:
		return 0
	case value.TypeString:
		return 1
	case value.TypeBool:
		return 2
	case value.TypeBytes:
		return 3
	case value.TypeList:
		return 4
	case value.TypeMap:
		return 5
	case value.TypeNode:
		return 6
	case value.TypeRel:
		return 7
	default: // TypeNull
		return 8
	}
}

// orderNumber compares two numbers for the total order, placing NaN above every
// other number.
func orderNumber(l, r value.Value) int {
	if l.Type() == value.TypeInt && r.Type() == value.TypeInt {
		li, _ := l.AsInt()
		ri, _ := r.AsInt()
		return cmpInt(li, ri)
	}
	lf, _ := l.AsFloat()
	rf, _ := r.AsFloat()
	ln, rn := math.IsNaN(lf), math.IsNaN(rf)
	switch {
	case ln && rn:
		return 0
	case ln:
		return 1
	case rn:
		return -1
	}
	return cmpFloat(lf, rf)
}

func orderMap(l, r value.Value) int {
	lm, _ := l.AsMap()
	rm, _ := r.AsMap()
	lk, rk := sortedKeys(lm), sortedKeys(rm)
	n := len(lk)
	if len(rk) < n {
		n = len(rk)
	}
	for i := 0; i < n; i++ {
		if c := strings.Compare(lk[i], rk[i]); c != 0 {
			return c
		}
		if c := Order(lm[lk[i]], rm[rk[i]]); c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(lk)), int64(len(rk)))
}

func sortedKeys(m map[string]value.Value) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	// insertion sort: maps in queries are small, and this avoids a sort import.
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j-1] > ks[j]; j-- {
			ks[j-1], ks[j] = ks[j], ks[j-1]
		}
	}
	return ks
}

func isNumber(t value.Type) bool { return t == value.TypeInt || t == value.TypeFloat }

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}
