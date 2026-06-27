package exec

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// aggregateOp groups its input by the non-aggregating columns and computes the
// aggregating ones (doc 09 §8, doc 12 §7.3). It is a pipeline breaker: it drains
// the whole input, builds one accumulator per aggregate per group, then emits one
// row per group. An aggregate may be nested in a larger expression (sum(x) * 2),
// so each maximal aggregate call is computed into a synthetic binding and the
// surrounding expression is evaluated against a representative row of the group
// extended with those bindings.
type aggregateOp struct {
	spec      *plan.Aggregate
	input     operator
	inputPlan plan.Op // the logical input, recompiled per worker on the parallel path
	ctx       *Ctx

	calls   []*ast.FunctionCall // the maximal aggregate calls, in discovery order
	aggExpr []ast.Expr          // each Aggs col rewritten with synthetic bindings
	out     []eval.Row
	pos     int
	done    bool
}

// aggNames is the read-path aggregate function set. count/sum/avg/min/max/collect
// are implemented in M2; the statistical aggregates are recognized (so they group
// rather than mis-evaluate as scalars) but not yet computed.
var aggNames = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
	"collect": true, "stdev": true, "stdevp": true,
	"percentilecont": true, "percentiledisc": true,
}

func (o *aggregateOp) open(ctx *Ctx) error {
	if err := o.prepare(ctx); err != nil {
		return err
	}
	return o.input.open(ctx)
}

// prepare wires the aggregate's run-time state: it rewrites each output column,
// hoisting every maximal aggregate call into a synthetic binding (filling o.calls),
// and rejects the statistical aggregates that are recognized but not yet computed.
// It is the part of open that does not touch the input operator, so the fused
// scan-aggregate path (scanAggregateOp) can reuse it without an input to open.
func (o *aggregateOp) prepare(ctx *Ctx) error {
	o.ctx, o.out, o.pos, o.done = ctx, nil, 0, false
	o.calls = nil
	idx := map[*ast.FunctionCall]string{}
	o.aggExpr = make([]ast.Expr, len(o.spec.Aggs))
	for i, c := range o.spec.Aggs {
		o.aggExpr[i] = o.rewrite(c.Expr, idx)
	}
	for _, call := range o.calls {
		switch strings.ToLower(call.Name) {
		case "stdev", "stdevp", "percentilecont", "percentiledisc":
			return fmt.Errorf("exec: aggregate %s is not yet supported", call.Name)
		}
	}
	return nil
}

// group is one group's running state: a representative input row (so non-aggregate
// sub-expressions, which are grouping keys and thus constant across the group,
// evaluate correctly) and one accumulator per aggregate call.
type group struct {
	rep  eval.Row
	accs []accumulator
}

func (o *aggregateOp) next() (eval.Row, bool, error) {
	if !o.done {
		if err := o.run(); err != nil {
			return nil, false, err
		}
		o.done = true
	}
	if o.pos >= len(o.out) {
		return nil, false, nil
	}
	row := o.out[o.pos]
	o.pos++
	return row, true, nil
}

// run drains the input and builds the output rows. An aggregation over a
// scan-rooted read pipeline of order-independent aggregates runs in parallel across
// morsels (grouped or not); everything else runs the serial path. The two produce
// identical rows in the same order (the parallel aggregates are exactly associative
// and the merge replays morsels in scan order, so group first-seen order matches).
func (o *aggregateOp) run() error {
	if ns := o.parallelScan(); ns != nil {
		return o.runParallel(ns)
	}
	return o.runSerial()
}

// parallelScan returns the NodeScan to parallelize over when this aggregation is
// eligible for the morsel-driven path, or nil to run serially. Eligibility:
// read-only, at least one aggregate and every one of them order-independent and not
// DISTINCT, more than one core available, and an input pipeline that is a
// row-independent chain rooted at a node scan. Grouping keys are allowed: the merge
// reconstructs the serial first-seen group order from the disjoint morsels.
func (o *aggregateOp) parallelScan() *plan.NodeScan {
	if o.ctx.Effects != nil || o.inputPlan == nil {
		return nil
	}
	if len(o.calls) == 0 || runtime.GOMAXPROCS(0) < 2 {
		return nil
	}
	for _, c := range o.calls {
		if c.Distinct || !parallelSafeAgg(c.Name) {
			return nil
		}
	}
	return parallelLeaf(o.inputPlan)
}

