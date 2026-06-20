package bind

import (
	"strings"

	"github.com/tamnd/gr/ast"
)

// checkExpr validates one expression against a scope: every referenced variable
// must be bound, every property key is resolved and recorded, and aggregates are
// placed and nested legally. allowAgg reports whether an aggregating call is
// permitted here — true inside a projection or ORDER BY, false in a WHERE, an
// UNWIND list, or a pattern's property map (doc 10 §4.3).
func (bd *binder) checkExpr(e ast.Expr, sc scope, allowAgg bool) error {
	switch x := e.(type) {
	case *ast.Literal, *ast.Param:
		return nil
	case *ast.Variable:
		if _, ok := sc[x.Name]; !ok {
			return &Error{"variable " + x.Name + " is not defined", x.Line, x.Col}
		}
		return nil
	case *ast.Property:
		if err := bd.resolvePropKey(x.Key, x.Pos); err != nil {
			return err
		}
		return bd.checkExpr(x.Base, sc, allowAgg)
	case *ast.Index:
		if err := bd.checkExpr(x.Base, sc, allowAgg); err != nil {
			return err
		}
		return bd.checkExpr(x.Index, sc, allowAgg)
	case *ast.Slice:
		if err := bd.checkExpr(x.Base, sc, allowAgg); err != nil {
			return err
		}
		if x.Lo != nil {
			if err := bd.checkExpr(x.Lo, sc, allowAgg); err != nil {
				return err
			}
		}
		if x.Hi != nil {
			return bd.checkExpr(x.Hi, sc, allowAgg)
		}
		return nil
	case *ast.Unary:
		return bd.checkExpr(x.X, sc, allowAgg)
	case *ast.Binary:
		if err := bd.checkExpr(x.L, sc, allowAgg); err != nil {
			return err
		}
		return bd.checkExpr(x.R, sc, allowAgg)
	case *ast.IsNull:
		return bd.checkExpr(x.X, sc, allowAgg)
	case *ast.ListLit:
		for _, el := range x.Elems {
			if err := bd.checkExpr(el, sc, allowAgg); err != nil {
				return err
			}
		}
		return nil
	case *ast.MapLit:
		// Map-literal keys are arbitrary map keys, not catalog property keys, so
		// they are not resolved; only the values are checked.
		for _, ent := range x.Entries {
			if err := bd.checkExpr(ent.Value, sc, allowAgg); err != nil {
				return err
			}
		}
		return nil
	case *ast.FunctionCall:
		return bd.checkCall(x, sc, allowAgg)
	case *ast.Case:
		return bd.checkCase(x, sc, allowAgg)
	default:
		return &Error{"unsupported expression", 0, 0}
	}
}

// checkCall validates a function call. An aggregate (count, sum, …) is allowed
// only where allowAgg holds and may not contain another aggregate (no
// count(sum(x))); a plain function passes allowAgg through to its arguments so
// an aggregate may still appear inside a scalar call in a projection.
func (bd *binder) checkCall(c *ast.FunctionCall, sc scope, allowAgg bool) error {
	if isAggregate(c.Name) {
		if !allowAgg {
			return &Error{"aggregate function " + c.Name + " is not allowed here", c.Line, c.Col}
		}
		for _, a := range c.Args {
			if containsAggregate(a) {
				return &Error{"aggregate function " + c.Name + " may not contain another aggregate", c.Line, c.Col}
			}
		}
		// count(*) has no argument expressions to check.
		for _, a := range c.Args {
			if err := bd.checkExpr(a, sc, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, a := range c.Args {
		if err := bd.checkExpr(a, sc, allowAgg); err != nil {
			return err
		}
	}
	return nil
}

func (bd *binder) checkCase(c *ast.Case, sc scope, allowAgg bool) error {
	if c.Subject != nil {
		if err := bd.checkExpr(c.Subject, sc, allowAgg); err != nil {
			return err
		}
	}
	for _, w := range c.Whens {
		if err := bd.checkExpr(w.When, sc, allowAgg); err != nil {
			return err
		}
		if err := bd.checkExpr(w.Then, sc, allowAgg); err != nil {
			return err
		}
	}
	if c.Else != nil {
		return bd.checkExpr(c.Else, sc, allowAgg)
	}
	return nil
}

// aggregates is the set of openCypher aggregating functions (doc 09 §8.1),
// matched case-insensitively.
var aggregates = map[string]bool{
	"count":          true,
	"sum":            true,
	"avg":            true,
	"min":            true,
	"max":            true,
	"collect":        true,
	"stdev":          true,
	"stdevp":         true,
	"percentilecont": true,
	"percentiledisc": true,
}

// isAggregate reports whether a function name is an aggregating function.
func isAggregate(name string) bool { return aggregates[strings.ToLower(name)] }

// containsAggregate reports whether an expression subtree contains an aggregate
// call, used to forbid nesting one aggregate inside another.
func containsAggregate(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.FunctionCall:
		if isAggregate(x.Name) {
			return true
		}
		for _, a := range x.Args {
			if containsAggregate(a) {
				return true
			}
		}
		return false
	case *ast.Property:
		return containsAggregate(x.Base)
	case *ast.Index:
		return containsAggregate(x.Base) || containsAggregate(x.Index)
	case *ast.Slice:
		return containsAggregate(x.Base) ||
			(x.Lo != nil && containsAggregate(x.Lo)) ||
			(x.Hi != nil && containsAggregate(x.Hi))
	case *ast.Unary:
		return containsAggregate(x.X)
	case *ast.Binary:
		return containsAggregate(x.L) || containsAggregate(x.R)
	case *ast.IsNull:
		return containsAggregate(x.X)
	case *ast.ListLit:
		for _, el := range x.Elems {
			if containsAggregate(el) {
				return true
			}
		}
		return false
	case *ast.MapLit:
		for _, ent := range x.Entries {
			if containsAggregate(ent.Value) {
				return true
			}
		}
		return false
	case *ast.Case:
		if x.Subject != nil && containsAggregate(x.Subject) {
			return true
		}
		for _, w := range x.Whens {
			if containsAggregate(w.When) || containsAggregate(w.Then) {
				return true
			}
		}
		return x.Else != nil && containsAggregate(x.Else)
	default:
		return false
	}
}

// --- column naming ---

// projectionColumns names the result columns of a projection, in order. A star
// expands to the carried scope's variables (sorted for determinism); each item
// is named by its alias, its variable name, its property path, or a compact
// printed form of the expression.
func projectionColumns(p *ast.Projection, sc scope) []string {
	var cols []string
	if p.Star {
		cols = append(cols, sortedNames(sc)...)
	}
	for _, it := range p.Items {
		cols = append(cols, columnName(it))
	}
	return cols
}

// columnName is the result-column name of a projected item: its AS alias if
// present, otherwise the expression's own name.
func columnName(it ast.ProjItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	return exprName(it.Expr)
}

// exprName is the implicit column name of an unaliased expression: a variable's
// name, a property path, or a compact printed form for anything else.
func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Variable:
		return x.Name
	case *ast.Property:
		return exprName(x.Base) + "." + x.Key
	default:
		return printExpr(e)
	}
}

