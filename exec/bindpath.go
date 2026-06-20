package exec

import (
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// bindPathOp materializes a named path variable (MATCH p = ...): for each input
// row it assembles the path value from the pattern's already-bound element
// variables in traversal order (node, rel, node, ...) and binds it to the path
// variable (doc 09 §3.4). The elements are node and relationship handles the
// scan and expand operators below already placed in the row.
type bindPathOp struct {
	spec  *plan.BindPath
	input operator
}

func (o *bindPathOp) open(ctx *Ctx) error { return o.input.open(ctx) }

func (o *bindPathOp) next() (eval.Row, bool, error) {
	row, ok, err := o.input.next()
	if err != nil || !ok {
		return nil, false, err
	}
	elems := make([]value.Value, len(o.spec.Elems))
	for i, name := range o.spec.Elems {
		elems[i] = row[name]
	}
	row[o.spec.Var] = value.Path(elems...)
	return row, true, nil
}

func (o *bindPathOp) close() error { return o.input.close() }
