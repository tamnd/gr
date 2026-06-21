package parse

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/lex"
)

// createConstraint parses
//
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (var:Label) REQUIRE var.prop IS UNIQUE
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (var:Label) REQUIRE var.prop IS NOT NULL
//
// the node uniqueness and node existence forms (doc 08 §4.1, §6). The name is
// optional; the engine derives one when it is omitted. This release supports
// single-property node constraints; composite keys and relationship constraints are
// later work.
func (p *parser) createConstraint() (*ast.Query, error) {
	start := p.advance() // CREATE
	if _, err := p.expectWord("CONSTRAINT"); err != nil {
		return nil, err
	}
	cc := &ast.CreateConstraint{Pos: pos(start)}
	// An optional name: any identifier that is not the IF or FOR that would follow
	// an unnamed constraint.
	if p.at(lex.Ident) && !p.atWord("IF") && !p.atWord("FOR") {
		cc.Name = p.advance().Text
	}
	if p.acceptWord("IF") {
		if _, err := p.expect(lex.Not); err != nil {
			return nil, err
		}
		if _, err := p.expectWord("EXISTS"); err != nil {
			return nil, err
		}
		cc.IfNotExists = true
	}
	if _, err := p.expectWord("FOR"); err != nil {
		return nil, err
	}
	if _, err := p.expect(lex.Lparen); err != nil {
		return nil, err
	}
	v, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	cc.Var = v.Text
	if _, err := p.expect(lex.Colon); err != nil {
		return nil, err
	}
	label, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	cc.Label = label.Text
	if _, err := p.expect(lex.Rparen); err != nil {
		return nil, err
	}
	if _, err := p.expectWord("REQUIRE"); err != nil {
		return nil, err
	}
	prop, err := p.requireProp(cc.Var)
	if err != nil {
		return nil, err
	}
	cc.Props = []string{prop}
	if _, err := p.expect(lex.Is); err != nil {
		return nil, err
	}
	// The predicate after IS chooses the constraint kind: UNIQUE for uniqueness,
	// NOT NULL for existence. NOT is a real keyword, NULL too, while UNIQUE is a
	// soft keyword matched as an identifier.
	if p.accept(lex.Not) {
		if _, err := p.expect(lex.Null); err != nil {
			return nil, err
		}
		cc.Type = ast.ConstraintExists
		return &ast.Query{Pos: pos(start), Schema: cc}, nil
	}
	if _, err := p.expectWord("UNIQUE"); err != nil {
		return nil, err
	}
	cc.Type = ast.ConstraintUnique
	return &ast.Query{Pos: pos(start), Schema: cc}, nil
}

// requireProp parses a `var.prop` property reference in a REQUIRE clause, checking
// that the variable matches the one the FOR pattern bound (doc 08 §6).
func (p *parser) requireProp(boundVar string) (string, error) {
	v, err := p.expect(lex.Ident)
	if err != nil {
		return "", err
	}
	if v.Text != boundVar {
		return "", p.errAt(v, "REQUIRE refers to "+v.Text+", not the pattern variable "+boundVar)
	}
	if _, err := p.expect(lex.Dot); err != nil {
		return "", err
	}
	prop, err := p.expect(lex.Ident)
	if err != nil {
		return "", err
	}
	return prop.Text, nil
}

// dropConstraint parses DROP CONSTRAINT name [IF EXISTS].
func (p *parser) dropConstraint() (*ast.Query, error) {
	start := p.advance() // DROP
	if _, err := p.expectWord("CONSTRAINT"); err != nil {
		return nil, err
	}
	name, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	dc := &ast.DropConstraint{Pos: pos(start), Name: name.Text}
	if p.acceptWord("IF") {
		if _, err := p.expectWord("EXISTS"); err != nil {
			return nil, err
		}
		dc.IfExists = true
	}
	return &ast.Query{Pos: pos(start), Schema: dc}, nil
}
