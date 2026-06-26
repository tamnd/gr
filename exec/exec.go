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
	"sync/atomic"
	"time"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
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
	// MemBudget bounds the bytes a single stateful operator (a hash join build) may
	// hold in memory before it spills to disk (doc 12 §9). Zero (the default) means
	// unbounded: the operator stays in memory and behaves exactly as before, so a
	// query that does not set a budget gets the in-memory fast path and identical
	// answers. A positive budget arms spilling, which needs TempFile set too.
	MemBudget int64
	// TempFile opens a fresh, empty temporary file for an over-budget operator to
	// spill into (doc 12 §9.2): it returns the open file and a discard function that
	// closes and removes it. The operator owns the lifecycle, discarding each file
	// as it finishes with it and any survivors when it closes. It is nil when no
	// spill area is configured, in which case an over-budget operator stays in
	// memory (best effort) rather than failing.
	TempFile func() (vfs.File, func() error, error)
	// Scanned counts the rows scans and expands touch during execution (doc 20 §3.1),
	// the work behind gr_query_rows_scanned and the amplification numerator the PROFILE
	// footer also reports. The scan, seek, and expand operators add to it as the storage
	// layer hands them each id or neighbor; the library reads it after draining the
	// cursor. It is a pointer so every operator, including the morsel workers of a
	// parallel aggregate, shares the one counter; nil means no caller asked for the count
	// and the operators skip the add.
	Scanned *atomic.Int64
	// Graph receives the graph-operator execution events the graph-specific metrics count (doc
	// 20 §6): a shortest-path search, a worst-case-optimal join, a binary hash join. The graph
	// operators report through it as they open; the library wires it to the metric registry. It
	// is nil for an uninstrumented run, where the report is a nil check and nothing more.
	Graph GraphObserver
	// Profile, when set, collects each operator's actual rows and time during the run (doc 20
	// §9): Open wraps every operator in the profiling shim and the shim records into it on each
	// pull, keyed by the plan node the operator came from. The library reads it after the cursor
	// drains to render PROFILE's annotated tree. It is nil for an ordinary run, where no shim is
	// inserted and the executor is exactly the uninstrumented one.
	Profile *Profile
	// Tracer, when set, emits per-operator trace spans as children of the gr.execute phase span
	// (doc 20 §12.2, the detailed tracing level). Open wraps each operator in the tracing shim so
	// the span covers open-through-close with the operator's row count. It is nil when
	// tracing_detail is "phase" (the default) or when no tracer is configured.
	Tracer OpTracer
}

// GraphObserver receives graph-operator execution events for the graph-specific metric catalogue
// (doc 20 §6). The executor calls these as the operators open, once per operator execution, so
// the implementation only counts; it does no work on the row hot path. A nil GraphObserver on the
// Ctx disables the reporting, the embedded default.
type GraphObserver interface {
	// ShortestPath reports that a dedicated shortest-path search executed (doc 20 §6.1).
	ShortestPath()
	// WCOJ reports that a worst-case-optimal join (cyclic-pattern intersection) executed (§6.3).
	WCOJ()
	// BinaryJoin reports that a binary hash join (tree-pattern join) executed (§6.3).
	BinaryJoin()
	// Expand reports one source position expanded along relType and dir, producing fanout
	// neighbors in dur (doc 20 §6.1). relType is the operator's single type token, or zero when
	// it expands every type; the observer resolves the token to the metric's type label. fanout
	// is the raw neighbor count the engine produced for this source, the per-source fan-out whose
	// tail is the supernode signal.
	Expand(relType engine.Token, dir engine.Direction, fanout int, dur time.Duration)
	// VarLenExpand reports one variable-length path expansion from a source position along relType
	// (doc 20 §6.1), the recursive-traversal rate. relType is the operator's single type token, or
	// zero when it expands every type or several, the same convention as Expand.
	VarLenExpand(relType engine.Token)
	// VarLenDepth reports the hop count of one variable-length path the expansion produced (doc 20
	// §6.1); the distribution's deep tail is a deep, expensive traversal. relType follows the same
	// convention as VarLenExpand.
	VarLenDepth(relType engine.Token, depth int)
	// WCOJIntersect reports the time one worst-case-optimal multi-way intersection took, the cost
	// of matching one input row's cyclic pattern (doc 20 §6.3).
	WCOJIntersect(dur time.Duration)
	// JoinBuild reports the time spent building one binary hash join's build side, a
	// pipeline-breaker cost (doc 20 §6.3).
	JoinBuild(dur time.Duration)
	// Factorized reports that one operator produced or consumed a factorized intermediate, the
	// factorization-engaged count (doc 20 §6.3).
	Factorized()
	// FactorizationRatio reports the flat-over-factorized size of one factorized intermediate, the
	// compression factorization achieved; a high ratio is a many-to-many expansion it kept from
	// blowing up (doc 20 §6.3).
	FactorizationRatio(ratio float64)
	// IndexSeek reports one index lookup that served a query's anchor, descending in dur (doc 20
	// §6.4). label and prop are the indexed label and property tokens, which the observer resolves
	// to the index name; kind is the access kind (point, range), the anchor-selection view the
	// planner's choice produced. A lookup that fell back to a full scan does not report here, since
	// it took the scan path, not the index.
	IndexSeek(label, prop engine.Token, kind string, dur time.Duration)
}