func (o *aggregateOp) runSerial() error {
	groups, order, err := o.drainGroups(o.input)
	if err != nil {
		return err
	}
	// With no grouping keys and no input, an aggregation still yields one row (the
	// empty group: count is 0, the rest null).
	if len(order) == 0 && len(o.spec.GroupKeys) == 0 {
		g := &group{rep: eval.Row{}, accs: o.newAccs()}
		groups[""] = g
		order = append(order, "")
	}
	return o.emit(groups, order)
}

// drainGroups consumes op to exhaustion, bucketing each row by its grouping-key
// value into one group (a representative row plus one accumulator per aggregate) and
// recording the keys in first-seen order. It is the shared core of the serial path
// and of each parallel worker's per-morsel pass: it touches only freshly allocated
// state and the read-only snapshot, so many workers run it concurrently over private
// pipelines that share one engine transaction.
func (o *aggregateOp) drainGroups(op operator) (map[string]*group, []string, error) {
	groups := map[string]*group{}
	var order []string
	keyExprs := make([]ast.Expr, len(o.spec.GroupKeys))
	for i, c := range o.spec.GroupKeys {
		keyExprs[i] = c.Expr
	}
	// One environment reused across every input row: the group representative is
	// kept separately (g.rep), and Eval never retains the Env, so only its Row
	// field changes per row.
	env := o.ctx.env(nil)
	for {
		in, ok, err := op.next()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			break
		}
		env.Row = in
		keyVals := make([]value.Value, len(keyExprs))
		for i, e := range keyExprs {
			v, err := eval.Eval(e, env)
			if err != nil {
				return nil, nil, err
			}
			keyVals[i] = v
		}
		key := valuesKey(keyVals)
		g := groups[key]
		if g == nil {
			g = &group{rep: in, accs: o.newAccs()}
			groups[key] = g
			order = append(order, key)
		}
		for j, call := range o.calls {
			if err := o.feed(g.accs[j], call, env); err != nil {
				return nil, nil, err
			}
		}
	}
	return groups, order, nil
}

