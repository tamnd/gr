// Package ast is the Cypher abstract syntax tree: the purely syntactic tree the
// parser ([parse]) produces and the binder ([10](10-query-pipeline.md) §4, §5)
// resolves (spec 2060 doc 10 §3.2). It records what was written, not what it
// means — a Variable names a variable but does not yet know what it binds, and a
// label is text, not yet a catalog token. Meaning is added downstream.
//
// Every node carries its 1-based source Line and Col (threaded from the tokens)
// so semantic and planning errors can point at the construct that was wrong.
package ast

import "github.com/tamnd/gr/value"

// Pos is a node's 1-based source position. It is embedded in every node so the
// whole tree is positioned.
type Pos struct {
	Line int
	Col  int
}

// Query is one statement: a single query, optionally combined with others by
// UNION. First is the leading part; Rest holds the UNION-joined tails in order.
type Query struct {
	Pos
	First *SingleQuery
	Rest  []UnionTail
}

// UnionTail is a UNION-joined query part. All distinguishes UNION ALL (keep
// duplicates) from UNION (set union, deduplicated).
type UnionTail struct {
	Pos
	All   bool
	Query *SingleQuery
}

// SingleQuery is a clause sequence with no UNION at its top level.
type SingleQuery struct {
	Pos
	Clauses []Clause
}

// Clause is one reading or projecting clause. The marker keeps the set closed.
type Clause interface {
	clauseNode()
}

// Match is a MATCH or OPTIONAL MATCH clause: a set of comma-separated path
// patterns and an optional inline WHERE predicate.
type Match struct {
	Pos
	Optional bool
	Patterns []*PathPattern
	Where    Expr // nil if absent
}

// With is a WITH clause: a projection that pipelines bindings to the next
// clause, with an optional post-projection WHERE.
type With struct {
	Pos
	Projection
	Where Expr // nil if absent
}

// Unwind is an UNWIND clause: expand a list expression into one row per element,
// binding each to Var.
type Unwind struct {
	Pos
	Expr Expr
	Var  string
}

// Return is the terminal RETURN clause: the query's final projection.
type Return struct {
	Pos
	Projection
}

func (*Match) clauseNode()  {}
func (*With) clauseNode()   {}
func (*Unwind) clauseNode() {}
func (*Return) clauseNode() {}

// Projection is the shared body of WITH and RETURN: the projected items (or a
// star), DISTINCT, and the ORDER BY / SKIP / LIMIT tail.
type Projection struct {
	Distinct bool
	Star     bool // RETURN * / WITH *
	Items    []ProjItem
	OrderBy  []SortItem
	Skip     Expr // nil if absent
	Limit    Expr // nil if absent
}

// ProjItem is one projected expression with an optional AS alias.
type ProjItem struct {
	Expr  Expr
	Alias string // "" if no AS clause
}

// SortItem is one ORDER BY key with its direction.
type SortItem struct {
	Expr Expr
	Desc bool
}

// --- patterns (doc 09 §3) ---

// PathPattern is a node, then a chain of relationship-then-node steps, with an
// optional bound path variable.
type PathPattern struct {
	Pos
	Var   string // bound path variable, "" if none
	Start *NodePattern
	Chain []PatternChain
}

// PatternChain is one traversal step: a relationship and the node it reaches.
type PatternChain struct {
	Rel  *RelPattern
	Node *NodePattern
}

// NodePattern is a parenthesized node: an optional variable, a label set, and a
// property-map constraint.
type NodePattern struct {
	Pos
	Var        string
	Labels     []string
	Properties []PropEntry
}

// RelPattern is a bracketed relationship: an optional variable, a type set, a
// direction, an optional property-map constraint, and an optional variable-length
// specifier.
type RelPattern struct {
	Pos
	Var        string
	Types      []string
	Dir        Direction
	Properties []PropEntry
	VarLen     *VarLength // nil for a single hop
}

// Direction is which way a relationship pattern points.
type Direction uint8

const (
	// DirOut is -[]->.
	DirOut Direction = iota
	// DirIn is <-[]-.
	DirIn
	// DirBoth is -[]- (either direction).
	DirBoth
)

// VarLength is a variable-length range: *min..max. A bound of -1 means it was
// omitted (the lower bound defaults to 1 and the upper to unbounded at binding).
type VarLength struct {
	Min int
	Max int
}

