package exec

import (
	"fmt"
	"time"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// unwindOp expands a list expression to one row per element, binding each to the
// variable (doc 09 §5). A null list yields no rows; a non-list, non-null value is
// an error. A leading UNWIND has no input and runs over a single empty row.
type unwindOp struct {
	expr    ast.Expr
	varName string
	input   operator
	ctx     *Ctx

	cur    eval.Row
	items  []value.Value
	pos    int
	primed bool // for a leading UNWIND, whether the one empty row was consumed
}

func (o *unwindOp) open(ctx *Ctx) error {
	o.ctx, o.cur, o.items, o.pos, o.primed = ctx, nil, nil, 0, false
	if o.input != nil {
		return o.input.open(ctx)
	}
	return nil
}

func (o *unwindOp) next() (eval.Row, bool, error) {
	for {
		for o.pos < len(o.items) {
			el := o.items[o.pos]
			o.pos++
			row := cloneRow(o.cur)
			row[o.varName] = el
			return row, true, nil
		}
		in, ok, err := o.nextInput()
		if err != nil || !ok {
			return nil, false, err
		}
		v, err := eval.Eval(o.expr, o.ctx.env(in))
		if err != nil {
			return nil, false, err
		}
		if v.IsNull() {
			continue // UNWIND null yields no rows
		}
		lst, ok := v.AsList()
		if !ok {
			return nil, false, fmt.Errorf("exec: UNWIND requires a list, got %s", v.Type())
		}
		o.cur, o.items, o.pos = in, lst, 0
	}
}

// nextInput pulls the next driving row: from the input operator, or, for a leading
// UNWIND, exactly one empty row.
func (o *unwindOp) nextInput() (eval.Row, bool, error) {
	if o.input != nil {
		return o.input.next()
	}
	if o.primed {
		return nil, false, nil
	}
	o.primed = true
	return eval.Row{}, true, nil
}

func (o *unwindOp) close() error {
	if o.input != nil {
		return o.input.close()
	}
	return nil
}

// joinOp is a hash join: it builds a hash table over the right input keyed by the
// shared variables, then probes it with each left row (doc 12 §5.1). An empty key
// set is a cartesian product (every left row pairs with every right row), the form
// the builder emits for a disconnected pattern.
type joinOp struct {
	on    []string
	left  operator
	right operator
	ctx   *Ctx

	// rightPlan is the build side's logical plan, kept so build can construct the hash
	// table across cores from a private copy of the pipeline per worker (paralleljoin.go)
	// when no memory budget is set. It is nil for a join whose build side is not a plan
	// the executor can recompile (only the test harness builds one of those).
	rightPlan plan.Op

	table       map[string][]eval.Row
	built       bool
	rightOpened bool // whether the serial build opened o.right (so close knows to close it)

	// grace is non-nil once a keyed join's build side has grown past the memory
	// budget and the join has spilled its partitions to disk (spilljoin.go); next
	// then pulls from it instead of probing the in-memory table. blockBuild is the
	// counterpart for a keyless (cartesian) join, which cannot be partitioned by key:
	// the build side spills to one file that blockRdr streams once per probe row, a
	// block-nested-loop over disk.
	grace      *graceJoin
	blockBuild *spillPart
	blockRdr   *partReader

	cur     eval.Row
	matches []eval.Row
	mpos    int
}

func (o *joinOp) open(ctx *Ctx) error {
	o.ctx, o.table, o.built, o.grace = ctx, nil, false, nil
	ctx.countBinaryJoin()
	o.blockBuild, o.blockRdr = nil, nil
	o.rightOpened = false
	o.cur, o.matches, o.mpos = nil, nil, 0
	// The right (build) side is opened lazily by build, because the parallel build path
	// runs private copies of the build subplan rather than this operator and would
	// otherwise pay for a wasted serial scan here.
	return o.left.open(ctx)
}

// build reads the whole build (right) side. While the budget allows it the rows go
// into the in-memory hash table, the fast path. If the table grows past the budget
// and spilling is configured, the join spills to disk: a keyed join switches to a
// grace hash join (partition both inputs by key), and a keyless join (a cartesian
// product, which has no key to partition on) spills the build side to one file that
// next streams per probe row.
func (o *joinOp) build() error {
	bstart := time.Now()
	defer func() { o.ctx.countJoinBuild(time.Since(bstart)) }()
	// Parallel build path: with no memory budget the table lives in memory anyway, so
	// when the build side is a scan-rooted independent pipeline, construct it across
	// cores. The probe below is unchanged and serial, so the output and its order match
	// the serial build exactly. A budget forces the serial path so the join can spill.
	if !o.ctx.spillEnabled() && o.rightPlan != nil {
		table, ok, err := parallelBuildTable(o.ctx, o.rightPlan, o.on)
		if err != nil {
			return err
		}
		if ok {
			o.table = table
			o.built = true
			return nil
		}
	}
	if err := o.right.open(o.ctx); err != nil {
		return err
	}
	o.rightOpened = true
	o.table = map[string][]eval.Row{}
	var bytes int64
	canSpill := o.ctx.spillEnabled()
	for {
		row, ok, err := o.right.next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if canSpill && o.grace == nil && o.blockBuild == nil && bytes > o.ctx.MemBudget {
			if len(o.on) > 0 {
				if err := o.spillKeyed(); err != nil {
					return err
				}
			} else if err := o.spillKeyless(); err != nil {
				return err
			}
		}
		switch {
		case o.grace != nil:
			if err := o.grace.partitionRight(row); err != nil {
				return err
			}
		case o.blockBuild != nil:
			if err := o.blockBuild.write(row); err != nil {
				return err
			}
		default:
			k := rowKey(row, o.on)
			o.table[k] = append(o.table[k], row)
			bytes += rowSize(row)
		}
	}
	if o.grace != nil {
		for {
			left, ok, err := o.left.next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if err := o.grace.partitionLeft(left); err != nil {
				return err
			}
		}
		o.grace.start()
	}
	o.built = true
	return nil
}

// spillKeyed creates the grace join and moves the rows buffered so far into its
// partition files, after which build routes the remaining build-side rows there too.
func (o *joinOp) spillKeyed() error {
	g, err := newGraceJoin(o.ctx, o.on)
	if err != nil {
		return err
	}
	for _, rows := range o.table {
		for _, r := range rows {
			if err := g.partitionRight(r); err != nil {
				g.cleanup()
				return err
			}
		}
	}
	o.table = nil
	o.grace = g
	return nil
}

// spillKeyless opens one spill file for a cartesian join's build side and moves the
// rows buffered so far into it, after which build appends the rest there too.
func (o *joinOp) spillKeyless() error {
	part, err := newSpillPart(o.ctx)
	if err != nil {
		return err
	}
	for _, rows := range o.table {
		for _, r := range rows {
			if err := part.write(r); err != nil {
				_ = part.discardOnce()
				return err
			}
		}
	}
	o.table = nil
	o.blockBuild = part
	return nil
}

func (o *joinOp) next() (eval.Row, bool, error) {
	if !o.built {
		if err := o.build(); err != nil {
			return nil, false, err
		}
	}
	if o.grace != nil {
		return o.grace.next()
	}
	if o.blockBuild != nil {
		return o.nextBlock()
	}
	for {
		for o.mpos < len(o.matches) {
			r := o.matches[o.mpos]
			o.mpos++
			return mergeRows(o.cur, r), true, nil
		}
		left, ok, err := o.left.next()
		if err != nil || !ok {
			return nil, false, err
		}
		o.cur = left
		o.matches = o.table[rowKey(left, o.on)]
		o.mpos = 0
	}
}

// nextBlock is the block-nested-loop probe for a spilled cartesian join: for each
// left row it streams the whole spilled build side from disk, pairing every build
// row with the current left row.
func (o *joinOp) nextBlock() (eval.Row, bool, error) {
	for {
		if o.blockRdr != nil {
			row, ok, err := o.blockRdr.next()
			if err != nil {
				return nil, false, err
			}
			if ok {
				return mergeRows(o.cur, row), true, nil
			}
			o.blockRdr = nil
		}
		left, ok, err := o.left.next()
		if err != nil || !ok {
			return nil, false, err
		}
		o.cur = left
		o.blockRdr = &partReader{p: o.blockBuild}
	}
}

func (o *joinOp) close() error {
	if o.grace != nil {
		o.grace.cleanup()
	}
	if o.blockBuild != nil {
		_ = o.blockBuild.discardOnce()
	}
	err := o.left.close()
	if o.rightOpened {
		if rerr := o.right.close(); err == nil {
			err = rerr
		}
	}
	return err
}

// optionalOp is a left-outer join (OPTIONAL MATCH, doc 09 §4.2): every outer row
// survives, extended by each match its correlated inner subplan finds, and padded
// with nulls on the inner's new variables when it finds none. The inner is
// reopened once per outer row, with the outer row fed to its Argument leaves.
type optionalOp struct {
	input   operator
	inner   operator
	args    []*argumentOp
	newVars []string
	ctx     *Ctx

	cur       eval.Row
	matched   bool
	innerOpen bool
}

func (o *optionalOp) open(ctx *Ctx) error {
	o.ctx, o.cur, o.matched, o.innerOpen = ctx, nil, false, false
	return o.input.open(ctx)
}

func (o *optionalOp) next() (eval.Row, bool, error) {
	for {
		if o.innerOpen {
			row, ok, err := o.inner.next()
			if err != nil {
				return nil, false, err
			}
			if ok {
				o.matched = true
				return mergeRows(o.cur, row), true, nil
			}
			o.inner.close()
			o.innerOpen = false
			if !o.matched {
				return o.padNulls(o.cur), true, nil
			}
		}
		outer, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		o.cur, o.matched = outer, false
		for _, a := range o.args {
			a.bound = restrict(outer, a.vars)
		}
		if err := o.inner.open(o.ctx); err != nil {
			return nil, false, err
		}
		o.innerOpen = true
	}
}

// padNulls returns the outer row extended with a null for each variable the inner
// would have introduced, the unmatched OPTIONAL MATCH result.
func (o *optionalOp) padNulls(outer eval.Row) eval.Row {
	row := cloneRow(outer)
	for _, v := range o.newVars {
		if _, ok := row[v]; !ok {
			row[v] = value.Null
		}
	}
	return row
}

func (o *optionalOp) close() error {
	if o.innerOpen {
		o.inner.close()
		o.innerOpen = false
	}
	return o.input.close()
}

// unionOp concatenates two query results (doc 09 §10): UNION ALL keeps every row,
// plain UNION deduplicates across both arms by Cypher equality over all columns.
type unionOp struct {
	all   bool
	left  operator
	right operator
	ctx   *Ctx

	onRight bool
	seen    map[string]bool
}

func (o *unionOp) open(ctx *Ctx) error {
	o.ctx, o.onRight = ctx, false
	if !o.all {
		o.seen = map[string]bool{}
	}
	if err := o.left.open(ctx); err != nil {
		return err
	}
	return o.right.open(ctx)
}

func (o *unionOp) next() (eval.Row, bool, error) {
	for {
		var (
			row eval.Row
			ok  bool
			err error
		)
		if !o.onRight {
			row, ok, err = o.left.next()
			if err != nil {
				return nil, false, err
			}
			if !ok {
				o.onRight = true
				continue
			}
		} else {
			row, ok, err = o.right.next()
			if err != nil || !ok {
				return nil, false, err
			}
		}
		if o.seen != nil {
			k := rowKeyAll(row)
			if o.seen[k] {
				continue
			}
			o.seen[k] = true
		}
		return row, true, nil
	}
}

func (o *unionOp) close() error {
	err := o.left.close()
	if rerr := o.right.close(); err == nil {
		err = rerr
	}
	return err
}

// mergeRows overlays b onto a copy of a: a's bindings, then b's (b wins on the
// shared join keys, where the values are equal anyway).
func mergeRows(a, b eval.Row) eval.Row {
	out := make(eval.Row, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// restrict copies just the named variables out of a row, the correlated bindings
// an Argument carries into a subplan.
func restrict(row eval.Row, vars []string) eval.Row {
	out := make(eval.Row, len(vars))
	for _, v := range vars {
		if val, ok := row[v]; ok {
			out[v] = val
		}
	}
	return out
}
