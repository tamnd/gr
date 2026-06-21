package parse

import (
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/lex"
	"github.com/tamnd/gr/value"
)

// createConstraint parses
//
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (var:Label) REQUIRE var.prop IS UNIQUE
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (var:Label) REQUIRE var.prop IS NOT NULL
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (var:Label) REQUIRE var.prop IS :: TYPE
//
// the node uniqueness, node existence, and node property-type forms (doc 08 §4.1,
// §6). The IS :: TYPE form also accepts the IS TYPED TYPE synonym. The name is
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
	// NOT NULL for existence, and :: TYPE (or the TYPED TYPE synonym) for a property
	// type. NOT and NULL are real keywords, while UNIQUE, TYPED, and the type names
	// are soft keywords matched as identifiers. A type predicate opens with a colon
	// (the lexer emits :: as two Colon tokens) or the word TYPED.
	if p.accept(lex.Not) {
		if _, err := p.expect(lex.Null); err != nil {
			return nil, err
		}
		cc.Type = ast.ConstraintExists
		return &ast.Query{Pos: pos(start), Schema: cc}, nil
	}
	if p.accept(lex.Colon) {
		if _, err := p.expect(lex.Colon); err != nil {
			return nil, err
		}
		vt, err := p.propType()
		if err != nil {
			return nil, err
		}
		cc.Type = ast.ConstraintPropertyType
		cc.PropType = vt
		return &ast.Query{Pos: pos(start), Schema: cc}, nil
	}
	if p.acceptWord("TYPED") {
		vt, err := p.propType()
		if err != nil {
			return nil, err
		}
		cc.Type = ast.ConstraintPropertyType
		cc.PropType = vt
		return &ast.Query{Pos: pos(start), Schema: cc}, nil
	}
	if _, err := p.expectWord("UNIQUE"); err != nil {
		return nil, err
	}
	cc.Type = ast.ConstraintUnique
	return &ast.Query{Pos: pos(start), Schema: cc}, nil
}

// propType parses a property-type name in a REQUIRE v.p IS :: TYPE clause and maps
// it to a value type. This release recognizes the four scalar Cypher types whose
// values gr stores natively: BOOLEAN, STRING, INTEGER, and FLOAT (doc 08 §4.1). The
// names are soft keywords matched as identifiers, so the source case is kept and
// compared case-insensitively here.
func (p *parser) propType() (value.Type, error) {
	name, err := p.expect(lex.Ident)
	if err != nil {
		return 0, err
	}
	switch strings.ToUpper(name.Text) {
	case "BOOLEAN":
		return value.TypeBool, nil
	case "STRING":
		return value.TypeString, nil
	case "INTEGER":
		return value.TypeInt, nil
	case "FLOAT":
		return value.TypeFloat, nil
	default:
		return 0, p.errAt(name, "unsupported property type "+name.Text+", want BOOLEAN, STRING, INTEGER, or FLOAT")
	}
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

// createIndex parses
//
//	CREATE INDEX [name] [IF NOT EXISTS] FOR (var:Label) ON (var.prop)
//
// a node property index (doc 07 §4). The name is optional; the engine derives one
// when it is omitted. This release supports single-property node indexes; composite
// indexes and relationship indexes are later work. The grammar mirrors
// createConstraint up to the FOR pattern, then takes ON (var.prop) where a
// constraint takes REQUIRE.
func (p *parser) createIndex() (*ast.Query, error) {
	start := p.advance() // CREATE
	if _, err := p.expectWord("INDEX"); err != nil {
		return nil, err
	}
	ci := &ast.CreateIndex{Pos: pos(start)}
	if p.at(lex.Ident) && !p.atWord("IF") && !p.atWord("FOR") {
		ci.Name = p.advance().Text
	}
	if p.acceptWord("IF") {
		if _, err := p.expect(lex.Not); err != nil {
			return nil, err
		}
		if _, err := p.expectWord("EXISTS"); err != nil {
			return nil, err
		}
		ci.IfNotExists = true
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
	ci.Var = v.Text
	if _, err := p.expect(lex.Colon); err != nil {
		return nil, err
	}
	label, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	ci.Label = label.Text
	if _, err := p.expect(lex.Rparen); err != nil {
		return nil, err
	}
	// ON is a real keyword (it also leads MERGE's ON CREATE / ON MATCH), so it is
	// matched as a token, not as a soft keyword.
	if _, err := p.expect(lex.On); err != nil {
		return nil, err
	}
	if _, err := p.expect(lex.Lparen); err != nil {
		return nil, err
	}
	prop, err := p.requireProp(ci.Var)
	if err != nil {
		return nil, err
	}
	ci.Props = []string{prop}
	if _, err := p.expect(lex.Rparen); err != nil {
		return nil, err
	}
	return &ast.Query{Pos: pos(start), Schema: ci}, nil
}

// dropIndex parses DROP INDEX name [IF EXISTS].
func (p *parser) dropIndex() (*ast.Query, error) {
	start := p.advance() // DROP
	if _, err := p.expectWord("INDEX"); err != nil {
		return nil, err
	}
	name, err := p.expect(lex.Ident)
	if err != nil {
		return nil, err
	}
	di := &ast.DropIndex{Pos: pos(start), Name: name.Text}
	if p.acceptWord("IF") {
		if _, err := p.expectWord("EXISTS"); err != nil {
			return nil, err
		}
		di.IfExists = true
	}
	return &ast.Query{Pos: pos(start), Schema: di}, nil
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
