// Package plan is the logical-planning stage of the Cypher read path: it turns
// the binder's resolved tree ([bind]) into a logical operator tree and
// canonicalizes it with meaning-preserving rewrites (spec 2060 doc 10 §6, §7;
// doc 25 §5.2 deliverable 4). The result is the cost-based planner's input
// ([11](11-query-planner.md)): a correct, canonical operator tree, not yet an
// optimal physical plan.
//
// The split is deliberate (doc 10 §8.2): this stage owns correctness and
// canonical form, the planner owns cost. Logical planning is mechanical — it
// turns clauses into operators by structure (a MATCH into scans and expands, a
// WHERE into a filter, a WITH/RETURN into a projection) — and the normalization
// rewrites (predicate splitting and negation push, predicate pushdown, trivial
// filter elimination) run to fixpoint to put the tree in a regular form the
// planner can reason over without special cases.
//
// The logical operators carry the catalog tokens the binder resolved (labels,
// relationship types) so the planner and executor work in ids, and they name
// every variable explicitly (anonymous pattern elements get a synthetic, unique
// name) so the operator graph is fully connected.
package plan

import (
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// Op is one logical operator. The marker keeps the set closed so the rewrite
// engine and the planner can type-switch over a known set.
type Op interface {
	op()
}

// Unit is the single-empty-row source: the input to a query with no leading
// reading clause (RETURN 1, or a leading UNWIND), so projections always have a
// row to compute against.
type Unit struct{}

// Argument is the correlated-input leaf: the variables an enclosing operator
// (an Optional's outer side) supplies to a subtree. It lets a correlated
// subplan root its expands on already-bound variables rather than rescanning
// them.
type Argument struct {
	Vars []string
}

// NodeScan produces nodes. Labels is the resolved label set the node must carry;
// an empty Labels scans every node (the all-nodes scan). Var names the produced
// node binding.
type NodeScan struct {
	Var    string
	Labels []bind.NameRef
}

// Expand traverses a relationship from already-bound source nodes to their
// neighbors. From is the bound source variable, To the produced (or, when
// ToBound, the already-bound) neighbor, Rel the produced relationship variable.
// Types is the resolved type set (empty matches any type), Dir the direction,
// and VarLen the variable-length range (nil for a single hop). ToBound marks an
// expand-into: the target was already bound, so the expand keeps only edges that
// reach it.
type Expand struct {
	Input    Op
	From     string
	Rel      string
	To       string
	Types    []bind.NameRef
	ToLabels []bind.NameRef // labels the reached node must carry (empty: any)
	Dir      ast.Direction
	VarLen   *ast.VarLength
	ToBound  bool
}

// Filter keeps input rows for which Pred holds (a WHERE, or a pattern's
// property-map constraint lowered to an equality predicate).
type Filter struct {
	Input Op
	Pred  ast.Expr
}

// Col is one computed output column: an expression and the variable name it
// binds (an alias, a carried variable name, or an implicit name).
type Col struct {
	Expr ast.Expr
	Name string
}

// Project computes new bindings (a WITH or RETURN without aggregation). Distinct
// deduplicates the projected rows.
type Project struct {
	Input    Op
	Cols     []Col
	Distinct bool
}

// Aggregate groups by the non-aggregating columns and computes the aggregating
// ones (a WITH or RETURN that aggregates, doc 09 §8). GroupKeys are the grouping
// expressions, Aggs the aggregate expressions.
type Aggregate struct {
	Input     Op
	GroupKeys []Col
	Aggs      []Col
	Distinct  bool
}

// Join combines two row streams. On lists the shared variable names the rows
// must agree on; an empty On is a cartesian product.
type Join struct {
	Left, Right Op
	On          []string
}

// Optional is a left-outer join: every Input row is kept, extended by a match
// from Inner where one exists and padded with nulls where none does (OPTIONAL
// MATCH, doc 09 §4.2). Inner is correlated on the variables it shares with
// Input.
type Optional struct {
	Input Op
	Inner Op
}

// Unwind expands a list expression to one row per element, binding each to Var.
// Input is nil for a leading UNWIND (it runs over a single empty row).
type Unwind struct {
	Input Op
	Expr  ast.Expr
	Var   string
}

// SortKey is one ORDER BY key.
type SortKey struct {
	Expr ast.Expr
	Desc bool
}

// Sort orders its input by the keys.
type Sort struct {
	Input Op
	Keys  []SortKey
}

// Skip drops the first N rows; Limit keeps at most N. N is an expression
// (typically a literal or a parameter).
type Skip struct {
	Input Op
	N     ast.Expr
}

// Limit caps the row count at N.
type Limit struct {
	Input Op
	N     ast.Expr
}

// Union combines two query results. All keeps duplicates (UNION ALL); otherwise
// the result is deduplicated (UNION).
type Union struct {
	Left, Right Op
	All         bool
}

func (*Unit) op()      {}
func (*Argument) op()  {}
func (*NodeScan) op()  {}
func (*Expand) op()    {}
func (*Filter) op()    {}
func (*Project) op()   {}
func (*Aggregate) op() {}
func (*Join) op()      {}
func (*Optional) op()  {}
func (*Unwind) op()    {}
func (*Sort) op()      {}
func (*Skip) op()      {}
func (*Limit) op()     {}
func (*Union) op()     {}

// outputVars returns the set of variable names an operator's rows carry. It is
// the basis for predicate pushdown (a filter can move below an operator only if
// that operator's output already carries all the predicate's variables) and join
// keying.
func outputVars(o Op) map[string]bool {
	switch x := o.(type) {
	case *Unit:
		return map[string]bool{}
	case *Argument:
		s := make(map[string]bool, len(x.Vars))
		for _, v := range x.Vars {
			s[v] = true
		}
		return s
	case *NodeScan:
		return map[string]bool{x.Var: true}
	case *Expand:
		s := outputVars(x.Input)
		s[x.Rel] = true
		s[x.To] = true
		return s
	case *Filter:
		return outputVars(x.Input)
	case *Project:
		return colNames(x.Cols)
	case *Aggregate:
		s := colNames(x.GroupKeys)
		for n := range colNames(x.Aggs) {
			s[n] = true
		}
		return s
	case *Join:
		s := outputVars(x.Left)
		for n := range outputVars(x.Right) {
			s[n] = true
		}
		return s
	case *Optional:
		s := outputVars(x.Input)
		for n := range outputVars(x.Inner) {
			s[n] = true
		}
		return s
	case *Unwind:
		s := map[string]bool{}
		if x.Input != nil {
			s = outputVars(x.Input)
		}
		s[x.Var] = true
		return s
	case *Sort:
		return outputVars(x.Input)
	case *Skip:
		return outputVars(x.Input)
	case *Limit:
		return outputVars(x.Input)
	case *Union:
		return outputVars(x.Left)
	default:
		return map[string]bool{}
	}
}

func colNames(cols []Col) map[string]bool {
	s := make(map[string]bool, len(cols))
	for _, c := range cols {
		s[c.Name] = true
	}
	return s
}

// String renders the operator tree as an indented, deterministic plan, the form
// the tests assert against.
func String(o Op) string {
	var b strings.Builder
	write(&b, o, 0)
	return b.String()
}

func write(b *strings.Builder, o Op, depth int) {
	for range depth {
		b.WriteString("  ")
	}
	switch x := o.(type) {
	case *Unit:
		b.WriteString("Unit\n")
	case *Argument:
		b.WriteString("Argument [" + strings.Join(x.Vars, ",") + "]\n")
	case *NodeScan:
		b.WriteString("NodeScan " + x.Var + labelSuffix(x.Labels) + "\n")
	case *Expand:
		b.WriteString("Expand " + expandLabel(x) + "\n")
		write(b, x.Input, depth+1)
	case *Filter:
		b.WriteString("Filter " + ast.Print(x.Pred) + "\n")
		write(b, x.Input, depth+1)
	case *Project:
		b.WriteString("Project" + distinct(x.Distinct) + " " + colList(x.Cols) + "\n")
		write(b, x.Input, depth+1)
	case *Aggregate:
		b.WriteString("Aggregate" + distinct(x.Distinct) + " by[" + colList(x.GroupKeys) + "] agg[" + colList(x.Aggs) + "]\n")
		write(b, x.Input, depth+1)
	case *Join:
		b.WriteString("Join on[" + strings.Join(x.On, ",") + "]\n")
		write(b, x.Left, depth+1)
		write(b, x.Right, depth+1)
	case *Optional:
		b.WriteString("Optional\n")
		write(b, x.Input, depth+1)
		write(b, x.Inner, depth+1)
	case *Unwind:
		b.WriteString("Unwind " + ast.Print(x.Expr) + " AS " + x.Var + "\n")
		if x.Input != nil {
			write(b, x.Input, depth+1)
		}
	case *Sort:
		b.WriteString("Sort " + sortList(x.Keys) + "\n")
		write(b, x.Input, depth+1)
	case *Skip:
		b.WriteString("Skip " + ast.Print(x.N) + "\n")
		write(b, x.Input, depth+1)
	case *Limit:
		b.WriteString("Limit " + ast.Print(x.N) + "\n")
		write(b, x.Input, depth+1)
	case *Union:
		all := ""
		if x.All {
			all = " ALL"
		}
		b.WriteString("Union" + all + "\n")
		write(b, x.Left, depth+1)
		write(b, x.Right, depth+1)
	}
}

func distinct(d bool) string {
	if d {
		return " DISTINCT"
	}
	return ""
}

func labelSuffix(labels []bind.NameRef) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, len(labels))
	for i, l := range labels {
		if l.Known {
			parts[i] = "#" + itoa(int(l.Token))
		} else {
			parts[i] = "#?"
		}
	}
	return ":" + strings.Join(parts, "&")
}