// countScan adds n to the scanned-rows counter when one is armed (doc 20 §3.1). It is the
// single place the scan, seek, and expand operators record the work they did, counted where
// the storage layer delivers a row so a filtered-out or rejected row still counts as touched.
// With no counter set it is a nil check and nothing more, so an uninstrumented run pays nothing.
func (c *Ctx) countScan(n int64) {
	if c.Scanned != nil {
		c.Scanned.Add(n)
	}
}

// countShortestPath, countWCOJ, and countBinaryJoin report one graph-operator execution to the
// observer when one is wired (doc 20 §6). Each is the single place its operator reports, called
// once as the operator opens; with no observer set they are a nil check and nothing more.
func (c *Ctx) countShortestPath() {
	if c.Graph != nil {
		c.Graph.ShortestPath()
	}
}

func (c *Ctx) countWCOJ() {
	if c.Graph != nil {
		c.Graph.WCOJ()
	}
}

func (c *Ctx) countBinaryJoin() {
	if c.Graph != nil {
		c.Graph.BinaryJoin()
	}
}

// countExpand reports one source position expanded to the observer when one is wired (doc 20
// §6.1), called once per source node the expand operator pulls neighbors for. With no observer
// set it is a nil check and nothing more, so an uninstrumented expand pays nothing.
func (c *Ctx) countExpand(relType engine.Token, dir engine.Direction, fanout int, dur time.Duration) {
	if c.Graph != nil {
		c.Graph.Expand(relType, dir, fanout, dur)
	}
}

// countVarLenExpand and countVarLenDepth report a variable-length expansion's work to the observer
// when one is wired (doc 20 §6.1): the first once per source position the operator expands, the
// second once per path it produces. With no observer set they are a nil check and nothing more.
func (c *Ctx) countVarLenExpand(relType engine.Token) {
	if c.Graph != nil {
		c.Graph.VarLenExpand(relType)
	}
}

func (c *Ctx) countVarLenDepth(relType engine.Token, depth int) {
	if c.Graph != nil {
		c.Graph.VarLenDepth(relType, depth)
	}
}

// countWCOJIntersect, countJoinBuild, countFactorized, and countFactorizationRatio report the
// graph-join machinery's work to the observer when one is wired (doc 20 §6.3). Each is the single
// place its operator reports; with no observer set they are a nil check and nothing more.
func (c *Ctx) countWCOJIntersect(dur time.Duration) {
	if c.Graph != nil {
		c.Graph.WCOJIntersect(dur)
	}
}

func (c *Ctx) countJoinBuild(dur time.Duration) {
	if c.Graph != nil {
		c.Graph.JoinBuild(dur)
	}
}

func (c *Ctx) countFactorized() {
	if c.Graph != nil {
		c.Graph.Factorized()
	}
}

func (c *Ctx) countFactorizationRatio(ratio float64) {
	if c.Graph != nil {
		c.Graph.FactorizationRatio(ratio)
	}
}

// countIndexSeek reports one index-served anchor lookup to the observer when one is wired (doc 20
// §6.4), called once as the seek operator opens after its index descent. With no observer set it is
// a nil check and nothing more.
func (c *Ctx) countIndexSeek(label, prop engine.Token, kind string, dur time.Duration) {
	if c.Graph != nil {
		c.Graph.IndexSeek(label, prop, kind, dur)
	}
}

