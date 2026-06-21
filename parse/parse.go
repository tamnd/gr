// Package parse is the Cypher parser: it turns the lexer's token stream into an
// abstract syntax tree ([ast]) for the read subset of the grammar (spec 2060
// doc 10 §3; doc 09 §10). It is a hand-written recursive-descent parser with a
// precedence-stratified expression grammar, the standard shape for a language
// this size ([21](21-go-runtime-engineering.md)).
//
// The parser reports the first syntactic error with its position and what was
// expected, and stops — it does not attempt multi-error recovery (doc 10 §3.3),
// which keeps the parser simple and its errors clear. The AST it produces is
// purely syntactic; meaning is added by the binder ([10](10-query-pipeline.md)
// §4, §5), the next M2 stage.
package parse

import (
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/lex"
)

// Error is a syntactic error: a message plus the position it was found at.
type Error struct {
	Msg  string
	Line int
	Col  int
}

func (e *Error) Error() string {
	return "parse: " + e.Msg + " at line " + itoa(e.Line) + ":" + itoa(e.Col)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// parser holds the token stream and a cursor.
type parser struct {
	toks []lex.Token
	i    int
}

// Parse lexes and parses src into a Query, returning the first lexical or
// syntactic error.
func Parse(src string) (*ast.Query, error) {
	toks, err := lex.Tokens(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	q, err := p.query()
	if err != nil {
		return nil, err
	}
	if p.cur().Kind != lex.EOF {
		return nil, p.errAt(p.cur(), "unexpected "+p.cur().Kind.String()+" after end of query")
	}
	return q, nil
}

// --- cursor helpers ---

func (p *parser) cur() lex.Token { return p.toks[p.i] }

func (p *parser) at(k lex.Kind) bool { return p.toks[p.i].Kind == k }

func (p *parser) peek(n int) lex.Token {
	j := p.i + n
	if j >= len(p.toks) {
		return p.toks[len(p.toks)-1] // EOF
	}
	return p.toks[j]
}

func (p *parser) advance() lex.Token {
	t := p.toks[p.i]
	if t.Kind != lex.EOF {
		p.i++
	}
	return t
}

// accept consumes the current token if it matches k, reporting whether it did.
func (p *parser) accept(k lex.Kind) bool {
	if p.at(k) {
		p.i++
		return true
	}
	return false
}

// expect consumes the current token, requiring it to match k.
func (p *parser) expect(k lex.Kind) (lex.Token, error) {
	if !p.at(k) {
		return lex.Token{}, p.errAt(p.cur(), "expected "+k.String()+", found "+p.cur().Kind.String())
	}
	return p.advance(), nil
}

func (p *parser) errAt(t lex.Token, msg string) error {
	return &Error{Msg: msg, Line: t.Line, Col: t.Col}
}

// wordIs reports whether a token is a bare identifier matching s case-insensitively.
// It lets the parser recognize the soft keywords of the schema grammar (CONSTRAINT,
// FOR, REQUIRE, UNIQUE, DROP, IF, EXISTS) without reserving them, so a query can
// still use those words as variable, label, or property names.
func (p *parser) wordIs(t lex.Token, s string) bool {
	return t.Kind == lex.Ident && strings.EqualFold(t.Text, s)
}

func (p *parser) atWord(s string) bool { return p.wordIs(p.cur(), s) }

func (p *parser) acceptWord(s string) bool {
	if p.atWord(s) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectWord(s string) (lex.Token, error) {
	if !p.atWord(s) {
		return lex.Token{}, p.errAt(p.cur(), "expected "+s+", found "+p.cur().Kind.String())
	}
	return p.advance(), nil
}

// pos turns a token into an AST position.
func pos(t lex.Token) ast.Pos { return ast.Pos{Line: t.Line, Col: t.Col} }

// --- query and clauses ---

func (p *parser) query() (*ast.Query, error) {
	if p.at(lex.Create) && p.wordIs(p.peek(1), "CONSTRAINT") {
		return p.createConstraint()
	}
	if p.atWord("DROP") && p.wordIs(p.peek(1), "CONSTRAINT") {
		return p.dropConstraint()
	}
	if p.at(lex.Create) && p.wordIs(p.peek(1), "INDEX") {
		return p.createIndex()
	}
	if p.atWord("DROP") && p.wordIs(p.peek(1), "INDEX") {
		return p.dropIndex()
	}
	start := p.cur()
	first, err := p.singleQuery()
	if err != nil {
		return nil, err
	}
	q := &ast.Query{Pos: pos(start), First: first}
	for p.at(lex.Union) {
		u := p.advance()
		all := p.accept(lex.All)
		sq, err := p.singleQuery()
		if err != nil {
			return nil, err
		}
		q.Rest = append(q.Rest, ast.UnionTail{Pos: pos(u), All: all, Query: sq})
	}
	return q, nil
}

func (p *parser) singleQuery() (*ast.SingleQuery, error) {
	sq := &ast.SingleQuery{Pos: pos(p.cur())}
	for !p.at(lex.EOF) && !p.at(lex.Union) {
		var (
			c   ast.Clause
			err error
		)
		switch p.cur().Kind {
		case lex.Match, lex.Optional:
			c, err = p.matchClause()
		case lex.With:
			c, err = p.withClause()
		case lex.Unwind:
			c, err = p.unwindClause()
		case lex.Return:
			c, err = p.returnClause()
		case lex.Create:
			c, err = p.createClause()
		case lex.Set:
			c, err = p.setClause()
		case lex.Remove:
			c, err = p.removeClause()
		case lex.Delete, lex.Detach:
			c, err = p.deleteClause()
		case lex.Merge:
			c, err = p.mergeClause()
		case lex.Foreach:
			c, err = p.foreachClause()
		default:
			return nil, p.errAt(p.cur(), "expected a clause (MATCH, OPTIONAL MATCH, WITH, UNWIND, RETURN), found "+p.cur().Kind.String())
		}
		if err != nil {
			return nil, err
		}
		sq.Clauses = append(sq.Clauses, c)
		if _, isReturn := c.(*ast.Return); isReturn {
			break // RETURN is terminal within a single query
		}
	}
	if len(sq.Clauses) == 0 {
		return nil, p.errAt(p.cur(), "expected a query")
	}
	return sq, nil
}

func (p *parser) matchClause() (*ast.Match, error) {
	start := p.cur()
	m := &ast.Match{Pos: pos(start)}
	if p.accept(lex.Optional) {
		m.Optional = true
	}
	if _, err := p.expect(lex.Match); err != nil {
		return nil, err
	}
	for {
		pp, err := p.pathPattern()
		if err != nil {
			return nil, err
		}
		m.Patterns = append(m.Patterns, pp)
		if !p.accept(lex.Comma) {
			break
		}
	}
	if p.accept(lex.Where) {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}
		m.Where = w
	}
	return m, nil
}

func (p *parser) withClause() (*ast.With, error) {
	start := p.cur()
	p.advance() // WITH
	w := &ast.With{Pos: pos(start)}
	if err := p.projection(&w.Projection); err != nil {
		return nil, err
	}
	if p.accept(lex.Where) {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		w.Where = e
	}
	return w, nil
}

func (p *parser) unwindClause() (*ast.Unwind, error) {
	start := p.cur()
	p.advance() // UNWIND
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lex.As); err != nil {
		return nil, err
	}
	v, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	return &ast.Unwind{Pos: pos(start), Expr: e, Var: v.Text}, nil
}

// createClause parses CREATE followed by one or more comma-separated path
// patterns. The patterns reuse the same grammar as MATCH; the binder rejects the
// forms CREATE cannot express (a variable-length step, an undirected
// relationship, a shortestPath wrapper).
func (p *parser) createClause() (*ast.Create, error) {
	start := p.cur()
	p.advance() // CREATE
	c := &ast.Create{Pos: pos(start)}
	for {
		pp, err := p.pathPattern()
		if err != nil {
			return nil, err
		}
		c.Patterns = append(c.Patterns, pp)
		if !p.accept(lex.Comma) {
			break
		}
	}
	return c, nil
}

// mergeClause parses MERGE followed by a single path pattern, then zero or more
// ON CREATE SET / ON MATCH SET sub-clauses in any order. The pattern reuses the
// MATCH grammar; the binder rejects the forms MERGE cannot express (the same set
// CREATE rejects). Each sub-clause's items reuse the SET item grammar.
func (p *parser) mergeClause() (*ast.Merge, error) {
	start := p.cur()
	p.advance() // MERGE
	pp, err := p.pathPattern()
	if err != nil {
		return nil, err
	}
	m := &ast.Merge{Pos: pos(start), Pattern: pp}
	for p.at(lex.On) {
		p.advance() // ON
		switch {
		case p.at(lex.Create):
			p.advance() // CREATE
			items, err := p.mergeSetItems()
			if err != nil {
				return nil, err
			}
			m.OnCreate = append(m.OnCreate, items...)
		case p.at(lex.Match):
			p.advance() // MATCH
			items, err := p.mergeSetItems()
			if err != nil {
				return nil, err
			}
			m.OnMatch = append(m.OnMatch, items...)
		default:
			return nil, p.errAt(p.cur(), "expected CREATE or MATCH after ON, found "+p.cur().Kind.String())
		}
	}
	return m, nil
}

// foreachClause parses FOREACH ( var IN expr | writes ), the write-only loop.
// The body is one or more write clauses, parsed up to the closing parenthesis;
// a read clause inside the body is rejected, because FOREACH is write-only.
func (p *parser) foreachClause() (*ast.Foreach, error) {
	start := p.cur()
	p.advance() // FOREACH
	if _, err := p.expect(lex.Lparen); err != nil {
		return nil, err
	}
	v, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lex.In); err != nil {
		return nil, err
	}
	list, err := p.expr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lex.Pipe); err != nil {
		return nil, err
	}
	f := &ast.Foreach{Pos: pos(start), Var: v.Text, List: list}
	for !p.at(lex.Rparen) {
		if p.at(lex.EOF) {
			return nil, p.errAt(p.cur(), "expected a write clause or ')' in FOREACH, found end of input")
		}
		c, err := p.foreachBodyClause()
		if err != nil {
			return nil, err
		}
		f.Body = append(f.Body, c)
	}
	if _, err := p.expect(lex.Rparen); err != nil {
		return nil, err
	}
	if len(f.Body) == 0 {
		return nil, p.errAt(p.cur(), "FOREACH body must contain at least one write clause")
	}
	return f, nil
}

