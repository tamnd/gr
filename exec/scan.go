package exec

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// unitOp is the single-empty-row source: it yields exactly one empty row, the
// input a projection with no leading reading clause (RETURN 1) computes against.
type unitOp struct{ done bool }

func (o *unitOp) open(*Ctx) error { o.done = false; return nil }

func (o *unitOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	return eval.Row{}, true, nil
}

func (o *unitOp) close() error { return nil }

// argumentOp is the correlated-input leaf: it yields one row carrying the outer
// variables an Optional supplies. The Optional sets bound before each reopen; with
// no binding it yields a single empty row (so an inner subplan still runs once).
type argumentOp struct {
	vars  []string
	bound eval.Row
	done  bool
}

func (o *argumentOp) open(*Ctx) error { o.done = false; return nil }

func (o *argumentOp) next() (eval.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	row := eval.Row{}
	for k, v := range o.bound {
		row[k] = v
	}
	return row, true, nil
}

func (o *argumentOp) close() error { return nil }

// nodeScanOp produces the nodes carrying a label set. It scans by the first known
// label (or all nodes when unlabeled) through the engine SPI, then filters the
// reached node by any remaining labels. An unknown label (the catalog never
// interned it) matches no node, so the scan yields nothing.
type nodeScanOp struct {
	spec *plan.NodeScan
	ctx  *Ctx

	buf  []engine.NodeID
	pos  int
	rest []engine.Token // additional labels the node must also carry
	none bool           // an unknown label: the scan is empty

	// In windowed mode the scan is a morsel consumer for parallel aggregation: it
	// walks a fixed [winLo,winHi) window of an already-scanned shared node id slice
	// once, applying the residual labels per row, instead of running its own label
	// scan. A worker sets winIDs once and winLo/winHi per morsel, re-opening the
	// pipeline between morsels. The serial path leaves windowed false and uses buf.
	windowed bool
	winIDs   []engine.NodeID
	winLo    int
	winHi    int
}

func (o *nodeScanOp) open(ctx *Ctx) error {
	o.ctx, o.buf, o.pos, o.rest, o.none = ctx, nil, 0, nil, false
	scanTok := engine.Token(0)
	for i, l := range o.spec.Labels {
		if !l.Known {
			o.none = true
			return nil
		}
		if i == 0 {
			scanTok = l.Token
		} else {
			o.rest = append(o.rest, l.Token)
		}
	}
	if o.windowed {
		// The shared slice already holds the primary-label scan; this pass only
		// walks its assigned [winLo,winHi) window, filtering by the residual labels.
		o.pos = o.winLo
		return nil
	}
	return ctx.Tx.ScanLabel(scanTok, func(id engine.NodeID) error {
		ctx.countScan(1)
		o.buf = append(o.buf, id)
		return nil
	})
}

func (o *nodeScanOp) next() (eval.Row, bool, error) {
	if o.none {
		return nil, false, nil
	}
	if o.windowed {
		return o.nextWindow()
	}
	for o.pos < len(o.buf) {
		id := o.buf[o.pos]
		o.pos++
		ok, err := o.hasAll(id)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return eval.Row{o.spec.Var: value.Node(uint64(id))}, true, nil
		}
	}
	return nil, false, nil
}

// nextWindow produces the scan's rows from its assigned morsel: it walks the fixed
// [winLo,winHi) window of the shared id slice once, applying the residual labels,
// and stops when the window is spent. The worker re-opens the pipeline (resetting
// pos to winLo) for each morsel it takes, so one pass covers exactly one morsel.
func (o *nodeScanOp) nextWindow() (eval.Row, bool, error) {
	for o.pos < o.winHi {
		id := o.winIDs[o.pos]
		o.pos++
		ok, err := o.hasAll(id)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return eval.Row{o.spec.Var: value.Node(uint64(id))}, true, nil
		}
	}
	return nil, false, nil
}

