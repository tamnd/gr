// Package exec is the read-path executor: it interprets a logical operator tree
// ([plan]) against a storage snapshot ([engine.Tx]), producing the query's result
// rows (spec 2060 doc 12; doc 25 §5.2 deliverable 6).
//
// M2 ships the executor in its naïve, correct form: a pull-based (Volcano)
// iterator, tuple-at-a-time, single-threaded. The operator interface is the seam
// doc 12 §2.1 names — an operator is opened (it sets up its state), it produces
// output on demand (next pulls one row, computed from its input), and it is
// closed. The seam is deliberately frozen here so M4 can replace the row with a
// vector batch and add morsel parallelism behind it without the planner or the
// surfaces noticing (doc 25 §3.8, the operator-interface seam). The per-value
// semantics live in [eval]; this package owns control flow, materialization, and
// the relational shape of each operator.
//
// The executor consumes the logical plan directly: in M2 the "physical plan" is
// essentially the logical plan, since the cost-based planner that rewrites it is
// M4. An Expand reads adjacency through the engine SPI (never the CSR arrays); a
// Filter keeps a row only when its predicate is definitely true under three-
// valued logic; an Aggregate, a Sort, a Join, and a set operation buffer as much
// as their relational meaning requires. Relationship-uniqueness (a pattern is
// isomorphic on its relationships, doc 02 §4.3) is enforced in Expand.
package exec