// foreachBodyClause parses one clause inside a FOREACH body, accepting only the
// write clauses (CREATE, MERGE, SET, REMOVE, DELETE/DETACH DELETE, and nested
// FOREACH) and rejecting the read clauses, since a FOREACH body is write-only.
func (p *parser) foreachBodyClause() (ast.Clause, error) {
	switch p.cur().Kind {
	case lex.Create:
		return p.createClause()
	case lex.Merge:
		return p.mergeClause()
	case lex.Set:
		return p.setClause()
	case lex.Remove:
		return p.removeClause()
	case lex.Delete, lex.Detach:
		return p.deleteClause()
	case lex.Foreach:
		return p.foreachClause()
	default:
		return nil, p.errAt(p.cur(), "FOREACH body allows only write clauses, found "+p.cur().Kind.String())
	}
}

// mergeSetItems parses the SET keyword and its comma-separated items, the body
// of an ON CREATE / ON MATCH sub-clause.
func (p *parser) mergeSetItems() ([]ast.SetItem, error) {
	if _, err := p.expect(lex.Set); err != nil {
		return nil, err
	}
	var items []ast.SetItem
	for {
		it, err := p.setItem()
		if err != nil {
			return nil, err
		}
		items = append(items, it)
		if !p.accept(lex.Comma) {
			break
		}
	}
	return items, nil
}

