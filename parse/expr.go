package parse

import (
	"strconv"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/lex"
	"github.com/tamnd/gr/value"
)

// expr parses a full expression. The grammar is precedence-stratified (doc 09
// §10.2): each level handles one precedence tier and defers to the next-tighter
// level, so a + b * c parses as a + (b * c).
//
// Tiers, loosest to tightest: OR, XOR, AND, NOT, comparison/string/IN/IS,
// add/sub, mul/div/mod, power, unary sign, postfix (.prop, [index], [slice]),
// atom.
func (p *parser) expr() (ast.Expr, error) { return p.orExpr() }

func (p *parser) orExpr() (ast.Expr, error) {
	l, err := p.xorExpr()
	if err != nil {
		return nil, err
	}
	for p.at(lex.Or) {
		op := p.advance()
		r, err := p.xorExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{Pos: pos(op), Op: ast.OpOr, L: l, R: r}
	}
	return l, nil
}

func (p *parser) xorExpr() (ast.Expr, error) {
	l, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for p.at(lex.Xor) {
		op := p.advance()
		r, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{Pos: pos(op), Op: ast.OpXor, L: l, R: r}
	}
	return l, nil
}

func (p *parser) andExpr() (ast.Expr, error) {
	l, err := p.notExpr()
	if err != nil {
		return nil, err
	}
	for p.at(lex.And) {
		op := p.advance()
		r, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{Pos: pos(op), Op: ast.OpAnd, L: l, R: r}
	}
	return l, nil
}

func (p *parser) notExpr() (ast.Expr, error) {
	if p.at(lex.Not) {
		op := p.advance()
		x, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Pos: pos(op), Op: ast.OpNot, X: x}, nil
	}
	return p.comparison()
}

// comparison parses the comparison, string-predicate, IN, and IS NULL tier. It
// is left-associative over the same tier.
func (p *parser) comparison() (ast.Expr, error) {
	l, err := p.addExpr()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.at(lex.Eq), p.at(lex.Ne), p.at(lex.Lt), p.at(lex.Le), p.at(lex.Gt), p.at(lex.Ge):
			op := p.advance()
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			l = &ast.Binary{Pos: pos(op), Op: cmpOp(op.Kind), L: l, R: r}
		case p.at(lex.Starts):
			op := p.advance()
			if _, err := p.expect(lex.With); err != nil {
				return nil, err
			}
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			l = &ast.Binary{Pos: pos(op), Op: ast.OpStartsWith, L: l, R: r}
		case p.at(lex.Ends):
			op := p.advance()
			if _, err := p.expect(lex.With); err != nil {
				return nil, err
			}
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			l = &ast.Binary{Pos: pos(op), Op: ast.OpEndsWith, L: l, R: r}
		case p.at(lex.Contains):
			op := p.advance()
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			l = &ast.Binary{Pos: pos(op), Op: ast.OpContains, L: l, R: r}
		case p.at(lex.In):
			op := p.advance()
			r, err := p.addExpr()
			if err != nil {
				return nil, err
			}
			l = &ast.Binary{Pos: pos(op), Op: ast.OpIn, L: l, R: r}
		case p.at(lex.Is):
			op := p.advance()
			negate := p.accept(lex.Not)
			if _, err := p.expect(lex.Null); err != nil {
				return nil, err
			}
			l = &ast.IsNull{Pos: pos(op), X: l, Negate: negate}
		default:
			return l, nil
		}
	}
}

func cmpOp(k lex.Kind) ast.BinaryOp {
	switch k {
	case lex.Eq:
		return ast.OpEq
	case lex.Ne:
		return ast.OpNe
	case lex.Lt:
		return ast.OpLt
	case lex.Le:
		return ast.OpLe
	case lex.Gt:
		return ast.OpGt
	default:
		return ast.OpGe
	}
}