// spillEnabled reports whether an over-budget operator may spill: a positive
// memory budget and a configured temp area. With either unset, stateful operators
// run entirely in memory, the M2 behavior.
func (c *Ctx) spillEnabled() bool { return c.MemBudget > 0 && c.TempFile != nil }

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
//
// When the context carries a Profile, every operator is wrapped in the profiling
// shim as it compiles, so the instrumented run records each operator's actual rows
// and time (doc 20 §9.2). With no Profile the shim is never inserted and the tree
// is exactly the uninstrumented one. When the context carries a Tracer, every
// operator is additionally wrapped in the tracing shim so its span covers its full
// lifetime (doc 20 §12.2, the detailed level). Both shims are additive; having both
// is valid, though unusual.
func Open(root plan.Op, ctx *Ctx) (*Cursor, error) {
	c := &compiler{prof: ctx.Profile, tracer: ctx.Tracer}
	op, err := c.compile(root)
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
	case *plan.IntersectCount:
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

// ScanCount returns the cursor's shared scanned-rows counter, or nil when none is armed (doc 20
// §3.1). The library stores it on the result so Close reads the final scan work after the stream
// is drained, the amplification numerator for gr_query_rows_scanned.
func (c *Cursor) ScanCount() *atomic.Int64 { return c.ctx.Scanned }

// compiler lowers a logical plan into its executor operator tree (doc 12 §2.1). It
// carries the profiling shim across the recursion: when prof is non-nil every
// operator is wrapped as it compiles, so the instrumented run records each operator's
// actual rows and time against the plan node it came from (doc 20 §9.2). A nil prof
// is the uninstrumented path, where wrap returns the operator untouched.
type compiler struct {
	prof   *Profile
	tracer OpTracer
}

// compile lowers a plan with no profiling instrumentation, the plain compile the
// operators that recompile a subplan at run time use (a parallel join's build side, a
// morsel worker's input): their work runs inside a parent operator's pull, whose
// inclusive time already covers it, so the nested tree needs no shim of its own.
func compile(o plan.Op) (operator, error) {
	return (&compiler{}).compile(o)
}

// compile lowers one logical operator into its executor operator, recursively
// compiling its inputs. Relationship-uniqueness peers are threaded by compileRel.
func (c *compiler) compile(o plan.Op) (operator, error) {
	op, _, err := c.compileRel(o, nil)
	return op, err
}

// compileRel compiles an operator while tracking the relationship variables bound
// by Expands in the same contiguous pattern chain, so each Expand can enforce
// relationship-uniqueness against its peers (doc 02 §4.3). The returned name set
// is the live rel-variable names a parent Expand must stay distinct from; a
// pipeline breaker (a projection, an aggregate, an unwind, a set operation, or an
// Optional's correlated boundary) starts a fresh pattern scope and returns an
// empty set, because relationship-uniqueness is scoped to a single MATCH.
//
// It wraps the operator it builds in the profiling shim before returning, so an
// instrumented run measures every node once at the point it is produced; an
// uninstrumented run gets the operator back unchanged.
func (c *compiler) compileRel(o plan.Op, peers []string) (operator, []string, error) {
	op, sib, err := c.compileRelInner(o, peers)
	if err != nil {
		return nil, nil, err
	}
	op = c.prof.wrap(o, op)
	op = wrapTrace(c.tracer, o, op)
	return op, sib, nil
}

// compileRelInner is the operator-building body compileRel wraps. It holds the per
// operator-kind lowering and the relationship-uniqueness peer threading; the shim
// insertion lives in its caller so it happens once for every node.
func (c *compiler) compileRelInner(o plan.Op, peers []string) (operator, []string, error) {
	switch x := o.(type) {
	case *plan.Unit:
		return &unitOp{}, peers, nil
	case *plan.Create:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &createOp{spec: x, input: input}, nil, nil
	case *plan.Merge:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		match, args, err := c.compileInner(x.Match)
		if err != nil {
			return nil, nil, err
		}
		return &mergeOp{spec: x, input: input, match: match, args: args}, nil, nil
	case *plan.Foreach:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		body, args, err := c.compileInner(x.Body)
		if err != nil {
			return nil, nil, err
		}
		return &foreachOp{input: input, body: body, args: args}, nil, nil
	case *plan.Set:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &setOp{spec: x, input: input}, nil, nil
	case *plan.Remove:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &removeOp{spec: x, input: input}, nil, nil
	case *plan.Delete:
		input, err := c.compile(x.Input)
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
		input, inPeers, err := c.compileRel(x.Input, peers)
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
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		sib := append([]string(nil), inPeers...)
		op := &intersectOp{spec: x, input: input, peers: sib}
		return op, append(inPeers, x.Legs[0].Rel, x.Legs[1].Rel), nil
	case *plan.Filter:
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &filterOp{pred: x.Pred, input: input}, inPeers, nil
	case *plan.BindPath:
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &bindPathOp{spec: x, input: input}, inPeers, nil
	case *plan.ShortestPath:
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		sib := append([]string(nil), inPeers...)
		return &shortestPathOp{spec: x, input: input, peers: sib}, append(inPeers, x.Rel), nil
	case *plan.Project:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &projectOp{spec: x, input: input}, nil, nil
	case *plan.Aggregate:
		// A grouping-free aggregation over a bare scan whose aggregates run serially
		// (avg/sum/collect/distinct) fuses onto the scan, dropping the per-node row and
		// env the general path allocates (see scanAggregateOp). The scan is not compiled
		// as a child here: the fused operator owns it.
		if ns := fuseScanAggregate(x); ns != nil {
			return &scanAggregateOp{aggregateOp: aggregateOp{spec: x, inputPlan: x.Input}, scan: ns}, nil, nil
		}
		input, err := c.compile(x.Input)
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
		input, inPeers, err := c.compileRel(x.Input, nil)
		if err != nil {
			return nil, nil, err
		}
		return &expandCountOp{spec: x, input: input, peers: inPeers}, nil, nil
	case *plan.IntersectCount:
		// The count stands in for an Aggregate over an Intersect, so its input is
		// compiled in a fresh pattern scope (peers nil), exactly as the aggregate
		// compiled the intersection's input. The rel-variable names that input binds are
		// the peers each counted leg edge must stay distinct from, the same sibling set
		// the replaced Intersect's legs carried.
		//
		// When that input is a plain Expand chain over a NodeScan (the anchor path of a
		// closed cycle: a->b for the triangle, a->b->c for the four-cycle), optionally
		// under an anchor filter such as the undirected triangle's id(a) < id(b), the
		// count fuses onto the scan and chain, dropping the per-anchor-path row the
		// general path builds only to read its endpoints back (see fusedIntersectCountOp).
		// The scan and expands are not compiled as children: the fused operator drives
		// them through the engine SPI itself and evaluates the anchor predicate per anchor
		// path in place of the peeled Filter.
		if FusePolygonCount {
			if ns, hops, anchor := plan.FusePolygonAnchor(x); ns != nil {
				return &fusedIntersectCountOp{spec: x, hops: hops, ns: ns, anchor: anchor}, nil, nil
			}
		}
		input, inPeers, err := c.compileRel(x.Input, nil)
		if err != nil {
			return nil, nil, err
		}
		return &intersectCountOp{spec: x, input: input, peers: inPeers}, nil, nil
	case *plan.ProductCount:
		// The product folds in only over an input that binds no relationship (the
		// rewrite's guard), so there are no peers to thread and the input compiles in
		// a fresh pattern scope.
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		return &productCountOp{spec: x, input: input}, nil, nil
	case *plan.Unwind:
		var input operator
		if x.Input != nil {
			in, err := c.compile(x.Input)
			if err != nil {
				return nil, nil, err
			}
			input = in
		}
		return &unwindOp{expr: x.Expr, varName: x.Var, input: input}, nil, nil
	case *plan.Sort:
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &sortOp{keys: x.Keys, input: input}, inPeers, nil
	case *plan.Skip:
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &skipOp{n: x.N, input: input}, inPeers, nil
	case *plan.Limit:
		input, inPeers, err := c.compileRel(x.Input, peers)
		if err != nil {
			return nil, nil, err
		}
		return &limitOp{n: x.N, input: input}, inPeers, nil
	case *plan.Join:
		left, err := c.compile(x.Left)
		if err != nil {
			return nil, nil, err
		}
		right, err := c.compile(x.Right)
		if err != nil {
			return nil, nil, err
		}
		return &joinOp{on: x.On, left: left, right: right, rightPlan: x.Right}, nil, nil
	case *plan.Optional:
		input, err := c.compile(x.Input)
		if err != nil {
			return nil, nil, err
		}
		inner, args, err := c.compileInner(x.Inner)
		if err != nil {
			return nil, nil, err
		}
		newVars := diffVars(x.Inner, x.Input)
		return &optionalOp{input: input, inner: inner, args: args, newVars: newVars}, nil, nil
	case *plan.Union:
		left, err := c.compile(x.Left)
		if err != nil {
			return nil, nil, err
		}
		right, err := c.compile(x.Right)
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
func (c *compiler) compileInner(o plan.Op) (operator, []*argumentOp, error) {
	op, err := c.compile(o)
	if err != nil {
		return nil, nil, err
	}
	return op, collectArguments(op), nil
}
