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
	"sort"
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

// BindPath binds a named path variable to the path value assembled from the
// pattern's element variables in traversal order: the start node, then each
// step's relationship and node (MATCH p = (a)-[r]->(b), doc 09 §3.4). It is
// added above a path pattern's expand chain when the pattern names a path. Elems
// lists the element variable names in order (node, rel, node, ...); each is
// already bound below.
type BindPath struct {
	Input Op
	Var   string
	Elems []string
}

// ShortestPath finds the shortest path(s) between two already-bound endpoint
// nodes (shortestPath / allShortestPaths, doc 09 §3.4, doc 12 §4.4). From and To
// are the endpoint variables, bound below by scans or earlier clauses; Rel binds
// the path's relationship list (like a variable-length expand); PathVar binds the
// materialized path value when the pattern names one ("" otherwise). Types is the
// resolved type set, Dir the direction, and VarLen the hop range (nil for a fixed
// single hop). All selects allShortestPaths: every path of the minimum length,
// rather than one.
type ShortestPath struct {
	Input   Op
	From    string
	To      string
	Rel     string
	PathVar string
	Types   []bind.NameRef
	Dir     ast.Direction
	VarLen  *ast.VarLength
	All     bool
}

// Create is the write operator that materializes a CREATE clause's patterns. It
// runs once per input row (the read part feeds it; a leading CREATE runs over the
// single Unit row), creating every new node and relationship, binding each to its
// variable, and passing the augmented row upward (doc 13 §5). Nodes are created
// before relationships so a relationship's endpoints — bound earlier or created
// here — always exist when it is built.
type Create struct {
	Input Op
	Nodes []NodeCreate
	Rels  []RelCreate
}

// NodeCreate is one node to create: the variable it binds, its labels, and its
// property assignments.
type NodeCreate struct {
	Var    string
	Labels []bind.NameRef
	Props  []PropSet
}

// RelCreate is one relationship to create: the variable it binds, its endpoint
// variables (From toward To, already oriented by the pattern's direction), its
// single type, and its property assignments.
type RelCreate struct {
	Var   string
	From  string
	To    string
	Type  bind.NameRef
	Props []PropSet
}

// PropSet is one property assignment in a write clause: the resolved key and the
// expression computing its value. A value that evaluates to null leaves the
// property unset (doc 13 §5.4).
type PropSet struct {
	Key  bind.NameRef
	Expr ast.Expr
}

// Set is the write operator for a SET clause: it applies each update item to the
// bound elements in the row, in order, and passes the row on unchanged (it binds
// nothing new, doc 13 §6). It runs once per input row.
type Set struct {
	Input Op
	Items []SetItem
}

// SetItem is one lowered SET assignment. Kind selects the shape: SetItemProp
// assigns Key from Expr on the element bound to Var; SetItemLabels adds Labels to
// the node bound to Var; SetItemMerge and SetItemReplace apply the map (or source
// element) Expr evaluates to, merging or replacing the target's properties.
type SetItem struct {
	Kind   SetItemKind
	Var    string
	Key    bind.NameRef   // SetItemProp
	Expr   ast.Expr       // SetItemProp, SetItemMerge, SetItemReplace
	Labels []bind.NameRef // SetItemLabels
}

// SetItemKind classifies a lowered SET item.
type SetItemKind uint8

const (
	// SetItemProp is a single-property assignment (n.k = e).
	SetItemProp SetItemKind = iota
	// SetItemLabels is a label addition (n:A:B).
	SetItemLabels
	// SetItemMerge is a map merge (n += m): set the map's keys, keep the rest.
	SetItemMerge
	// SetItemReplace is a map replace (n = m): the target ends with exactly m.
	SetItemReplace
)

// Remove is the write operator for a REMOVE clause: it removes each item's
// property or labels from the bound element and passes the row on unchanged (doc
// 13 §7). It runs once per input row.
type Remove struct {
	Input Op
	Items []RemoveItem
}

// RemoveItem is one lowered REMOVE target. Labels non-empty marks a label
// removal from the node bound to Var; otherwise it is a property removal of Key
// from the element bound to Var.
type RemoveItem struct {
	Var    string
	Key    bind.NameRef   // property removal
	Labels []bind.NameRef // label removal
}