func (p *parser) addExpr() (ast.Expr, error) {
	l, err := p.mulExpr()
	if err != nil {
		return nil, err
	}
	for p.at(lex.Plus) || p.at(lex.Minus) {
		op := p.advance()
		r, err := p.mulExpr()
		if err != nil {
			return nil, err
		}
		bop := ast.OpAdd
		if op.Kind == lex.Minus {
			bop = ast.OpSub
		}
		l = &ast.Binary{Pos: pos(op), Op: bop, L: l, R: r}
	}
	return l, nil
}

func (p *parser) mulExpr() (ast.Expr, error) {
	l, err := p.powExpr()
	if err != nil {
		return nil, err
	}
	for p.at(lex.Star) || p.at(lex.Slash) || p.at(lex.Percent) {
		op := p.advance()
		r, err := p.powExpr()
		if err != nil {
			return nil, err
		}
		var bop ast.BinaryOp
		switch op.Kind {
		case lex.Star:
			bop = ast.OpMul
		case lex.Slash:
			bop = ast.OpDiv
		default:
			bop = ast.OpMod
		}
		l = &ast.Binary{Pos: pos(op), Op: bop, L: l, R: r}
	}
	return l, nil
}

// powExpr is right-associative: 2 ^ 3 ^ 2 is 2 ^ (3 ^ 2).
func (p *parser) powExpr() (ast.Expr, error) {
	l, err := p.unaryExpr()
	if err != nil {
		return nil, err
	}
	if p.at(lex.Caret) {
		op := p.advance()
		r, err := p.powExpr()
		if err != nil {
			return nil, err
		}
		return &ast.Binary{Pos: pos(op), Op: ast.OpPow, L: l, R: r}, nil
	}
	return l, nil
}

func (p *parser) unaryExpr() (ast.Expr, error) {
	if p.at(lex.Minus) {
		op := p.advance()
		x, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Pos: pos(op), Op: ast.OpNeg, X: x}, nil
	}
	if p.at(lex.Plus) { // unary plus is a no-op
		p.advance()
		return p.unaryExpr()
	}
	return p.postfixExpr()
}

// postfixExpr parses the postfix chain of property access and indexing/slicing.
func (p *parser) postfixExpr() (ast.Expr, error) {
	x, err := p.atom()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.at(lex.Dot):
			op := p.advance()
			key, err := p.expect(lex.Ident)
			if err != nil {
				return nil, err
			}
			x = &ast.Property{Pos: pos(op), Base: x, Key: key.Text}
		case p.at(lex.Lbracket):
			op := p.advance()
			// [..hi], [lo..hi], [lo..], or [index]
			if p.accept(lex.DotDot) {
				var hi ast.Expr
				if !p.at(lex.Rbracket) {
					hi, err = p.expr()
					if err != nil {
						return nil, err
					}
				}
				if _, err := p.expect(lex.Rbracket); err != nil {
					return nil, err
				}
				x = &ast.Slice{Pos: pos(op), Base: x, Lo: nil, Hi: hi}
				continue
			}
			idx, err := p.expr()
			if err != nil {
				return nil, err
			}
			if p.accept(lex.DotDot) {
				var hi ast.Expr
				if !p.at(lex.Rbracket) {
					hi, err = p.expr()
					if err != nil {
						return nil, err
					}
				}
				if _, err := p.expect(lex.Rbracket); err != nil {
					return nil, err
				}
				x = &ast.Slice{Pos: pos(op), Base: x, Lo: idx, Hi: hi}
				continue
			}
			if _, err := p.expect(lex.Rbracket); err != nil {
				return nil, err
			}
			x = &ast.Index{Pos: pos(op), Base: x, Index: idx}
		default:
			return x, nil
		}
	}
}

