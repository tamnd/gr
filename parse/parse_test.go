package parse_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/parse"
	"github.com/tamnd/gr/value"
)

// sexpr renders an AST as a parenthesized string so tests can assert structure
// with a single string compare. It is deliberately compact and total over the
// node set the parser produces.
func sexpr(n any) string {
	switch x := n.(type) {
	case *ast.Query:
		var b strings.Builder
		b.WriteString(sexpr(x.First))
		for _, u := range x.Rest {
			kw := "union"
			if u.All {
				kw = "union-all"
			}
			b.WriteString(" " + kw + " " + sexpr(u.Query))
		}
		return b.String()
	case *ast.SingleQuery:
		parts := make([]string, len(x.Clauses))
		for i, c := range x.Clauses {
			parts[i] = sexpr(c)
		}
		return "(query " + strings.Join(parts, " ") + ")"
	case *ast.Match:
		s := "(match"
		if x.Optional {
			s = "(optional-match"
		}
		for _, pp := range x.Patterns {
			s += " " + sexpr(pp)
		}
		if x.Where != nil {
			s += " (where " + sexpr(x.Where) + ")"
		}
		return s + ")"
	case *ast.With:
		return "(with " + projection(x.Projection) + whereSuffix(x.Where) + ")"
	case *ast.Return:
		return "(return " + projection(x.Projection) + ")"
	case *ast.Unwind:
		return "(unwind " + sexpr(x.Expr) + " as " + x.Var + ")"
	case *ast.PathPattern:
		s := "(path"
		if x.Var != "" {
			s += " " + x.Var + "="
		}
		s += " " + sexpr(x.Start)
		for _, ch := range x.Chain {
			s += " " + sexpr(ch.Rel) + " " + sexpr(ch.Node)
		}
		return s + ")"
	case *ast.NodePattern:
		s := "(node"
		if x.Var != "" {
			s += " " + x.Var
		}
		for _, l := range x.Labels {
			s += " :" + l
		}
		s += props(x.Properties)
		return s + ")"
	case *ast.RelPattern:
		dir := map[ast.Direction]string{ast.DirOut: "->", ast.DirIn: "<-", ast.DirBoth: "--"}[x.Dir]
		s := "(rel " + dir
		if x.Var != "" {
			s += " " + x.Var
		}
		for _, t := range x.Types {
			s += " :" + t
		}
		if x.VarLen != nil {
			s += " *" + strconv.Itoa(x.VarLen.Min) + ".." + strconv.Itoa(x.VarLen.Max)
		}
		s += props(x.Properties)
		return s + ")"
	case *ast.Literal:
		return litString(x.Value)
	case *ast.ListLit:
		parts := make([]string, len(x.Elems))
		for i, e := range x.Elems {
			parts[i] = sexpr(e)
		}
		return "(list " + strings.Join(parts, " ") + ")"
	case *ast.MapLit:
		return "(map" + props(x.Entries) + ")"
	case *ast.Param:
		return "$" + x.Name
	case *ast.Variable:
		return x.Name
	case *ast.Property:
		return "(. " + sexpr(x.Base) + " " + x.Key + ")"
	case *ast.Index:
		return "(index " + sexpr(x.Base) + " " + sexpr(x.Index) + ")"
	case *ast.Slice:
		return "(slice " + sexpr(x.Base) + " " + optExpr(x.Lo) + " " + optExpr(x.Hi) + ")"
	case *ast.Unary:
		op := map[ast.UnaryOp]string{ast.OpNeg: "neg", ast.OpNot: "not"}[x.Op]
		return "(" + op + " " + sexpr(x.X) + ")"
	case *ast.Binary:
		return "(" + x.Op.String() + " " + sexpr(x.L) + " " + sexpr(x.R) + ")"
	case *ast.IsNull:
		op := "is-null"
		if x.Negate {
			op = "is-not-null"
		}
		return "(" + op + " " + sexpr(x.X) + ")"
	case *ast.FunctionCall:
		s := "(call " + x.Name
		if x.Distinct {
			s += " distinct"
		}
		if x.Star {
			s += " *"
		}
		for _, a := range x.Args {
			s += " " + sexpr(a)
		}
		return s + ")"
	case *ast.Case:
		s := "(case"
		if x.Subject != nil {
			s += " " + sexpr(x.Subject)
		}
		for _, w := range x.Whens {
			s += " (when " + sexpr(w.When) + " " + sexpr(w.Then) + ")"
		}
		if x.Else != nil {
			s += " (else " + sexpr(x.Else) + ")"
		}
		return s + ")"
	default:
		return "?"
	}
}

