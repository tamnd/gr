package parse

import (
	"strconv"
	"strings"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/lex"
	"github.com/tamnd/gr/value"
)

// The PRAGMA grammar (doc 24 §3.2). PRAGMA is a soft keyword recognized only at the head
// of a statement (through wordIs), so it stays usable as an ordinary identifier in a
// normal query. A pragma is one of:
//
//	PRAGMA name             the query form, read the knob's effective value (doc 24 §3.3)
//	PRAGMA name = value      the set form, change the knob (doc 24 §3.4)
//	PRAGMA name = value TEMP the set form, forced session-only (doc 24 §3.5)
//	PRAGMA name(value)       the call form, invoke an action pragma (doc 24 §3.7)
//	PRAGMA name()            the call form with no argument
//
// The call form is distinguished from the set form by the parenthesis; an action pragma
// with no argument is also reachable through the bare query form (PRAGMA name), and the
// configuration subsystem routes by whether the named pragma is an action.

// pragma parses a PRAGMA statement.
func (p *parser) pragma() (*ast.Query, error) {
	start := p.advance() // PRAGMA
	name, err := p.pragmaName()
	if err != nil {
		return nil, err
	}
	cmd := &ast.PragmaCommand{Pos: pos(start), Name: name}
	switch {
	case p.accept(lex.Eq):
		cmd.Set = true
		v, err := p.pragmaValue()
		if err != nil {
			return nil, err
		}
		cmd.Value = v
		// TEMP is a soft keyword: it makes a persistent knob's set apply session-only
		// (doc 24 §3.5), so it stays usable as an identifier elsewhere.
		if p.acceptWord("TEMP") {
			cmd.Temp = true
		}
	case p.accept(lex.Lparen):
		// The call form for action pragmas (doc 24 §3.7). The argument is optional, so
		// PRAGMA name() invokes the action with no argument, carried as a null Value.
		cmd.Call = true
		cmd.Value = value.Null
		if !p.accept(lex.Rparen) {
			v, err := p.pragmaValue()
			if err != nil {
				return nil, err
			}
			cmd.Value = v
			if _, err := p.expect(lex.Rparen); err != nil {
				return nil, err
			}
		}
	}
	return &ast.Query{Pos: pos(start), Pragma: cmd}, nil
}

// pragmaName parses the pragma name, a bare identifier. The name is case-insensitive
// (doc 24 §3.2), so it is folded to the canonical lower-case spelling here; the
// configuration subsystem looks it up by the folded name.
func (p *parser) pragmaName() (string, error) {
	t, err := p.expect(lex.Ident)
	if err != nil {
		return "", err
	}
	return strings.ToLower(t.Text), nil
}

// pragmaValue parses the right-hand side of a set form: a number (optionally signed), a
// quoted string, a boolean keyword, or a bare word. A bare word that names a boolean
// (on/off/yes/no/true/false) becomes a bool; any other bare word stays a string, which is
// how an enum knob's value (NORMAL, FULL) is carried. The configuration subsystem
// validates and coerces the value against the named knob's type (doc 24 §24.4).
func (p *parser) pragmaValue() (value.Value, error) {
	neg := false
	if p.accept(lex.Minus) {
		neg = true
	}
	t := p.cur()
	switch t.Kind {
	case lex.Int:
		n, err := p.intText(t)
		if err != nil {
			return value.Null, err
		}
		p.advance()
		if neg {
			n = -n
		}
		return value.Int(int64(n)), nil
	case lex.Float:
		f, err := strconv.ParseFloat(t.Text, 64)
		if err != nil {
			return value.Null, p.errAt(t, "malformed float literal "+t.Text)
		}
		p.advance()
		if neg {
			f = -f
		}
		return value.Float(f), nil
	case lex.String:
		if neg {
			return value.Null, p.errAt(t, "a string pragma value cannot be negated")
		}
		p.advance()
		return value.String(t.Text), nil
	case lex.True:
		if neg {
			return value.Null, p.errAt(t, "a boolean pragma value cannot be negated")
		}
		p.advance()
		return value.Bool(true), nil
	case lex.False:
		if neg {
			return value.Null, p.errAt(t, "a boolean pragma value cannot be negated")
		}
		p.advance()
		return value.Bool(false), nil
	case lex.On:
		// ON is a hard keyword (MERGE ... ON CREATE), so it arrives as lex.On rather than
		// a bare word; as a pragma value it reads as the boolean true, the SQLite spelling.
		if neg {
			return value.Null, p.errAt(t, "a boolean pragma value cannot be negated")
		}
		p.advance()
		return value.Bool(true), nil
	case lex.Ident:
		if neg {
			return value.Null, p.errAt(t, "a word pragma value cannot be negated")
		}
		p.advance()
		switch strings.ToLower(t.Text) {
		case "on", "yes", "true":
			return value.Bool(true), nil
		case "off", "no", "false":
			return value.Bool(false), nil
		default:
			return value.String(t.Text), nil
		}
	default:
		return value.Null, p.errAt(t, "expected a pragma value, found "+t.Kind.String())
	}
}