func (p *parser) atom() (ast.Expr, error) {
	t := p.cur()
	switch t.Kind {
	case lex.Int:
		n, err := p.intText(t)
		if err != nil {
			return nil, err
		}
		p.advance()
		return &ast.Literal{Pos: pos(t), Value: value.Int(int64(n))}, nil
	case lex.Float:
		f, err := strconv.ParseFloat(t.Text, 64)
		if err != nil {
			return nil, p.errAt(t, "malformed float literal "+t.Text)
		}
		p.advance()
		return &ast.Literal{Pos: pos(t), Value: value.Float(f)}, nil
	case lex.String:
		p.advance()
		return &ast.Literal{Pos: pos(t), Value: value.String(t.Text)}, nil
	case lex.True:
		p.advance()
		return &ast.Literal{Pos: pos(t), Value: value.Bool(true)}, nil
	case lex.False:
		p.advance()
		return &ast.Literal{Pos: pos(t), Value: value.Bool(false)}, nil
	case lex.Null:
		p.advance()
		return &ast.Literal{Pos: pos(t), Value: value.Null}, nil
	case lex.Param:
		p.advance()
		return &ast.Param{Pos: pos(t), Name: t.Text}, nil
	case lex.Lparen:
		p.advance()
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lex.Rparen); err != nil {
			return nil, err
		}
		return e, nil
	case lex.Lbracket:
		return p.listLiteral()
	case lex.Lbrace:
		return p.mapLiteral()
	case lex.Case:
		return p.caseExpr()
	case lex.Ident:
		if p.peek(1).Kind == lex.Lparen {
			return p.functionCall()
		}
		p.advance()
		return &ast.Variable{Pos: pos(t), Name: t.Text}, nil
	default:
		return nil, p.errAt(t, "expected an expression, found "+t.Kind.String())
	}
}

func (p *parser) listLiteral() (ast.Expr, error) {
	start := p.advance() // [
	l := &ast.ListLit{Pos: pos(start)}
	if !p.at(lex.Rbracket) {
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			l.Elems = append(l.Elems, e)
			if !p.accept(lex.Comma) {
				break
			}
		}
	}
	if _, err := p.expect(lex.Rbracket); err != nil {
		return nil, err
	}
	return l, nil
}

func (p *parser) mapLiteral() (ast.Expr, error) {
	start := p.cur()
	entries, err := p.propertyMap()
	if err != nil {
		return nil, err
	}
	return &ast.MapLit{Pos: pos(start), Entries: entries}, nil
}

func (p *parser) functionCall() (ast.Expr, error) {
	name := p.advance() // the function name
	fc := &ast.FunctionCall{Pos: pos(name), Name: name.Text}
	p.advance() // (
	if p.accept(lex.Star) {
		fc.Star = true // count(*)
	} else if !p.at(lex.Rparen) {
		fc.Distinct = p.accept(lex.Distinct)
		for {
			a, err := p.expr()
			if err != nil {
				return nil, err
			}
			fc.Args = append(fc.Args, a)
			if !p.accept(lex.Comma) {
				break
			}
		}
	}
	if _, err := p.expect(lex.Rparen); err != nil {
		return nil, err
	}
	return fc, nil
}

// caseExpr parses both the simple form (CASE subject WHEN value …) and the
// searched form (CASE WHEN predicate …).
func (p *parser) caseExpr() (ast.Expr, error) {
	start := p.advance() // CASE
	c := &ast.Case{Pos: pos(start)}
	if !p.at(lex.When) {
		subj, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.Subject = subj
	}
	for p.accept(lex.When) {
		when, err := p.expr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lex.Then); err != nil {
			return nil, err
		}
		then, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, ast.WhenThen{When: when, Then: then})
	}
	if len(c.Whens) == 0 {
		return nil, p.errAt(start, "CASE requires at least one WHEN")
	}
	if p.accept(lex.Else) {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.Else = e
	}
	if _, err := p.expect(lex.End); err != nil {
		return nil, err
	}
	return c, nil
}

// intText parses an integer literal's text, honoring the 0x/0o forms.
func (p *parser) intText(t lex.Token) (int, error) {
	n, err := strconv.ParseInt(t.Text, 0, 64)
	if err != nil {
		return 0, p.errAt(t, "malformed integer literal "+t.Text)
	}
	return int(n), nil
}
