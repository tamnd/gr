package exec

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
)

// filterOp keeps an input row only when its predicate is definitely true under
// three-valued logic (doc 02 §6.2): a null or false result drops the row.
type filterOp struct {
	pred  ast.Expr
	input operator
	ctx   *Ctx
}

func (o *filterOp) open(ctx *Ctx) error { o.ctx = ctx; return o.input.open(ctx) }

func (o *filterOp) next() (eval.Row, bool, error) {
	for {
		row, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		v, err := eval.Eval(o.pred, o.ctx.env(row))
		if err != nil {
			return nil, false, err
		}
		if b, ok := v.AsBool(); ok && b {
			return row, true, nil
		}
	}
}

func (o *filterOp) close() error { return o.input.close() }

// projectOp computes each output column from its expression against the input row
// (doc 12 §7.2): the late-materialized property reads and scalar functions
// evaluate here. With Distinct it deduplicates the projected rows by Cypher
// equality over all columns.
type projectOp struct {
	spec  *plan.Project
	input operator
	ctx   *Ctx

	cols []string
	seen map[string]bool // distinct dedup set
}

func (o *projectOp) open(ctx *Ctx) error {
	o.ctx = ctx
	o.cols = colNames(o.spec.Cols)
	if o.spec.Distinct {
		o.seen = make(map[string]bool)
	}
	return o.input.open(ctx)
}

func (o *projectOp) next() (eval.Row, bool, error) {
	for {
		in, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		env := o.ctx.env(in)
		out := make(eval.Row, len(o.spec.Cols))
		for _, c := range o.spec.Cols {
			v, err := eval.Eval(c.Expr, env)
			if err != nil {
				return nil, false, err
			}
			out[c.Name] = v
		}
		if o.seen != nil {
			k := rowKey(out, o.cols)
			if o.seen[k] {
				continue
			}
			o.seen[k] = true
		}
		return out, true, nil
	}
}

func (o *projectOp) close() error { return o.input.close() }