func projection(p ast.Projection) string {
	s := ""
	if p.Distinct {
		s += "distinct "
	}
	if p.Star {
		s += "* "
	}
	items := make([]string, len(p.Items))
	for i, it := range p.Items {
		items[i] = sexpr(it.Expr)
		if it.Alias != "" {
			items[i] += " as " + it.Alias
		}
	}
	s += strings.Join(items, ", ")
	for _, o := range p.OrderBy {
		dir := "asc"
		if o.Desc {
			dir = "desc"
		}
		s += " order(" + sexpr(o.Expr) + " " + dir + ")"
	}
	if p.Skip != nil {
		s += " skip(" + sexpr(p.Skip) + ")"
	}
	if p.Limit != nil {
		s += " limit(" + sexpr(p.Limit) + ")"
	}
	return strings.TrimSpace(s)
}

func whereSuffix(e ast.Expr) string {
	if e == nil {
		return ""
	}
	return " (where " + sexpr(e) + ")"
}

func props(entries []ast.PropEntry) string {
	if len(entries) == 0 {
		return ""
	}
	s := " {"
	for i, e := range entries {
		if i > 0 {
			s += " "
		}
		s += e.Key + ":" + sexpr(e.Value)
	}
	return s + "}"
}

func optExpr(e ast.Expr) string {
	if e == nil {
		return "_"
	}
	return sexpr(e)
}

func litString(v value.Value) string {
	switch v.Type() {
	case value.TypeNull:
		return "null"
	case value.TypeBool:
		b, _ := v.AsBool()
		return strconv.FormatBool(b)
	case value.TypeInt:
		n, _ := v.AsInt()
		return strconv.FormatInt(n, 10)
	case value.TypeFloat:
		f, _ := v.AsFloat()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case value.TypeString:
		s, _ := v.AsString()
		return "'" + s + "'"
	default:
		return "?lit"
	}
}

func mustParse(t *testing.T, src string) *ast.Query {
	t.Helper()
	q, err := parse.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return q
}

func check(t *testing.T, src, want string) {
	t.Helper()
	got := sexpr(mustParse(t, src))
	if got != want {
		t.Fatalf("Parse(%q)\n got: %s\nwant: %s", src, got, want)
	}
}

// TestFriendsOfFriends parses the M2 demo query and checks the whole tree.
func TestFriendsOfFriends(t *testing.T) {
	src := `MATCH (a:Person {name:$name})-[:KNOWS]->(:Person)-[r:KNOWS]->(fof:Person)
	        WHERE fof <> a
	        RETURN DISTINCT fof.name AS suggestion`
	want := "(query (match (path (node a :Person {name:$name}) " +
		"(rel -> :KNOWS) (node :Person) (rel -> r :KNOWS) (node fof :Person)) " +
		"(where (<> fof a))) " +
		"(return distinct (. fof name) as suggestion))"
	check(t, src, want)
}

// TestPrecedence checks the expression grammar binds operators correctly.
func TestPrecedence(t *testing.T) {
	check(t, "RETURN 1 + 2 * 3", "(query (return (+ 1 (* 2 3))))")
	check(t, "RETURN (1 + 2) * 3", "(query (return (* (+ 1 2) 3)))")
	check(t, "RETURN 2 ^ 3 ^ 2", "(query (return (^ 2 (^ 3 2))))")
	check(t, "RETURN a OR b AND c", "(query (return (OR a (AND b c))))")
	check(t, "RETURN NOT a = b", "(query (return (not (= a b))))")
	check(t, "RETURN -a + b", "(query (return (+ (neg a) b)))")
}

// TestComparisonsAndPredicates covers the comparison tier operators.
func TestComparisonsAndPredicates(t *testing.T) {
	check(t, "RETURN a STARTS WITH 'x'", "(query (return (STARTS WITH a 'x')))")
	check(t, "RETURN a ENDS WITH 'x'", "(query (return (ENDS WITH a 'x')))")
	check(t, "RETURN a CONTAINS 'x'", "(query (return (CONTAINS a 'x')))")
	check(t, "RETURN a IN [1, 2, 3]", "(query (return (IN a (list 1 2 3))))")
	check(t, "RETURN a IS NULL", "(query (return (is-null a)))")
	check(t, "RETURN a IS NOT NULL", "(query (return (is-not-null a)))")
}

