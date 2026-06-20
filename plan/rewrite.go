package plan

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/value"
)

// Normalize canonicalizes a logical tree with meaning-preserving rewrites,
// applied to fixpoint (doc 10 §7): predicate normalization (push NOT inward,
// drop double negation), conjunction splitting (one filter per conjunct so each
// pushes independently), predicate pushdown (move a filter to the operator that
// produces its variables, so filtering is early), and trivial-filter
// elimination. Equivalent queries converge to the same tree, and the planner
// sees a regular input without special cases.
//
// Deferred to later stages (and noted in the implementation doc): constant
// folding and other value-level simplification compose with the expression
// evaluator (deliverable 7); subquery flattening awaits subquery syntax; full
// OPTIONAL-MATCH outer-join normalization and projection elimination await the
// cost-based planner.
func Normalize(o Op) Op {
	for {
		next, changed := apply(o)
		if !changed {
			return next
		}
		o = next
	}
}

// apply rebuilds an operator's children bottom-up, then runs the node-local
// rewrite rules, reporting whether anything changed.
func apply(o Op) (Op, bool) {
	o, ch := rebuildChildren(o)
	o2, ch2 := localRules(o)
	return o2, ch || ch2
}

// rebuildChildren applies the rewrite to each child and reconstructs the node.
func rebuildChildren(o Op) (Op, bool) {
	switch x := o.(type) {
	case *Create:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		y := *x
		y.Input = in
		return &y, true
	case *Expand:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		y := *x
		y.Input = in
		return &y, true
	case *Filter:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		return &Filter{Input: in, Pred: x.Pred}, true
	case *BindPath:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		y := *x
		y.Input = in
		return &y, true
	case *ShortestPath:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		y := *x
		y.Input = in
		return &y, true
	case *Project:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		y := *x
		y.Input = in
		return &y, true
	case *Aggregate:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		y := *x
		y.Input = in
		return &y, true
	case *Join:
		l, lc := apply(x.Left)
		r, rc := apply(x.Right)
		if !lc && !rc {
			return x, false
		}
		return &Join{Left: l, Right: r, On: x.On}, true
	case *Optional:
		in, ic := apply(x.Input)
		inner, nc := apply(x.Inner)
		if !ic && !nc {
			return x, false
		}
		return &Optional{Input: in, Inner: inner}, true
	case *Unwind:
		if x.Input == nil {
			return x, false
		}
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		return &Unwind{Input: in, Expr: x.Expr, Var: x.Var}, true
	case *Sort:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		return &Sort{Input: in, Keys: x.Keys}, true
	case *Skip:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		return &Skip{Input: in, N: x.N}, true
	case *Limit:
		in, ch := apply(x.Input)
		if !ch {
			return x, false
		}
		return &Limit{Input: in, N: x.N}, true
	case *Union:
		l, lc := apply(x.Left)
		r, rc := apply(x.Right)
		if !lc && !rc {
			return x, false
		}
		return &Union{Left: l, Right: r, All: x.All}, true
	default:
		return o, false // Unit, Argument, NodeScan: leaves
	}
}

// localRules applies the node-local normalization rules. Only Filter has rules;
// every other operator is returned unchanged.
func localRules(o Op) (Op, bool) {
	f, ok := o.(*Filter)
	if !ok {
		return o, false
	}
	changed := false
	pred, nc := normalizeExpr(f.Pred)
	if nc {
		f = &Filter{Input: f.Input, Pred: pred}
		changed = true
	}
	// A trivially-true filter is a no-op.
	if isTrue(f.Pred) {
		return f.Input, true
	}
	// Split a conjunction into nested filters so each conjunct pushes alone.
	if b, ok := f.Pred.(*ast.Binary); ok && b.Op == ast.OpAnd {
		return &Filter{Input: &Filter{Input: f.Input, Pred: b.R}, Pred: b.L}, true
	}
	// Push the filter toward the operator that produces its variables.
	if pushed, ok := pushFilter(f); ok {
		return pushed, true
	}
	return f, changed
}

// pushFilter moves a single-predicate filter below its input where the move is
// meaning-preserving: below an expand or unwind whose input already carries the
// predicate's variables, into the matching side of a join, or below a sort.
func pushFilter(f *Filter) (Op, bool) {
	v := freeVars(f.Pred)
	switch x := f.Input.(type) {
	case *Filter:
		// Filters commute; try to push this predicate below the inner one. Only
		// commit if it can actually descend further, so the rewrite makes progress
		// rather than oscillating.
		if pushed, ok := pushFilter(&Filter{Input: x.Input, Pred: f.Pred}); ok {
			return &Filter{Input: pushed, Pred: x.Pred}, true
		}
	case *Expand:
		if subset(v, outputVars(x.Input)) {
			y := *x
			y.Input = &Filter{Input: x.Input, Pred: f.Pred}
			return &y, true
		}
	case *Unwind:
		if x.Input != nil && subset(v, outputVars(x.Input)) {
			return &Unwind{Input: &Filter{Input: x.Input, Pred: f.Pred}, Expr: x.Expr, Var: x.Var}, true
		}
	case *Join:
		if subset(v, outputVars(x.Left)) {
			return &Join{Left: &Filter{Input: x.Left, Pred: f.Pred}, Right: x.Right, On: x.On}, true
		}
		if subset(v, outputVars(x.Right)) {
			return &Join{Left: x.Left, Right: &Filter{Input: x.Right, Pred: f.Pred}, On: x.On}, true
		}
	case *Sort:
		return &Sort{Input: &Filter{Input: x.Input, Pred: f.Pred}, Keys: x.Keys}, true
	}
	return f, false
}

