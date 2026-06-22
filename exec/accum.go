package exec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/value"
)

// errNoScanLeaf guards the parallel aggregation path: a worker's compiled
// pipeline copy must bottom out in a nodeScanOp to be pointed at the morsel
// source. parallelLeaf already proved the plan has that shape, so this is a
// belt-and-braces error, never expected to fire.
var errNoScanLeaf = errors.New("exec: parallel aggregate input has no scan leaf")

// mergeable is an accumulator that folds another partial of the same kind into
// itself, so a morsel-parallel aggregation can combine each worker's partial into
// one result (doc 12 §10). Only the order-independent accumulators implement it
// (count, min, max); the parallel path runs over exactly those, so the type
// assertion in the merge step is always satisfied.
type mergeable interface {
	merge(other accumulator)
}

// accumulator is one aggregate's running state over a group's rows. add folds in
// one value (nulls are dropped, the Cypher aggregate convention); result returns
// the aggregate over the values seen so far. The set is intentionally small for
// M2: count, sum, avg, min, max, collect (doc 09 §8.1).
type accumulator interface {
	add(v value.Value) error
	result() value.Value
}

// newAccumulator builds the accumulator for an aggregate call, wrapping it for
// DISTINCT when the call has it (count(DISTINCT x), collect(DISTINCT x)). The
// caller has already rejected the unsupported statistical aggregates.
func newAccumulator(call *ast.FunctionCall) accumulator {
	var base accumulator
	switch strings.ToLower(call.Name) {
	case "sum":
		base = &sumAcc{}
	case "avg":
		base = &avgAcc{}
	case "min":
		base = &minMaxAcc{}
	case "max":
		base = &minMaxAcc{max: true}
	case "collect":
		base = &collectAcc{}
	default: // count (and count(*))
		base = &countAcc{}
	}
	if call.Distinct {
		return &distinctAcc{inner: base}
	}
	return base
}

// countAcc counts the non-null values it sees (count(*) feeds a non-null sentinel,
// so it counts every row).
type countAcc struct{ n int64 }

func (a *countAcc) add(v value.Value) error {
	if !v.IsNull() {
		a.n++
	}
	return nil
}
func (a *countAcc) result() value.Value     { return value.Int(a.n) }
func (a *countAcc) merge(other accumulator) { a.n += other.(*countAcc).n }

// sumAcc sums numeric values, staying integer until a float appears (doc 02 §5.2).
// The sum of no values is integer zero.
type sumAcc struct {
	i         int64
	f         float64
	seenFloat bool
}

func (a *sumAcc) add(v value.Value) error {
	if v.IsNull() {
		return nil
	}
	switch v.Type() {
	case value.TypeInt:
		x, _ := v.AsInt()
		a.i += x
	case value.TypeFloat:
		x, _ := v.AsFloat()
		a.f += x
		a.seenFloat = true
	default:
		return fmt.Errorf("exec: sum requires numbers, got %s", v.Type())
	}
	return nil
}
func (a *sumAcc) result() value.Value {
	if a.seenFloat {
		return value.Float(a.f + float64(a.i))
	}
	return value.Int(a.i)
}

// avgAcc is the arithmetic mean of the non-null numeric values; the mean of none
// is null.
type avgAcc struct {
	sum   float64
	count int64
}

func (a *avgAcc) add(v value.Value) error {
	if v.IsNull() {
		return nil
	}
	f, ok := v.AsFloat()
	if !ok {
		return fmt.Errorf("exec: avg requires numbers, got %s", v.Type())
	}
	a.sum += f
	a.count++
	return nil
}
func (a *avgAcc) result() value.Value {
	if a.count == 0 {
		return value.Null
	}
	return value.Float(a.sum / float64(a.count))
}

// minMaxAcc keeps the least (or, with max, the greatest) value under the total
// order ([eval.Order]); over no values it is null. Nulls are dropped before the
// comparison, so they never win.
type minMaxAcc struct {
	best value.Value
	has  bool
	max  bool
}

func (a *minMaxAcc) add(v value.Value) error {
	if v.IsNull() {
		return nil
	}
	if !a.has {
		a.best, a.has = v, true
		return nil
	}
	c := eval.Order(v, a.best)
	if (a.max && c > 0) || (!a.max && c < 0) {
		a.best = v
	}
	return nil
}
func (a *minMaxAcc) result() value.Value {
	if !a.has {
		return value.Null
	}
	return a.best
}

// merge folds another partial's best into this one through the same comparison
// add uses, so the combined result is the global min or max. A partial that saw
// no value contributes nothing.
func (a *minMaxAcc) merge(other accumulator) {
	o := other.(*minMaxAcc)
	if o.has {
		_ = a.add(o.best)
	}
}

// collectAcc gathers the non-null values into a list (doc 09 §8.1).
type collectAcc struct{ items []value.Value }

func (a *collectAcc) add(v value.Value) error {
	if !v.IsNull() {
		a.items = append(a.items, v)
	}
	return nil
}
func (a *collectAcc) result() value.Value { return value.List(a.items...) }

// distinctAcc passes only the first occurrence of each distinct non-null value to
// its inner accumulator, by the same canonical encoding used for grouping.
type distinctAcc struct {
	inner accumulator
	seen  map[string]bool
}

func (a *distinctAcc) add(v value.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	k := valuesKey([]value.Value{v})
	if a.seen[k] {
		return nil
	}
	a.seen[k] = true
	return a.inner.add(v)
}
func (a *distinctAcc) result() value.Value { return a.inner.result() }