// PropEntry is one key:value pair in a property-map constraint or a map literal.
type PropEntry struct {
	Key   string
	Value Expr
}

// --- expressions (doc 09 §6) ---

// Expr is a Cypher expression. The marker keeps the set closed.
type Expr interface {
	exprNode()
}

// Literal is a scalar literal already converted to a value.Value (int, float,
// string, bool, or null).
type Literal struct {
	Pos
	Value value.Value
}

// ListLit is a list literal: [a, b, c].
type ListLit struct {
	Pos
	Elems []Expr
}

// MapLit is a map literal: {k: v, ...}.
type MapLit struct {
	Pos
	Entries []PropEntry
}

// Param is a parameter reference; Name is the name without the leading dollar.
type Param struct {
	Pos
	Name string
}

// Variable is a reference to a bound variable by name.
type Variable struct {
	Pos
	Name string
}

// Property is static property access: Base.Key.
type Property struct {
	Pos
	Base Expr
	Key  string
}

// Index is dynamic element access: Base[Index] (a list index or a map key).
type Index struct {
	Pos
	Base  Expr
	Index Expr
}

// Slice is list slicing: Base[Lo..Hi]. Either bound may be nil (open).
type Slice struct {
	Pos
	Base Expr
	Lo   Expr
	Hi   Expr
}

// Unary is a prefix operation: -x or NOT x.
type Unary struct {
	Pos
	Op UnaryOp
	X  Expr
}

// Binary is an infix operation; see BinaryOp for the operator set.
type Binary struct {
	Pos
	Op   BinaryOp
	L, R Expr
}

// IsNull is the postfix x IS NULL / x IS NOT NULL test.
type IsNull struct {
	Pos
	X      Expr
	Negate bool // true for IS NOT NULL
}

// FunctionCall is a call: Name(args), with an optional DISTINCT (count(DISTINCT x)).
type FunctionCall struct {
	Pos
	Name     string
	Distinct bool
	Args     []Expr
	Star     bool // count(*)
}

// Case is a CASE expression. Subject is nil for the searched form (CASE WHEN …).
type Case struct {
	Pos
	Subject Expr
	Whens   []WhenThen
	Else    Expr // nil if no ELSE
}

// WhenThen is one branch of a CASE.
type WhenThen struct {
	When Expr
	Then Expr
}

func (*Literal) exprNode()      {}
func (*ListLit) exprNode()      {}
func (*MapLit) exprNode()       {}
func (*Param) exprNode()        {}
func (*Variable) exprNode()     {}
func (*Property) exprNode()     {}
func (*Index) exprNode()        {}
func (*Slice) exprNode()        {}
func (*Unary) exprNode()        {}
func (*Binary) exprNode()       {}
func (*IsNull) exprNode()       {}
func (*FunctionCall) exprNode() {}
func (*Case) exprNode()         {}

// UnaryOp is a prefix operator.
type UnaryOp uint8

const (
	// OpNeg is unary minus.
	OpNeg UnaryOp = iota
	// OpNot is logical NOT.
	OpNot
)

// BinaryOp is an infix operator.
type BinaryOp uint8

const (
	// Arithmetic and concatenation.
	OpAdd BinaryOp = iota // + (numeric add, string/list concat)
	OpSub                 // -
	OpMul                 // *
	OpDiv                 // /
	OpMod                 // %
	OpPow                 // ^

	// Comparison.
	OpEq // =
	OpNe // <>
	OpLt // <
	OpLe // <=
	OpGt // >
	OpGe // >=

	// Boolean.
	OpAnd
	OpOr
	OpXor

	// String predicates.
	OpStartsWith
	OpEndsWith
	OpContains
	OpRegex // =~

	// List membership.
	OpIn
)

// opNames gives each BinaryOp a label for diagnostics and pretty-printing.
var opNames = map[BinaryOp]string{
	OpAdd: "+", OpSub: "-", OpMul: "*", OpDiv: "/", OpMod: "%", OpPow: "^",
	OpEq: "=", OpNe: "<>", OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">=",
	OpAnd: "AND", OpOr: "OR", OpXor: "XOR",
	OpStartsWith: "STARTS WITH", OpEndsWith: "ENDS WITH", OpContains: "CONTAINS",
	OpRegex: "=~", OpIn: "IN",
}

// String returns the operator's source spelling.
func (o BinaryOp) String() string { return opNames[o] }