func (o *aggregateOp) emit(groups map[string]*group, order []string) error {
	var seen map[string]bool
	cols := Columns(o.spec)
	if o.spec.Distinct {
		seen = map[string]bool{}
	}
	for _, key := range order {
		g := groups[key]
		row := make(eval.Row, len(o.spec.GroupKeys)+len(o.spec.Aggs))
		repEnv := o.ctx.env(g.rep)
		for _, c := range o.spec.GroupKeys {
			v, err := eval.Eval(c.Expr, repEnv)
			if err != nil {
				return err
			}
			row[c.Name] = v
		}
		// Bind each aggregate result, then evaluate the (possibly compound) column
		// expression against the representative row extended with those bindings.
		aggRow := cloneRow(g.rep)
		for j := range o.calls {
			aggRow[aggSlot(j)] = g.accs[j].result()
		}
		aggEnv := o.ctx.env(aggRow)
		for i, c := range o.spec.Aggs {
			v, err := eval.Eval(o.aggExpr[i], aggEnv)
			if err != nil {
				return err
			}
			row[c.Name] = v
		}
		if seen != nil {
			k := rowKey(row, cols)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		o.out = append(o.out, row)
	}
	return nil
}

func (o *aggregateOp) newAccs() []accumulator {
	accs := make([]accumulator, len(o.calls))
	for i, call := range o.calls {
		accs[i] = newAccumulator(call)
	}
	return accs
}

// feed evaluates an aggregate call's argument against the current row and adds it
// to the accumulator. count(*) has no argument and counts every row.
func (o *aggregateOp) feed(acc accumulator, call *ast.FunctionCall, env *eval.Env) error {
	if call.Star {
		return acc.add(value.Bool(true)) // count(*) counts presence; the value is ignored
	}
	if len(call.Args) == 0 {
		return fmt.Errorf("exec: %s requires an argument", call.Name)
	}
	v, err := eval.Eval(call.Args[0], env)
	if err != nil {
		return err
	}
	return acc.add(v)
}

func (o *aggregateOp) close() error { return o.input.close() }

// rewrite replaces each maximal aggregate call in an expression with a synthetic
// variable bound to that call's computed result, recording the call so it gets an
// accumulator. Non-aggregate nodes are rebuilt with rewritten children; unchanged
// subtrees are shared (evaluation is read-only).
func (o *aggregateOp) rewrite(e ast.Expr, idx map[*ast.FunctionCall]string) ast.Expr {
	switch x := e.(type) {
	case *ast.FunctionCall:
		if aggNames[strings.ToLower(x.Name)] {
			name, ok := idx[x]
			if !ok {
				name = aggSlot(len(o.calls))
				idx[x] = name
				o.calls = append(o.calls, x)
			}
			return &ast.Variable{Name: name}
		}
		args := make([]ast.Expr, len(x.Args))
		for i, a := range x.Args {
			args[i] = o.rewrite(a, idx)
		}
		return &ast.FunctionCall{Name: x.Name, Distinct: x.Distinct, Args: args, Star: x.Star}
	case *ast.Unary:
		return &ast.Unary{Op: x.Op, X: o.rewrite(x.X, idx)}
	case *ast.Binary:
		return &ast.Binary{Op: x.Op, L: o.rewrite(x.L, idx), R: o.rewrite(x.R, idx)}
	case *ast.IsNull:
		return &ast.IsNull{X: o.rewrite(x.X, idx), Negate: x.Negate}
	case *ast.Property:
		return &ast.Property{Base: o.rewrite(x.Base, idx), Key: x.Key}
	case *ast.Index:
		return &ast.Index{Base: o.rewrite(x.Base, idx), Index: o.rewrite(x.Index, idx)}
	case *ast.Slice:
		return &ast.Slice{Base: o.rewrite(x.Base, idx), Lo: o.rewriteOpt(x.Lo, idx), Hi: o.rewriteOpt(x.Hi, idx)}
	case *ast.ListLit:
		elems := make([]ast.Expr, len(x.Elems))
		for i, el := range x.Elems {
			elems[i] = o.rewrite(el, idx)
		}
		return &ast.ListLit{Elems: elems}
	case *ast.MapLit:
		entries := make([]ast.PropEntry, len(x.Entries))
		for i, en := range x.Entries {
			entries[i] = ast.PropEntry{Key: en.Key, Value: o.rewrite(en.Value, idx)}
		}
		return &ast.MapLit{Entries: entries}
	case *ast.Case:
		c := &ast.Case{Subject: o.rewriteOpt(x.Subject, idx), Else: o.rewriteOpt(x.Else, idx)}
		c.Whens = make([]ast.WhenThen, len(x.Whens))
		for i, w := range x.Whens {
			c.Whens[i] = ast.WhenThen{When: o.rewrite(w.When, idx), Then: o.rewrite(w.Then, idx)}
		}
		return c
	default:
		return e // Literal, Param, Variable: no aggregate inside
	}
}

func (o *aggregateOp) rewriteOpt(e ast.Expr, idx map[*ast.FunctionCall]string) ast.Expr {
	if e == nil {
		return nil
	}
	return o.rewrite(e, idx)
}

// aggSlot names the synthetic binding for the n-th aggregate call. The '@' prefix
// cannot appear in a user identifier, so it never collides with a real variable.
func aggSlot(n int) string { return "@agg" + itoa(n) }

// itoa formats a non-negative int without importing strconv for this one use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