// setClause parses SET followed by one or more comma-separated update items.
// Each item starts with a bound variable and is one of: a property assignment
// (n.k = e), a map merge (n += e), a map replace (n = e), or a label addition
// (n:A:B). The +=/= forms are told apart by what follows the variable.
func (p *parser) setClause() (*ast.Set, error) {
	start := p.cur()
	p.advance() // SET
	s := &ast.Set{Pos: pos(start)}
	for {
		it, err := p.setItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, it)
		if !p.accept(lex.Comma) {
			break
		}
	}
	return s, nil
}

func (p *parser) setItem() (ast.SetItem, error) {
	v, err := p.expect(lex.Ident)
	if err != nil {
		return ast.SetItem{}, err
	}
	it := ast.SetItem{Pos: pos(v), Var: v.Text}
	switch {
	case p.at(lex.Dot):
		p.advance() // .
		key, err := p.expect(lex.Ident)
		if err != nil {
			return ast.SetItem{}, err
		}
		if _, err := p.expect(lex.Eq); err != nil {
			return ast.SetItem{}, err
		}
		e, err := p.expr()
		if err != nil {
			return ast.SetItem{}, err
		}
		it.Op = ast.SetProperty
		it.Key = key.Text
		it.Value = e
	case p.at(lex.Colon):
		labels, err := p.labelList()
		if err != nil {
			return ast.SetItem{}, err
		}
		it.Op = ast.SetLabels
		it.Labels = labels
	case p.at(lex.Plus) && p.peek(1).Kind == lex.Eq:
		p.advance() // +
		p.advance() // =
		e, err := p.expr()
		if err != nil {
			return ast.SetItem{}, err
		}
		it.Op = ast.SetMerge
		it.Value = e
	case p.at(lex.Eq):
		p.advance() // =
		e, err := p.expr()
		if err != nil {
			return ast.SetItem{}, err
		}
		it.Op = ast.SetReplace
		it.Value = e
	default:
		return ast.SetItem{}, p.errAt(p.cur(), "expected '.', ':', '=' or '+=' after a SET variable, found "+p.cur().Kind.String())
	}
	return it, nil
}