// sortedNames returns a scope's variable names in ascending order.
func sortedNames(sc scope) []string {
	names := make([]string, 0, len(sc))
	for k := range sc {
		names = append(names, k)
	}
	// insertion sort keeps this dependency-free and the scopes are small.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// unionColumnsMatch checks two UNION arms project compatible columns: the same
// names in the same order (doc 09 §7). Arms that project a star are skipped, as
// their column set is not statically known here.
func unionColumnsMatch(a, b []string, pos ast.Pos) error {
	if hasStarColumn(a) || hasStarColumn(b) {
		return nil
	}
	if len(a) != len(b) {
		return &Error{"UNION arms project a different number of columns", pos.Line, pos.Col}
	}
	for i := range a {
		if a[i] != b[i] {
			return &Error{"UNION arms project different column names", pos.Line, pos.Col}
		}
	}
	return nil
}

// hasStarColumn reports whether a column list contains an empty name, the marker
// an anonymous or star-expanded projection can leave.
func hasStarColumn(cols []string) bool {
	for _, c := range cols {
		if c == "" {
			return true
		}
	}
	return false
}

// printExpr renders an expression to a compact source-like string for use as an
// implicit column name. It is a best-effort label, not a faithful pretty-printer.
func printExpr(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Literal:
		return x.Value.String()
	case *ast.Param:
		return "$" + x.Name
	case *ast.Variable:
		return x.Name
	case *ast.Property:
		return printExpr(x.Base) + "." + x.Key
	case *ast.Index:
		return printExpr(x.Base) + "[" + printExpr(x.Index) + "]"
	case *ast.Slice:
		return printExpr(x.Base) + "[" + optPrint(x.Lo) + ".." + optPrint(x.Hi) + "]"
	case *ast.Unary:
		if x.Op == ast.OpNot {
			return "NOT " + printExpr(x.X)
		}
		return "-" + printExpr(x.X)
	case *ast.Binary:
		return printExpr(x.L) + " " + x.Op.String() + " " + printExpr(x.R)
	case *ast.IsNull:
		if x.Negate {
			return printExpr(x.X) + " IS NOT NULL"
		}
		return printExpr(x.X) + " IS NULL"
	case *ast.ListLit:
		return "[" + joinExprs(x.Elems) + "]"
	case *ast.MapLit:
		return "{...}"
	case *ast.FunctionCall:
		if x.Star {
			return x.Name + "(*)"
		}
		pre := ""
		if x.Distinct {
			pre = "DISTINCT "
		}
		return x.Name + "(" + pre + joinExprs(x.Args) + ")"
	case *ast.Case:
		return "CASE"
	default:
		return "?"
	}
}

func optPrint(e ast.Expr) string {
	if e == nil {
		return ""
	}
	return printExpr(e)
}

func joinExprs(es []ast.Expr) string {
	var b strings.Builder
	for i, e := range es {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(printExpr(e))
	}
	return b.String()
}

// exprPos extracts a node's source position for error messages.
func exprPos(e ast.Expr) ast.Pos {
	switch x := e.(type) {
	case *ast.Literal:
		return x.Pos
	case *ast.ListLit:
		return x.Pos
	case *ast.MapLit:
		return x.Pos
	case *ast.Param:
		return x.Pos
	case *ast.Variable:
		return x.Pos
	case *ast.Property:
		return x.Pos
	case *ast.Index:
		return x.Pos
	case *ast.Slice:
		return x.Pos
	case *ast.Unary:
		return x.Pos
	case *ast.Binary:
		return x.Pos
	case *ast.IsNull:
		return x.Pos
	case *ast.FunctionCall:
		return x.Pos
	case *ast.Case:
		return x.Pos
	default:
		return ast.Pos{}
	}
}
