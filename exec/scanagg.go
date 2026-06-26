package exec

import (
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// scanAggregateOp fuses a grouping-free aggregation directly onto its node scan,
// so it computes the aggregate without ever materializing a row per node. The
// general aggregateOp pulls one eval.Row map from the scan per node and builds one
// eval.Env per node to evaluate each aggregate's argument; over a full-label scan
// that is several heap allocations per node, and the resulting garbage dominates a
// cheap aggregate's latency (the micro-scan gap, spec 2060 doc 13 §4). The fused
// operator walks the scan itself and reuses a single row and a single env across
// every node, binding the scanned node into them in place. The aggregate values it
// pulls out are copied into the accumulators, which never retain the row, so the
// reuse changes no answer.
//
// It runs only for the shapes the compiler proves safe: a grouping-free aggregation
// whose input is a bare node scan and whose aggregates are not all order-independent
// (an all count/min/max aggregation keeps the existing morsel-parallel path, which
// wins on a large scan; this serial fused path is for the avg/sum/collect/distinct
// aggregates that already ran serially). It is a pipeline breaker like aggregateOp:
// it drains the scan on the first pull and emits exactly one row.
type scanAggregateOp struct {
	aggregateOp // for spec, ctx, calls, aggExpr, rewrite, feed, newAccs

	scan *plan.NodeScan

	scanTok engine.Token   // the primary label to scan, or 0 for all nodes
	rest    []engine.Token // residual labels the node must also carry
	none    bool           // an unknown label: the scan is empty

	accs    []accumulator
	row     eval.Row  // reused single-node row, bound to scan.Var each step
	env     *eval.Env // reused environment over row
	emitted bool
}

func (o *scanAggregateOp) open(ctx *Ctx) error {
	if err := o.prepare(ctx); err != nil {
		return err
	}
	o.scanTok = engine.Token(0)
	o.rest = o.rest[:0]
	o.none = false
	for i, l := range o.scan.Labels {
		if !l.Known {
			o.none = true
			break
		}
		if i == 0 {
			o.scanTok = l.Token
		} else {
			o.rest = append(o.rest, l.Token)
		}
	}
	o.accs = o.newAccs()
	o.row = eval.Row{}
	o.env = ctx.env(o.row)
	o.emitted = false
	return nil
}

func (o *scanAggregateOp) next() (eval.Row, bool, error) {
	if o.emitted {
		return nil, false, nil
	}
	if err := o.drain(); err != nil {
		return nil, false, err
	}
	o.emitted = true
	return o.emitRow()
}

// drain walks the scan once, feeding every surviving node into the accumulators
// through the reused row and env. An unknown primary label makes the scan empty,
// which leaves the accumulators at their zero state (count 0, the rest null), the
// same one-row-of-empties result the general path produces for no input.
func (o *scanAggregateOp) drain() error {
	if o.none {
		return nil
	}
	return o.ctx.Tx.ScanLabel(o.scanTok, func(id engine.NodeID) error {
		o.ctx.countScan(1)
		for _, t := range o.rest {
			has, err := o.ctx.Tx.HasLabel(id, t)
			if err != nil {
				return err
			}
			if !has {
				return nil
			}
		}
		o.row[o.scan.Var] = value.Node(uint64(id))
		for j, call := range o.calls {
			if err := o.feed(o.accs[j], call, o.env); err != nil {
				return err
			}
		}
		return nil
	})
}

// emitRow builds the single output row from the drained accumulators. Each
// aggregate result is bound to its synthetic slot, then the (possibly compound)
// output column expression is evaluated against those bindings. A grouping-free
// aggregation's column expressions reference only the aggregate slots, literals,
// and parameters, never the scanned variable directly, so no representative row is
// needed.
func (o *scanAggregateOp) emitRow() (eval.Row, bool, error) {
	aggRow := make(eval.Row, len(o.calls))
	for j := range o.calls {
		aggRow[aggSlot(j)] = o.accs[j].result()
	}
	aggEnv := o.ctx.env(aggRow)
	row := make(eval.Row, len(o.spec.Aggs))
	for i, c := range o.spec.Aggs {
		v, err := eval.Eval(o.aggExpr[i], aggEnv)
		if err != nil {
			return nil, false, err
		}
		row[c.Name] = v
	}
	return row, true, nil
}

func (o *scanAggregateOp) close() error { return nil }

// fuseScanAggregate reports the node scan to fuse an aggregation onto, or nil when
// the aggregation is not a fusible shape. Fusible means: grouping-free (a GROUP BY
// keeps a representative row per group, which the fused path does not carry), its
// input is a bare node scan (no operator in between to apply per row), and at least
// one aggregate is not order-independent. The last clause leaves a pure
// count/min/max aggregation on aggregateOp, whose morsel-parallel path outruns this
// serial scan on a large graph; the fused path targets the avg/sum/collect/distinct
// aggregates that run serially anyway, where removing the per-node garbage is the
// whole win.
func fuseScanAggregate(x *plan.Aggregate) *plan.NodeScan {
	if len(x.GroupKeys) == 0 && x.Input != nil {
		if ns, ok := x.Input.(*plan.NodeScan); ok && hasSerialAgg(x) {
			return ns
		}
	}
	return nil
}

// walkAggCalls visits every maximal aggregate function call in an expression,
// reporting its name and whether it is DISTINCT. It stops at an aggregate call (an
// aggregate cannot nest inside another), and otherwise descends into the same
// expression shapes aggregateOp.rewrite descends, so the two agree on what counts
// as an aggregate. It is used only to classify an aggregation's shape, never on the
// row hot path.
func walkAggCalls(e ast.Expr, fn func(name string, distinct bool)) {
	switch x := e.(type) {
	case *ast.FunctionCall:
		if aggNames[strings.ToLower(x.Name)] {
			fn(x.Name, x.Distinct)
			return
		}
		for _, a := range x.Args {
			walkAggCalls(a, fn)
		}
	case *ast.Unary:
		walkAggCalls(x.X, fn)
	case *ast.Binary:
		walkAggCalls(x.L, fn)
		walkAggCalls(x.R, fn)
	case *ast.IsNull:
		walkAggCalls(x.X, fn)
	case *ast.Property:
		walkAggCalls(x.Base, fn)
	case *ast.Index:
		walkAggCalls(x.Base, fn)
		walkAggCalls(x.Index, fn)
	case *ast.Slice:
		walkAggCalls(x.Base, fn)
		walkAggCallsOpt(x.Lo, fn)
		walkAggCallsOpt(x.Hi, fn)
	case *ast.ListLit:
		for _, el := range x.Elems {
			walkAggCalls(el, fn)
		}
	case *ast.MapLit:
		for _, en := range x.Entries {
			walkAggCalls(en.Value, fn)
		}
	case *ast.Case:
		walkAggCallsOpt(x.Subject, fn)
		walkAggCallsOpt(x.Else, fn)
		for _, w := range x.Whens {
			walkAggCalls(w.When, fn)
			walkAggCalls(w.Then, fn)
		}
	}
}

func walkAggCallsOpt(e ast.Expr, fn func(name string, distinct bool)) {
	if e != nil {
		walkAggCalls(e, fn)
	}
}

// hasSerialAgg reports whether any of the aggregation's output columns carries an
// aggregate the morsel-parallel path does not handle, so the aggregation would run
// serially and benefits from fusion. A column with no aggregate call at all (a bare
// grouping-free projection) does not qualify on its own.
func hasSerialAgg(x *plan.Aggregate) bool {
	for _, c := range x.Aggs {
		found := false
		serial := false
		walkAggCalls(c.Expr, func(name string, distinct bool) {
			found = true
			if distinct || !parallelSafeAgg(name) {
				serial = true
			}
		})
		if found && serial {
			return true
		}
	}
	return false
}