// deleteClause parses an optional DETACH, then DELETE, then one or more
// comma-separated expressions naming the elements to delete.
func (p *parser) deleteClause() (*ast.Delete, error) {
	start := p.cur()
	d := &ast.Delete{Pos: pos(start)}
	if p.accept(lex.Detach) {
		d.Detach = true
	}
	if _, err := p.expect(lex.Delete); err != nil {
		return nil, err
	}
	for {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		d.Targets = append(d.Targets, e)
		if !p.accept(lex.Comma) {
			break
		}
	}
	return d, nil
}

// removeClause parses REMOVE followed by one or more comma-separated targets,
// each a property (n.k) or one or more labels (n:A:B).
func (p *parser) removeClause() (*ast.Remove, error) {
	start := p.cur()
	p.advance() // REMOVE
	r := &ast.Remove{Pos: pos(start)}
	for {
		it, err := p.removeItem()
		if err != nil {
			return nil, err
		}
		r.Items = append(r.Items, it)
		if !p.accept(lex.Comma) {
			break
		}
	}
	return r, nil
}

func (p *parser) removeItem() (ast.RemoveItem, error) {
	v, err := p.expect(lex.Ident)
	if err != nil {
		return ast.RemoveItem{}, err
	}
	it := ast.RemoveItem{Pos: pos(v), Var: v.Text}
	switch {
	case p.at(lex.Dot):
		p.advance() // .
		key, err := p.expect(lex.Ident)
		if err != nil {
			return ast.RemoveItem{}, err
		}
		it.Key = key.Text
	case p.at(lex.Colon):
		labels, err := p.labelList()
		if err != nil {
			return ast.RemoveItem{}, err
		}
		it.Labels = labels
	default:
		return ast.RemoveItem{}, p.errAt(p.cur(), "expected '.' or ':' after a REMOVE variable, found "+p.cur().Kind.String())
	}
	return it, nil
}

// labelList parses a colon-prefixed label chain (:A:B:C), at least one label.
func (p *parser) labelList() ([]string, error) {
	var labels []string
	for p.accept(lex.Colon) {
		l, err := p.expect(lex.Ident)
		if err != nil {
			return nil, err
		}
		labels = append(labels, l.Text)
	}
	return labels, nil
}

func (p *parser) returnClause() (*ast.Return, error) {
	start := p.cur()
	p.advance() // RETURN
	r := &ast.Return{Pos: pos(start)}
	if err := p.projection(&r.Projection); err != nil {
		return nil, err
	}
	return r, nil
}

