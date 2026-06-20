package exec

import (
	"fmt"
	"sort"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// sortOp orders its input by the ORDER BY keys (doc 12 §7.4). It is a pipeline
// breaker: it buffers the full input, then emits in sorted order. Keys compare
// under the total order ([eval.Order], where null sorts last); a DESC key
// reverses that key's contribution. The sort is stable, so rows equal on all keys
// keep their input order.
type sortOp struct {
	keys  []plan.SortKey
	input operator
	ctx   *Ctx

	rows []eval.Row
	pos  int
	done bool
}

func (o *sortOp) open(ctx *Ctx) error {
	o.ctx, o.rows, o.pos, o.done = ctx, nil, 0, false
	return o.input.open(ctx)
}

func (o *sortOp) next() (eval.Row, bool, error) {
	if !o.done {
		if err := o.materialize(); err != nil {
			return nil, false, err
		}
		o.done = true
	}
	if o.pos >= len(o.rows) {
		return nil, false, nil
	}
	row := o.rows[o.pos]
	o.pos++
	return row, true, nil
}

// materialize drains the input and sorts it. Each key expression is evaluated
// once per row up front so the comparator does no evaluation (and so an
// evaluation error surfaces here, not mid-sort).
func (o *sortOp) materialize() error {
	type keyed struct {
		row  eval.Row
		keys []value.Value
	}
	var buf []keyed
	for {
		row, ok, err := o.input.next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		ks := make([]value.Value, len(o.keys))
		env := o.ctx.env(row)
		for i, k := range o.keys {
			v, err := eval.Eval(k.Expr, env)
			if err != nil {
				return err
			}
			ks[i] = v
		}
		buf = append(buf, keyed{row: row, keys: ks})
	}
	sort.SliceStable(buf, func(i, j int) bool {
		for ki, k := range o.keys {
			c := eval.Order(buf[i].keys[ki], buf[j].keys[ki])
			if k.Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	o.rows = make([]eval.Row, len(buf))
	for i, b := range buf {
		o.rows[i] = b.row
	}
	return nil
}

func (o *sortOp) close() error { return o.input.close() }

// skipOp discards the first N input rows. N is a constant expression evaluated
// once against the parameters (it cannot reference a row variable).
type skipOp struct {
	n     ast.Expr
	input operator
	ctx   *Ctx

	left    int64
	started bool
}

func (o *skipOp) open(ctx *Ctx) error {
	o.ctx, o.started = ctx, false
	return o.input.open(ctx)
}

func (o *skipOp) next() (eval.Row, bool, error) {
	if !o.started {
		n, err := constInt(o.n, o.ctx, "SKIP")
		if err != nil {
			return nil, false, err
		}
		o.left = n
		o.started = true
	}
	for o.left > 0 {
		_, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		o.left--
	}
	return o.input.next()
}

func (o *skipOp) close() error { return o.input.close() }

// limitOp caps the row count at N, stopping early once the cap is reached (doc 12
// §7.4, the limit's early-stop). N is a constant expression.
type limitOp struct {
	n     ast.Expr
	input operator
	ctx   *Ctx

	left    int64
	started bool
}

func (o *limitOp) open(ctx *Ctx) error {
	o.ctx, o.started = ctx, false
	return o.input.open(ctx)
}

func (o *limitOp) next() (eval.Row, bool, error) {
	if !o.started {
		n, err := constInt(o.n, o.ctx, "LIMIT")
		if err != nil {
			return nil, false, err
		}
		o.left = n
		o.started = true
	}
	if o.left <= 0 {
		return nil, false, nil
	}
	row, ok, err := o.input.next()
	if err != nil || !ok {
		return nil, false, err
	}
	o.left--
	return row, true, nil
}

func (o *limitOp) close() error { return o.input.close() }

// constInt evaluates a SKIP/LIMIT expression to a non-negative integer. The
// expression is constant (it has no row context); a null, non-integer, or
// negative value is an error.
func constInt(e ast.Expr, ctx *Ctx, what string) (int64, error) {
	v, err := eval.Eval(e, ctx.env(nil))
	if err != nil {
		return 0, err
	}
	n, ok := v.AsInt()
	if !ok {
		return 0, fmt.Errorf("exec: %s requires an integer, got %s", what, v.Type())
	}
	if n < 0 {
		return 0, fmt.Errorf("exec: %s must not be negative, got %d", what, n)
	}
	return n, nil
}