// Delete is the write operator for a DELETE or DETACH DELETE clause: for each
// input row it evaluates every target expression and removes the node or
// relationship it names, then passes the row on unchanged (doc 13 §9). It binds
// nothing new. Detach cascades to a node's relationships before removing it.
type Delete struct {
	Input   Op
	Detach  bool
	Targets []ast.Expr
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

func (*Unit) op()         {}
func (*Create) op()       {}
func (*Set) op()          {}
func (*Remove) op()       {}
func (*Delete) op()       {}
func (*Argument) op()     {}
func (*NodeScan) op()     {}
func (*Expand) op()       {}
func (*Filter) op()       {}
func (*BindPath) op()     {}
func (*ShortestPath) op() {}
func (*Project) op()      {}
func (*Aggregate) op()    {}
func (*Join) op()         {}
func (*Optional) op()     {}
func (*Unwind) op()       {}
func (*Sort) op()         {}
func (*Skip) op()         {}
func (*Limit) op()        {}
func (*Union) op()        {}

// outputVars returns the set of variable names an operator's rows carry. It is
// the basis for predicate pushdown (a filter can move below an operator only if
// that operator's output already carries all the predicate's variables) and join
// keying.
func outputVars(o Op) map[string]bool {
	switch x := o.(type) {
	case *Unit:
		return map[string]bool{}
	case *Create:
		s := outputVars(x.Input)
		for _, n := range x.Nodes {
			s[n.Var] = true
		}
		for _, r := range x.Rels {
			s[r.Var] = true
		}
		return s
	case *Set:
		return outputVars(x.Input)
	case *Remove:
		return outputVars(x.Input)
	case *Delete:
		return outputVars(x.Input)
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
	case *BindPath:
		s := outputVars(x.Input)
		s[x.Var] = true
		return s
	case *ShortestPath:
		s := outputVars(x.Input)
		s[x.Rel] = true
		if x.PathVar != "" {
			s[x.PathVar] = true
		}
		return s
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

// OutputVars returns the variable names an operator's rows carry, sorted, for
// consumers outside the package (the executor null-pads an unmatched OPTIONAL
// MATCH against the inner's new variables, doc 09 §4.2).
func OutputVars(o Op) []string {
	m := outputVars(o)
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
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
	case *Create:
		b.WriteString("Create " + createLabel(x) + "\n")
		write(b, x.Input, depth+1)
	case *Set:
		b.WriteString("Set " + setLabel(x) + "\n")
		write(b, x.Input, depth+1)
	case *Remove:
		b.WriteString("Remove " + removeLabel(x) + "\n")
		write(b, x.Input, depth+1)
	case *Delete:
		b.WriteString("Delete " + deleteLabel(x) + "\n")
		write(b, x.Input, depth+1)
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
	case *BindPath:
		b.WriteString("BindPath " + x.Var + " = [" + strings.Join(x.Elems, ",") + "]\n")
		write(b, x.Input, depth+1)
	case *ShortestPath:
		b.WriteString("ShortestPath " + shortestLabel(x) + "\n")
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

// shortestLabel renders a ShortestPath operator's pattern: the search kind, the
// (optional) path variable, and the relationship between the two endpoints.
func shortestLabel(x *ShortestPath) string {
	kind := "shortest"
	if x.All {
		kind = "allShortest"
	}
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
	s := kind + " " + x.From + " " + left + "[" + rel + typeSuffix(x.Types) + "]" + right + " " + x.To
	if x.PathVar != "" {
		s = x.PathVar + " = " + s
	}
	return s
}

// createLabel renders a Create operator: its new nodes then its new
// relationships, each with the labels/type and property assignments it carries.
func createLabel(x *Create) string {
	var parts []string
	for _, n := range x.Nodes {
		parts = append(parts, "("+n.Var+labelSuffix(n.Labels)+propsSuffix(n.Props)+")")
	}
	for _, r := range x.Rels {
		parts = append(parts, "("+r.From+")-["+r.Var+typeSuffix([]bind.NameRef{r.Type})+propsSuffix(r.Props)+"]->("+r.To+")")
	}
	return strings.Join(parts, ", ")
}

// setLabel renders a Set operator's items: a property assignment as
// "var.#key = expr" and a label addition as "var:#tok&#tok".
func setLabel(x *Set) string {
	parts := make([]string, len(x.Items))
	for i, it := range x.Items {
		switch it.Kind {
		case SetItemProp:
			parts[i] = it.Var + "." + tokenLabel(it.Key) + " = " + ast.Print(it.Expr)
		case SetItemLabels:
			parts[i] = it.Var + labelSuffix(it.Labels)
		case SetItemMerge:
			parts[i] = it.Var + " += " + ast.Print(it.Expr)
		case SetItemReplace:
			parts[i] = it.Var + " = " + ast.Print(it.Expr)
		}
	}
	return strings.Join(parts, ", ")
}

// removeLabel renders a Remove operator's items: a property removal as "var.#key"
// and a label removal as "var:#tok&#tok".
func removeLabel(x *Remove) string {
	parts := make([]string, len(x.Items))
	for i, it := range x.Items {
		if len(it.Labels) > 0 {
			parts[i] = it.Var + labelSuffix(it.Labels)
		} else {
			parts[i] = it.Var + "." + tokenLabel(it.Key)
		}
	}
	return strings.Join(parts, ", ")
}

// deleteLabel renders a Delete operator: a DETACH marker when present, then the
// target expressions.
func deleteLabel(x *Delete) string {
	parts := make([]string, len(x.Targets))
	for i, t := range x.Targets {
		parts[i] = ast.Print(t)
	}
	s := strings.Join(parts, ", ")
	if x.Detach {
		return "DETACH " + s
	}
	return s
}

// tokenLabel renders a resolved name as its #token, or #? when unresolved.
func tokenLabel(ref bind.NameRef) string {
	if ref.Known {
		return "#" + itoa(int(ref.Token))
	}
	return "#?"
}

// propsSuffix renders a property-assignment list as {#key: expr, ...}, the key as
// its catalog token (matching the #token rendering of labels and types).
func propsSuffix(props []PropSet) string {
	if len(props) == 0 {
		return ""
	}
	parts := make([]string, len(props))
	for i, p := range props {
		key := "#?"
		if p.Key.Known {
			key = "#" + itoa(int(p.Key.Token))
		}
		parts[i] = key + ": " + ast.Print(p.Expr)
	}
	return " {" + strings.Join(parts, ", ") + "}"
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