// TestVarLength covers the variable-length range forms.
func TestVarLength(t *testing.T) {
	check(t, "MATCH (a)-[:KNOWS*1..3]->(b) RETURN b",
		"(query (match (path (node a) (rel -> :KNOWS *1..3) (node b))) (return b))")
	check(t, "MATCH (a)-[:KNOWS*]->(b) RETURN b",
		"(query (match (path (node a) (rel -> :KNOWS *-1..-1) (node b))) (return b))")
	check(t, "MATCH (a)-[:KNOWS*2]->(b) RETURN b",
		"(query (match (path (node a) (rel -> :KNOWS *2..2) (node b))) (return b))")
	check(t, "MATCH (a)-[:KNOWS*2..]->(b) RETURN b",
		"(query (match (path (node a) (rel -> :KNOWS *2..-1) (node b))) (return b))")
	check(t, "MATCH (a)-[:KNOWS*..3]->(b) RETURN b",
		"(query (match (path (node a) (rel -> :KNOWS *-1..3) (node b))) (return b))")
}

// TestDirections covers all three relationship directions and the type union.
func TestDirections(t *testing.T) {
	check(t, "MATCH (a)<-[:KNOWS|FOLLOWS]-(b) RETURN b",
		"(query (match (path (node a) (rel <- :KNOWS :FOLLOWS) (node b))) (return b))")
	check(t, "MATCH (a)-[:KNOWS]-(b) RETURN b",
		"(query (match (path (node a) (rel -- :KNOWS) (node b))) (return b))")
	check(t, "MATCH (a)-->(b) RETURN b",
		"(query (match (path (node a) (rel ->) (node b))) (return b))")
}

// TestProjectionTail covers DISTINCT, *, ORDER BY, SKIP, LIMIT, and aliases.
func TestProjectionTail(t *testing.T) {
	check(t, "MATCH (p) RETURN p ORDER BY p.age DESC, p.name SKIP 10 LIMIT 5",
		"(query (match (path (node p))) (return p order((. p age) desc) order((. p name) asc) skip(10) limit(5)))")
	check(t, "MATCH (p) RETURN *", "(query (match (path (node p))) (return *))")
	check(t, "MATCH (p)-[:KNOWS]->(f) WITH p, count(f) AS friends WHERE friends > 5 RETURN p",
		"(query (match (path (node p) (rel -> :KNOWS) (node f))) "+
			"(with p, (call count f) as friends (where (> friends 5))) (return p))")
}

// TestLiteralsAndAccess covers list/map literals, indexing, slicing, and CASE.
func TestLiteralsAndAccess(t *testing.T) {
	check(t, "RETURN [1, 2][0]", "(query (return (index (list 1 2) 0)))")
	check(t, "RETURN list[1..3]", "(query (return (slice list 1 3)))")
	check(t, "RETURN list[..2]", "(query (return (slice list _ 2)))")
	check(t, "RETURN {a: 1, b: 'x'}", "(query (return (map {a:1 b:'x'})))")
	check(t, "RETURN count(*)", "(query (return (call count *)))")
	check(t, "RETURN count(DISTINCT p.city)", "(query (return (call count distinct (. p city))))")
	check(t, "RETURN CASE WHEN a > 1 THEN 'big' ELSE 'small' END",
		"(query (return (case (when (> a 1) 'big') (else 'small'))))")
	check(t, "RETURN CASE x WHEN 1 THEN 'one' END",
		"(query (return (case x (when 1 'one'))))")
}

// TestUnwindAndUnion covers UNWIND and the UNION combinators.
func TestUnwindAndUnion(t *testing.T) {
	check(t, "UNWIND [1, 2, 3] AS x RETURN x",
		"(query (unwind (list 1 2 3) as x) (return x))")
	check(t, "MATCH (p:Person) RETURN p.name UNION MATCH (c:Company) RETURN c.name",
		"(query (match (path (node p :Person))) (return (. p name))) union "+
			"(query (match (path (node c :Company))) (return (. c name)))")
	check(t, "MATCH (a) RETURN a UNION ALL MATCH (b) RETURN b",
		"(query (match (path (node a))) (return a)) union-all (query (match (path (node b))) (return b))")
}

// TestParseErrors confirms malformed queries are rejected with a parse.Error.
func TestParseErrors(t *testing.T) {
	bad := []string{
		"MATCH (a",                       // missing )
		"MATCH (a)-[:KNOWS]->",           // missing target node
		"RETURN",                         // empty projection
		"MATCH (a) WHERE RETURN a",       // missing predicate
		"CREATE (a)",                     // write clause (M2)
		"MATCH (a) SET a.x = 1 RETURN a", // write clause (M2)
		"MATCH (a)<-[:T]->(b) RETURN b",  // both directions
		"RETURN 1 +",                     // dangling operator
		"FOO bar",                        // not a clause
	}
	for _, src := range bad {
		if _, err := parse.Parse(src); err == nil {
			t.Fatalf("Parse(%q): expected an error, got none", src)
		}
	}
}