import (
	"fmt"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// Ctx is the execution-wide context shared by every operator: the snapshot the
// query reads, the supplied parameters, and the property-key resolver eval needs
// to turn a property name into a catalog token. It is read-only for the M2 read
// path and lives for the duration of one cursor.
type Ctx struct {
	Tx      engine.Tx
	Params  map[string]value.Value
	Resolve func(name string) (engine.Token, bool)
	// LabelName, RelTypeName, and PropKeyName are the reverse resolvers eval's
	// entity functions need to name a token (doc 09 §7). They may be nil when the
	// query names no entity labels, type, or keys; db.Query wires them from the
	// engine's catalog (deliverable 9).
	LabelName   func(t engine.Token) (string, bool)
	RelTypeName func(t engine.Token) (string, bool)
	PropKeyName func(t engine.Token) (string, bool)
	// Effects accumulates the mutation counts of a write query (doc 13 §3.4). The
	// write operators increment it as they run; the library write path reads it
	// after draining the cursor. It is nil for a read query, which never writes.
	Effects *SideEffects
}

// env builds the per-row evaluation environment from the context and a row.
func (c *Ctx) env(row eval.Row) *eval.Env {
	return &eval.Env{
		Row:         row,
		Params:      c.Params,
		Tx:          c.Tx,
		Resolve:     c.Resolve,
		LabelName:   c.LabelName,
		RelTypeName: c.RelTypeName,
		PropKeyName: c.PropKeyName,
	}
}

// ResolverFromBound builds a property-key resolver from a bound query: a name the
// catalog knew resolves to its token, an unknown one reports false (the schema-
// optional model, doc 08 §5.3, where reading an unknown key yields null rather
// than an error). It is the bridge db.Query (deliverable 9) uses to wire the
// binder's resolution into the executor.
func ResolverFromBound(b *bind.Bound) func(string) (engine.Token, bool) {
	return func(name string) (engine.Token, bool) {
		ref := b.PropKey(name)
		return ref.Token, ref.Known
	}
}

// operator is the executor's pull-based seam (doc 12 §2.1). open initializes the
// operator's state and may be called again to restart it (the correlated inner of
// an Optional is reopened once per outer row); next pulls one result row, with ok
// false at end of stream; close releases the operator's resources. M2 yields one
// eval.Row per next; M4 replaces eval.Row with a vector batch behind this same
// interface.
type operator interface {
	open(ctx *Ctx) error
	next() (eval.Row, bool, error)
	close() error
}

// Cursor is the public streaming handle over a query result: the output column
// names in order, and a Next/Close pair that pulls materialized rows. It owns the
// compiled operator tree and the execution context.
type Cursor struct {
	cols []string
	root operator
	ctx  *Ctx
}

// Open compiles the logical plan into an operator tree and starts it. The cursor
// streams rows lazily; nothing beyond the operator setup runs until Next is
// called. The caller must Close the cursor.
func Open(root plan.Op, ctx *Ctx) (*Cursor, error) {
	op, err := compile(root)
	if err != nil {
		return nil, err
	}
	if err := op.open(ctx); err != nil {
		return nil, err
	}
	return &Cursor{cols: Columns(root), root: op, ctx: ctx}, nil
}

// Columns returns a result's output column names in order: the projection's
// columns, looking through the ORDER BY / SKIP / LIMIT tail and taking a union's
// left arm (both arms share column names, the binder enforced it).
func Columns(root plan.Op) []string {
	switch x := root.(type) {
	case *plan.Project:
		return colNames(x.Cols)
	case *plan.Aggregate:
		return append(colNames(x.GroupKeys), colNames(x.Aggs)...)
	case *plan.ExpandCount:
		return []string{x.Col}
	case *plan.ProductCount:
		return []string{x.Col}
	case *plan.Sort:
		return Columns(x.Input)
	case *plan.Skip:
		return Columns(x.Input)
	case *plan.Limit:
		return Columns(x.Input)
	case *plan.Union:
		return Columns(x.Left)
	default:
		return nil
	}
}

func colNames(cols []plan.Col) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// Cols returns the cursor's output column names in order.
func (c *Cursor) Cols() []string { return c.cols }

// Next pulls the next result row, returning ok false at the end of the stream.
func (c *Cursor) Next() (eval.Row, bool, error) { return c.root.next() }

// Close releases the operator tree.
func (c *Cursor) Close() error { return c.root.close() }

// compile lowers one logical operator into its executor operator, recursively
// compiling its inputs. Relationship-uniqueness peers are threaded by compileRel.
func compile(o plan.Op) (operator, error) {
	op, _, err := compileRel(o, nil)
	return op, err
}

// compileRel compiles an operator while tracking the relationship variables bound
// by Expands in the same contiguous pattern chain, so each Expand can enforce
// relationship-uniqueness against its peers (doc 02 §4.3). The returned name set
// is the live rel-variable names a parent Expand must stay distinct from; a
// pipeline breaker (a projection, an aggregate, an unwind, a set operation, or an
// Optional's correlated boundary) starts a fresh pattern scope and returns an
// empty set, because relationship-uniqueness is scoped to a single MATCH.
func compileRel(o plan.Op, peers []string) (operator, []string, error) {
	switch x := o.(type) {
	case *plan.Unit:
		return &unitOp{}, peers, nil
	case *plan.Create:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &createOp{spec: x, input: input}, nil, nil
	case *plan.Merge:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		match, args, err := compileInner(x.Match)
		if err != nil {
			return nil, nil, err
		}
		return &mergeOp{spec: x, input: input, match: match, args: args}, nil, nil
	case *plan.Foreach:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		body, args, err := compileInner(x.Body)
		if err != nil {
			return nil, nil, err
		}
		return &foreachOp{input: input, body: body, args: args}, nil, nil
	case *plan.Set:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &setOp{spec: x, input: input}, nil, nil
	case *plan.Remove:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &removeOp{spec: x, input: input}, nil, nil
	case *plan.Delete:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &deleteOp{spec: x, input: input}, nil, nil
	case *plan.Argument:
		return &argumentOp{vars: x.Vars}, nil, nil
	case *plan.NodeScan:
		return &nodeScanOp{spec: x}, peers, nil
	case *plan.NodeIndexSeek:
		return &nodeIndexSeekOp{spec: x}, peers, nil
	case *plan.Expand:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		sib := append([]string(nil), inPeers...)
		var op operator
		if x.VarLen != nil {
			op = &varExpandOp{spec: x, input: input, peers: sib}
		} else {
			op = &expandOp{spec: x, input: input, peers: sib}
		}
		return op, append(inPeers, x.Rel), nil
	case *plan.Intersect:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		sib := append([]string(nil), inPeers...)
		op := &intersectOp{spec: x, input: input, peers: sib}
		return op, append(inPeers, x.Legs[0].Rel, x.Legs[1].Rel), nil
	case *plan.Filter:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &filterOp{pred: x.Pred, input: input}, inPeers, nil
	case *plan.BindPath:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &bindPathOp{spec: x, input: input}, inPeers, nil
	case *plan.ShortestPath:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		sib := append([]string(nil), inPeers...)
		return &shortestPathOp{spec: x, input: input, peers: sib}, append(inPeers, x.Rel), nil
	case *plan.Project:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &projectOp{spec: x, input: input}, nil, nil
	case *plan.Aggregate:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &aggregateOp{spec: x, input: input, inputPlan: x.Input}, nil, nil
	case *plan.ExpandCount:
		// The count stands in for an Aggregate over an Expand, so its input is
		// compiled in a fresh pattern scope (peers nil), exactly as the aggregate
		// compiled the expand's input. The rel-variable names that input binds are
		// the peers the counted edge must stay distinct from, the same sibling set
		// the replaced Expand carried.
		input, inPeers, err := compileRel(x.Input, nil)
		if err != nil {
			return nil, nil, err
		}
		return &expandCountOp{spec: x, input: input, peers: inPeers}, nil, nil
	case *plan.ProductCount:
		// The product folds in only over an input that binds no relationship (the
		// rewrite's guard), so there are no peers to thread and the input compiles in
		// a fresh pattern scope.
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &productCountOp{spec: x, input: input}, nil, nil
	case *plan.Unwind:
		var input operator
		if x.Input != nil {
			in, err := compile(x.Input)
			if err != nil {
				return nil, nil, err
			}
			input = in
		}
		return &unwindOp{expr: x.Expr, varName: x.Var, input: input}, nil, nil
	case *plan.Sort:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &sortOp{keys: x.Keys, input: input}, inPeers, nil
	case *plan.Skip:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &skipOp{n: x.N, input: input}, inPeers, nil
	case *plan.Limit:
		input, inPeers, err := compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &limitOp{n: x.N, input: input}, inPeers, nil
	case *plan.Join:
		left, err := compile(x.Left)
		if err != nil {
			return nil, nil, err
		}
		right, err := compile(x.Right)
		if err != nil {
			return nil, nil, err
		}
		return &joinOp{on: x.On, left: left, right: right}, nil, nil
	case *plan.Optional:
		input, err := compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		inner, args, err := compileInner(x.Inner)
		if err != nil {
			return nil, nil, err
		}
		newVars := diffVars(x.Inner, x.Input)
		return &optionalOp{input: input, inner: inner, args: args, newVars: newVars}, nil, nil
	case *plan.Union:
		left, err := compile(x.Left)
		if err != nil {
			return nil, nil, err
		}
		right, err := compile(x.Right)
		if err != nil {
			return nil, nil, err
		}
		return &unionOp{all: x.All, left: left, right: right}, nil, nil
	default:
		return nil, nil, fmt.Errorf("exec: cannot compile %T", o)
	}
}

// compileInner compiles a correlated subplan (an Optional's inner) and collects
// the Argument operators inside it so the Optional can feed each the current
// outer row before reopening the subplan.
func compileInner(o plan.Op) (operator, []*argumentOp, error) {
	op, err := compile(o)
	if err != nil {
		return nil, nil, err
	}
	return op, collectArguments(op), nil
}