func expandLabel(x *Expand) string {
	left, right := "-", "-"
	switch x.Dir {
	case ast.DirOut:
		right = "->"
	case ast.DirIn:
		left = "<-"
	}
	rel := x.Rel
	if x.VarLen != nil {
		rel += "*"
	}
	tail := labelSuffix(x.ToLabels)
	if x.ToBound {
		tail += " (into)"
	}
	return x.From + " " + left + "[" + rel + typeSuffix(x.Types) + "]" + right + " " + x.To + tail
}

func typeSuffix(types []bind.NameRef) string {
	if len(types) == 0 {
		return ""
	}
	parts := make([]string, len(types))
	for i, t := range types {
		if t.Known {
			parts[i] = "#" + itoa(int(t.Token))
		} else {
			parts[i] = "#?"
		}
	}
	return ":" + strings.Join(parts, "|")
}

func colList(cols []Col) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		if c.Name != "" && c.Name != ast.Print(c.Expr) {
			parts[i] = ast.Print(c.Expr) + " AS " + c.Name
		} else {
			parts[i] = ast.Print(c.Expr)
		}
	}
	return strings.Join(parts, ", ")
}

func sortList(keys []SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		dir := " ASC"
		if k.Desc {
			dir = " DESC"
		}
		parts[i] = ast.Print(k.Expr) + dir
	}
	return strings.Join(parts, ", ")
}

// itoa formats a non-negative int without importing fmt.
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