func (o *nodeScanOp) hasAll(id engine.NodeID) (bool, error) {
	for _, t := range o.rest {
		has, err := o.ctx.Tx.HasLabel(id, t)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

func (o *nodeScanOp) close() error { return nil }

// expandOp traverses one relationship from each input row's source node to its
// neighbors, producing a row per surviving edge (doc 12 §4.1). It enforces the
// type, target-label, expand-into, and relationship-uniqueness constraints, and
// binds the relationship and (unless already bound) the reached node.
type expandOp struct {
	spec  *plan.Expand
	input operator
	peers []string // sibling relationship variables this edge must differ from
	ctx   *Ctx

	cur   eval.Row // the input row being expanded
	queue []engine.Neighbor
	qpos  int

	relTok engine.Token          // the single type token to expand, or 0 for all
	allow  map[engine.Token]bool // post-filter type set when more than one type
	noType bool                  // every named type is unknown: no edge matches
}

func (o *expandOp) open(ctx *Ctx) error {
	o.ctx, o.cur, o.queue, o.qpos = ctx, nil, nil, 0
	o.relTok, o.allow, o.noType = resolveTypes(o.spec.Types)
	return o.input.open(ctx)
}

// resolveTypes turns the resolved type set into an expand token and an optional
// post-filter: no types means expand all (token 0); one known type expands that
// token directly; several known types expand all and filter to the known set; an
// all-unknown set matches nothing.
func resolveTypes(types []bind.NameRef) (tok engine.Token, allow map[engine.Token]bool, none bool) {
	known := make([]engine.Token, 0, len(types))
	for _, t := range types {
		if t.Known {
			known = append(known, t.Token)
		}
	}
	switch {
	case len(types) == 0:
		return 0, nil, false
	case len(known) == 0:
		return 0, nil, true
	case len(known) == 1:
		return known[0], nil, false
	default:
		allow = make(map[engine.Token]bool, len(known))
		for _, t := range known {
			allow[t] = true
		}
		return 0, allow, false
	}
}

func (o *expandOp) next() (eval.Row, bool, error) {
	for {
		// Drain the queued neighbors of the current input row.
		for o.qpos < len(o.queue) {
			nb := o.queue[o.qpos]
			o.qpos++
			row, ok, err := o.accept(nb)
			if err != nil {
				return nil, false, err
			}
			if ok {
				return row, true, nil
			}
		}
		// Pull the next input row and load its adjacency.
		in, ok, err := o.input.next()
		if err != nil || !ok {
			return nil, false, err
		}
		if o.noType {
			continue
		}
		src, ok := in[o.spec.From].AsNode()
		if !ok {
			// A null source (an unmatched OPTIONAL variable) expands to nothing.
			continue
		}
		o.cur, o.queue, o.qpos = in, o.queue[:0], 0
		dir := toEngineDir(o.spec.Dir)
		err = o.ctx.Tx.Expand(engine.NodeID(src), o.relTok, dir, func(nb engine.Neighbor) error {
			o.ctx.countScan(1)
			o.queue = append(o.queue, nb)
			return nil
		})
		if err != nil {
			return nil, false, err
		}
	}
}

// accept applies the per-edge constraints and, when the edge survives, builds the
// extended row. It returns ok false for an edge filtered out by type, target
// label, expand-into, or relationship-uniqueness.
func (o *expandOp) accept(nb engine.Neighbor) (eval.Row, bool, error) {
	if o.allow != nil && !o.allow[nb.Type] {
		return nil, false, nil
	}
	if !o.unique(nb.Rel) {
		return nil, false, nil
	}
	if o.spec.ToBound {
		want, ok := o.cur[o.spec.To].AsNode()
		if !ok || engine.NodeID(want) != nb.Node {
			return nil, false, nil
		}
	}
	ok, err := o.hasToLabels(nb.Node)
	if err != nil || !ok {
		return nil, false, err
	}
	row := cloneRow(o.cur)
	row[o.spec.Rel] = value.Rel(uint64(nb.Rel))
	if !o.spec.ToBound {
		row[o.spec.To] = value.Node(uint64(nb.Node))
	}
	return row, true, nil
}

// unique enforces relationship-uniqueness: the new edge must not already be bound
// to a sibling relationship variable in the same pattern (doc 02 §4.3).
func (o *expandOp) unique(rel engine.RelID) bool {
	for _, p := range o.peers {
		if v, ok := o.cur[p]; ok && relValueContains(v, rel) {
			return false
		}
	}
	return true
}

func (o *expandOp) hasToLabels(id engine.NodeID) (bool, error) {
	for _, l := range o.spec.ToLabels {
		if !l.Known {
			return false, nil
		}
		has, err := o.ctx.Tx.HasLabel(id, l.Token)
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

func (o *expandOp) close() error { return o.input.close() }

func toEngineDir(d ast.Direction) engine.Direction {
	switch d {
	case ast.DirOut:
		return engine.Outgoing
	case ast.DirIn:
		return engine.Incoming
	default:
		return engine.Both
	}
}