// projection parses the shared WITH/RETURN body: DISTINCT, the items or a star,
// then ORDER BY / SKIP / LIMIT.
func (p *parser) projection(proj *ast.Projection) error {
	proj.Distinct = p.accept(lex.Distinct)
	if p.accept(lex.Star) {
		proj.Star = true
		if !p.accept(lex.Comma) {
			return p.tail(proj) // RETURN * with nothing further
		}
	}
	for {
		it, err := p.projItem()
		if err != nil {
			return err
		}
		proj.Items = append(proj.Items, it)
		if !p.accept(lex.Comma) {
			break
		}
	}
	return p.tail(proj)
}

func (p *parser) projItem() (ast.ProjItem, error) {
	e, err := p.expr()
	if err != nil {
		return ast.ProjItem{}, err
	}
	it := ast.ProjItem{Expr: e}
	if p.accept(lex.As) {
		name, err := p.expect(lex.Ident)
		if err != nil {
			return ast.ProjItem{}, err
		}
		it.Alias = name.Text
	}
	return it, nil
}

// tail parses the optional ORDER BY / SKIP / LIMIT after the projection items.
func (p *parser) tail(proj *ast.Projection) error {
	if p.accept(lex.Order) {
		if _, err := p.expect(lex.By); err != nil {
			return err
		}
		for {
			e, err := p.expr()
			if err != nil {
				return err
			}
			si := ast.SortItem{Expr: e}
			switch {
			case p.accept(lex.Asc):
			case p.accept(lex.Desc):
				si.Desc = true
			case p.at(lex.Ident):
				switch strings.ToUpper(p.cur().Text) {
				case "ASCENDING":
					p.advance()
				case "DESCENDING":
					si.Desc = true
					p.advance()
				}
			}
			proj.OrderBy = append(proj.OrderBy, si)
			if !p.accept(lex.Comma) {
				break
			}
		}
	}
	if p.accept(lex.Skip) {
		e, err := p.expr()
		if err != nil {
			return err
		}
		proj.Skip = e
	}
	if p.accept(lex.Limit) {
		e, err := p.expr()
		if err != nil {
			return err
		}
		proj.Limit = e
	}
	return nil
}

// --- patterns ---

func (p *parser) pathPattern() (*ast.PathPattern, error) {
	pp := &ast.PathPattern{Pos: pos(p.cur())}
	// An optional bound path variable: `path = (...)`.
	if p.at(lex.Ident) && p.peek(1).Kind == lex.Eq {
		pp.Var = p.advance().Text
		p.advance() // =
	}
	// shortestPath(...) / allShortestPaths(...) wrap a pattern in parentheses.
	if k := p.shortestKind(); k != ast.NotShortest {
		pp.Shortest = k
		p.advance() // the function name
		if _, err := p.expect(lex.Lparen); err != nil {
			return nil, err
		}
		if err := p.pathBody(pp); err != nil {
			return nil, err
		}
		if _, err := p.expect(lex.Rparen); err != nil {
			return nil, err
		}
		return pp, nil
	}
	if err := p.pathBody(pp); err != nil {
		return nil, err
	}
	return pp, nil
}

// shortestKind recognizes a shortest-path wrapper: an identifier naming one of
// the shortest-path functions immediately followed by an opening parenthesis.
// These are ordinary identifiers to the lexer; only this position gives them
// meaning, so a node or relationship variable named "shortestpath" elsewhere is
// unaffected.
func (p *parser) shortestKind() ast.ShortestKind {
	if !p.at(lex.Ident) || p.peek(1).Kind != lex.Lparen {
		return ast.NotShortest
	}
	switch strings.ToLower(p.cur().Text) {
	case "shortestpath":
		return ast.ShortestOne
	case "allshortestpaths":
		return ast.ShortestAll
	}
	return ast.NotShortest
}

// pathBody parses a pattern's node and its chain of relationship-then-node steps,
// the shape shared by a bare pattern and one wrapped in a shortest-path function.
func (p *parser) pathBody(pp *ast.PathPattern) error {
	n, err := p.nodePattern()
	if err != nil {
		return err
	}
	pp.Start = n
	for p.at(lex.Minus) || p.at(lex.Lt) {
		rel, err := p.relPattern()
		if err != nil {
			return err
		}
		node, err := p.nodePattern()
		if err != nil {
			return err
		}
		pp.Chain = append(pp.Chain, ast.PatternChain{Rel: rel, Node: node})
	}
	return nil
}