// --- predicate normalization ---

// normalizeExpr pushes NOT inward (De Morgan and comparison negation, valid in
// the three-valued logic) and eliminates double negation, recursing through the
// boolean structure. Value-level folding is deferred to the evaluator.
func normalizeExpr(e ast.Expr) (ast.Expr, bool) {
	switch x := e.(type) {
	case *ast.Unary:
		if x.Op == ast.OpNot {
			return normalizeNot(x.X)
		}
		inner, ch := normalizeExpr(x.X)
		if ch {
			return &ast.Unary{Op: x.Op, X: inner}, true
		}
		return x, false
	case *ast.Binary:
		l, lc := normalizeExpr(x.L)
		r, rc := normalizeExpr(x.R)
		if lc || rc {
			return &ast.Binary{Op: x.Op, L: l, R: r}, true
		}
		return x, false
	default:
		return e, false
	}
}

// normalizeNot rewrites NOT applied to an expression: double negation cancels,
// De Morgan distributes over AND/OR, and a negated comparison flips its
// operator. Anything else keeps the NOT but normalizes its operand.
func normalizeNot(inner ast.Expr) (ast.Expr, bool) {
	switch x := inner.(type) {
	case *ast.Unary:
		if x.Op == ast.OpNot {
			out, _ := normalizeExpr(x.X)
			return out, true
		}
	case *ast.Binary:
		switch x.Op {
		case ast.OpAnd:
			l, _ := normalizeExpr(&ast.Unary{Op: ast.OpNot, X: x.L})
			r, _ := normalizeExpr(&ast.Unary{Op: ast.OpNot, X: x.R})
			return &ast.Binary{Op: ast.OpOr, L: l, R: r}, true
		case ast.OpOr:
			l, _ := normalizeExpr(&ast.Unary{Op: ast.OpNot, X: x.L})
			r, _ := normalizeExpr(&ast.Unary{Op: ast.OpNot, X: x.R})
			return &ast.Binary{Op: ast.OpAnd, L: l, R: r}, true
		case ast.OpEq, ast.OpNe, ast.OpLt, ast.OpLe, ast.OpGt, ast.OpGe:
			l, _ := normalizeExpr(x.L)
			r, _ := normalizeExpr(x.R)
			return &ast.Binary{Op: flipComparison(x.Op), L: l, R: r}, true
		}
	}
	out, _ := normalizeExpr(inner)
	return &ast.Unary{Op: ast.OpNot, X: out}, false
}

// flipComparison returns the operator equivalent to negating a comparison.
func flipComparison(op ast.BinaryOp) ast.BinaryOp {
	switch op {
	case ast.OpEq:
		return ast.OpNe
	case ast.OpNe:
		return ast.OpEq
	case ast.OpLt:
		return ast.OpGe
	case ast.OpLe:
		return ast.OpGt
	case ast.OpGt:
		return ast.OpLe
	case ast.OpGe:
		return ast.OpLt
	default:
		return op
	}
}

// --- expression helpers ---

// isTrue reports whether an expression is the boolean literal true.
func isTrue(e ast.Expr) bool {
	lit, ok := e.(*ast.Literal)
	if !ok || lit.Value.Type() != value.TypeBool {
		return false
	}
	bv, _ := lit.Value.AsBool()
	return bv
}

// freeVars collects the variable names an expression references.
func freeVars(e ast.Expr) map[string]bool {
	s := map[string]bool{}
	collectVars(e, s)
	return s
}

func collectVars(e ast.Expr, s map[string]bool) {
	switch x := e.(type) {
	case *ast.Variable:
		s[x.Name] = true
	case *ast.Property:
		collectVars(x.Base, s)
	case *ast.Index:
		collectVars(x.Base, s)
		collectVars(x.Index, s)
	case *ast.Slice:
		collectVars(x.Base, s)
		if x.Lo != nil {
			collectVars(x.Lo, s)
		}
		if x.Hi != nil {
			collectVars(x.Hi, s)
		}
	case *ast.Unary:
		collectVars(x.X, s)
	case *ast.Binary:
		collectVars(x.L, s)
		collectVars(x.R, s)
	case *ast.IsNull:
		collectVars(x.X, s)
	case *ast.ListLit:
		for _, el := range x.Elems {
			collectVars(el, s)
		}
	case *ast.MapLit:
		for _, ent := range x.Entries {
			collectVars(ent.Value, s)
		}
	case *ast.FunctionCall:
		for _, a := range x.Args {
			collectVars(a, s)
		}
	case *ast.Case:
		if x.Subject != nil {
			collectVars(x.Subject, s)
		}
		for _, w := range x.Whens {
			collectVars(w.When, s)
			collectVars(w.Then, s)
		}
		if x.Else != nil {
			collectVars(x.Else, s)
		}
	}
}

// subset reports whether every key of a is present in b.
func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