func (p *parser) nodePattern() (*ast.NodePattern, error) {
	start, err := p.expect(lex.Lparen)
	if err != nil {
		return nil, err
	}
	n := &ast.NodePattern{Pos: pos(start)}
	if p.at(lex.Ident) {
		n.Var = p.advance().Text
	}
	for p.accept(lex.Colon) {
		lbl, err := p.expect(lex.Ident)
		if err != nil {
			return nil, err
		}
		n.Labels = append(n.Labels, lbl.Text)
	}
	if p.at(lex.Lbrace) {
		props, err := p.propertyMap()
		if err != nil {
			return nil, err
		}
		n.Properties = props
	}
	if _, err := p.expect(lex.Rparen); err != nil {
		return nil, err
	}
	return n, nil
}

func (p *parser) relPattern() (*ast.RelPattern, error) {
	start := p.cur()
	r := &ast.RelPattern{Pos: pos(start), Dir: ast.DirBoth}
	left := p.accept(lex.Lt)
	if _, err := p.expect(lex.Minus); err != nil {
		return nil, err
	}
	if p.at(lex.Lbracket) {
		if err := p.relDetail(r); err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(lex.Minus); err != nil {
		return nil, err
	}
	right := p.accept(lex.Gt)
	switch {
	case left && right:
		return nil, p.errAt(start, "a relationship pattern cannot point in both directions")
	case left:
		r.Dir = ast.DirIn
	case right:
		r.Dir = ast.DirOut
	default:
		r.Dir = ast.DirBoth
	}
	return r, nil
}

// relDetail parses the bracketed body of a relationship: variable, type set,
// variable-length specifier, and property-map constraint.
func (p *parser) relDetail(r *ast.RelPattern) error {
	p.advance() // [
	if p.at(lex.Ident) {
		r.Var = p.advance().Text
	}
	if p.accept(lex.Colon) {
		t, err := p.expect(lex.Ident)
		if err != nil {
			return err
		}
		r.Types = append(r.Types, t.Text)
		for p.accept(lex.Pipe) {
			p.accept(lex.Colon) // the :KNOWS|:FOLLOWS form allows a leading colon
			t, err := p.expect(lex.Ident)
			if err != nil {
				return err
			}
			r.Types = append(r.Types, t.Text)
		}
	}
	if p.at(lex.Star) {
		vl, err := p.varLength()
		if err != nil {
			return err
		}
		r.VarLen = vl
	}
	if p.at(lex.Lbrace) {
		props, err := p.propertyMap()
		if err != nil {
			return err
		}
		r.Properties = props
	}
	if _, err := p.expect(lex.Rbracket); err != nil {
		return err
	}
	return nil
}

// varLength parses *, *n, *n..m, *n.., *..m forms after the leading star.
func (p *parser) varLength() (*ast.VarLength, error) {
	p.advance() // *
	vl := &ast.VarLength{Min: -1, Max: -1}
	if p.at(lex.Int) {
		n, err := p.intText(p.cur())
		if err != nil {
			return nil, err
		}
		vl.Min = n
		p.advance()
	}
	if p.accept(lex.DotDot) {
		if p.at(lex.Int) {
			n, err := p.intText(p.cur())
			if err != nil {
				return nil, err
			}
			vl.Max = n
			p.advance()
		}
	} else if vl.Min != -1 {
		vl.Max = vl.Min // *n means exactly n hops
	}
	return vl, nil
}

func (p *parser) propertyMap() ([]ast.PropEntry, error) {
	if _, err := p.expect(lex.Lbrace); err != nil {
		return nil, err
	}
	var entries []ast.PropEntry
	if !p.at(lex.Rbrace) {
		for {
			key, err := p.expect(lex.Ident)
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(lex.Colon); err != nil {
				return nil, err
			}
			v, err := p.expr()
			if err != nil {
				return nil, err
			}
			entries = append(entries, ast.PropEntry{Key: key.Text, Value: v})
			if !p.accept(lex.Comma) {
				break
			}
		}
	}
	if _, err := p.expect(lex.Rbrace); err != nil {
		return nil, err
	}
	return entries, nil
}
