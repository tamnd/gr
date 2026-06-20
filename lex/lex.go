package lex

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error is a lexical error: a description plus the position it was found at. The
// lexer does not recover (doc 10 §2.2) — a malformed token stops the scan.
type Error struct {
	Msg  string
	Pos  int
	Line int
	Col  int
}

func (e *Error) Error() string {
	return "lex: " + e.Msg + " at line " + itoa(e.Line) + ":" + itoa(e.Col)
}

// itoa is a tiny base-10 formatter so the error string needs no fmt import.
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

// Lexer scans a query string into tokens. Construct it with New and pull tokens
// with Next; Tokens drains the whole stream in one call.
type Lexer struct {
	src  string
	pos  int // byte offset of the next rune to read
	line int // 1-based line of pos
	col  int // 1-based column of pos
}

// New returns a lexer over src positioned at the start.
func New(src string) *Lexer {
	return &Lexer{src: src, pos: 0, line: 1, col: 1}
}

// Tokens scans src to completion and returns the token slice (ending in an EOF
// token), or the first lexical error.
func Tokens(src string) ([]Token, error) {
	l := New(src)
	var out []Token
	for {
		t, err := l.Next()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if t.Kind == EOF {
			return out, nil
		}
	}
}

// peek returns the next rune and its byte width without advancing; width 0 means
// end of input.
func (l *Lexer) peek() (rune, int) {
	if l.pos >= len(l.src) {
		return 0, 0
	}
	return utf8.DecodeRuneInString(l.src[l.pos:])
}

// peekAt returns the rune n runes ahead (n>=0) without advancing.
func (l *Lexer) peekAt(n int) rune {
	p := l.pos
	for i := 0; i <= n; i++ {
		if p >= len(l.src) {
			return 0
		}
		r, w := utf8.DecodeRuneInString(l.src[p:])
		if i == n {
			return r
		}
		p += w
	}
	return 0
}

// advance consumes one rune, maintaining line/col, and returns it.
func (l *Lexer) advance() rune {
	r, w := l.peek()
	if w == 0 {
		return 0
	}
	l.pos += w
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// errf builds an Error at the current position.
func (l *Lexer) errf(msg string) error {
	return &Error{Msg: msg, Pos: l.pos, Line: l.line, Col: l.col}
}

// Next returns the next token, skipping whitespace and comments first.
func (l *Lexer) Next() (Token, error) {
	if err := l.skipTrivia(); err != nil {
		return Token{}, err
	}
	startPos, startLine, startCol := l.pos, l.line, l.col
	r, w := l.peek()
	if w == 0 {
		return Token{Kind: EOF, Pos: startPos, Line: startLine, Col: startCol}, nil
	}

	mk := func(k Kind, text string) Token {
		return Token{Kind: k, Text: text, Pos: startPos, Line: startLine, Col: startCol}
	}

	switch {
	case isIdentStart(r):
		return l.scanIdent(startPos, startLine, startCol)
	case r == '`':
		return l.scanBacktick(startPos, startLine, startCol)
	case r >= '0' && r <= '9':
		return l.scanNumber(startPos, startLine, startCol)
	case r == '.' && isDigit(l.peekAt(1)):
		return l.scanNumber(startPos, startLine, startCol)
	case r == '\'' || r == '"':
		return l.scanString(startPos, startLine, startCol)
	case r == '$':
		return l.scanParam(startPos, startLine, startCol)
	}

	// Operators and punctuation. Multi-character operators are matched first.
	l.advance()
	switch r {
	case '+':
		return mk(Plus, "+"), nil
	case '-':
		return mk(Minus, "-"), nil
	case '*':
		return mk(Star, "*"), nil
	case '/':
		return mk(Slash, "/"), nil
	case '%':
		return mk(Percent, "%"), nil
	case '^':
		return mk(Caret, "^"), nil
	case '=':
		return mk(Eq, "="), nil
	case '<':
		if n, _ := l.peek(); n == '>' {
			l.advance()
			return mk(Ne, "<>"), nil
		}
		if n, _ := l.peek(); n == '=' {
			l.advance()
			return mk(Le, "<="), nil
		}
		return mk(Lt, "<"), nil
	case '>':
		if n, _ := l.peek(); n == '=' {
			l.advance()
			return mk(Ge, ">="), nil
		}
		return mk(Gt, ">"), nil
	case '(':
		return mk(Lparen, "("), nil
	case ')':
		return mk(Rparen, ")"), nil
	case '[':
		return mk(Lbracket, "["), nil
	case ']':
		return mk(Rbracket, "]"), nil
	case '{':
		return mk(Lbrace, "{"), nil
	case '}':
		return mk(Rbrace, "}"), nil
	case ',':
		return mk(Comma, ","), nil
	case ':':
		return mk(Colon, ":"), nil
	case '|':
		return mk(Pipe, "|"), nil
	case ';':
		return mk(Semi, ";"), nil
	case '.':
		if n, _ := l.peek(); n == '.' {
			l.advance()
			return mk(DotDot, ".."), nil
		}
		return mk(Dot, "."), nil
	}
	return Token{}, &Error{Msg: "invalid character " + string(r), Pos: startPos, Line: startLine, Col: startCol}
}

// skipTrivia advances past whitespace and comments. An unterminated block
// comment is a lexical error.
func (l *Lexer) skipTrivia() error {
	for {
		r, w := l.peek()
		if w == 0 {
			return nil
		}
		switch {
		case unicode.IsSpace(r):
			l.advance()
		case r == '/' && l.peekAt(1) == '/':
			for {
				c, cw := l.peek()
				if cw == 0 || c == '\n' {
					break
				}
				l.advance()
			}
		case r == '/' && l.peekAt(1) == '*':
			cl, cc, cp := l.line, l.col, l.pos
			l.advance() // /
			l.advance() // *
			closed := false
			for {
				c, cw := l.peek()
				if cw == 0 {
					break
				}
				if c == '*' && l.peekAt(1) == '/' {
					l.advance()
					l.advance()
					closed = true
					break
				}
				l.advance()
			}
			if !closed {
				return &Error{Msg: "unterminated block comment", Pos: cp, Line: cl, Col: cc}
			}
		default:
			return nil
		}
	}
}

// scanIdent scans a bare identifier and folds it to a keyword if it matches one.
func (l *Lexer) scanIdent(p, ln, cl int) (Token, error) {
	start := l.pos
	for {
		r, w := l.peek()
		if w == 0 || !isIdentPart(r) {
			break
		}
		l.advance()
	}
	text := l.src[start:l.pos]
	if k, ok := keywords[strings.ToUpper(text)]; ok {
		return Token{Kind: k, Text: text, Pos: p, Line: ln, Col: cl}, nil
	}
	return Token{Kind: Ident, Text: text, Pos: p, Line: ln, Col: cl}, nil
}

// scanBacktick scans a backtick-quoted identifier, where a doubled backtick is a
// literal backtick. Backtick-quoted names are never keywords.
func (l *Lexer) scanBacktick(p, ln, cl int) (Token, error) {
	l.advance() // opening `
	var b strings.Builder
	for {
		r, w := l.peek()
		if w == 0 {
			return Token{}, &Error{Msg: "unterminated quoted identifier", Pos: p, Line: ln, Col: cl}
		}
		l.advance()
		if r == '`' {
			if n, _ := l.peek(); n == '`' {
				l.advance()
				b.WriteByte('`')
				continue
			}
			return Token{Kind: Ident, Text: b.String(), Pos: p, Line: ln, Col: cl}, nil
		}
		b.WriteRune(r)
	}
}

// scanNumber scans an integer or float literal, including 0x.. and 0o.. integer
// forms and the e-notation float form. The raw lexeme is kept in Text.
func (l *Lexer) scanNumber(p, ln, cl int) (Token, error) {
	start := l.pos
	isFloat := false

	// Hex and octal integers.
	if r, _ := l.peek(); r == '0' {
		switch l.peekAt(1) {
		case 'x', 'X':
			l.advance()
			l.advance()
			n := 0
			for isHex(l.peekRune()) {
				l.advance()
				n++
			}
			if n == 0 {
				return Token{}, l.errf("malformed hexadecimal literal")
			}
			return Token{Kind: Int, Text: l.src[start:l.pos], Pos: p, Line: ln, Col: cl}, nil
		case 'o', 'O':
			l.advance()
			l.advance()
			n := 0
			for r := l.peekRune(); r >= '0' && r <= '7'; r = l.peekRune() {
				l.advance()
				n++
			}
			if n == 0 {
				return Token{}, l.errf("malformed octal literal")
			}
			return Token{Kind: Int, Text: l.src[start:l.pos], Pos: p, Line: ln, Col: cl}, nil
		}
	}

	for isDigit(l.peekRune()) {
		l.advance()
	}
	if r := l.peekRune(); r == '.' && l.peekAt(1) != '.' { // not the .. range token
		if isDigit(l.peekAt(1)) || !isIdentStart(l.peekAt(1)) {
			isFloat = true
			l.advance() // .
			for isDigit(l.peekRune()) {
				l.advance()
			}
		}
	}
	if r := l.peekRune(); r == 'e' || r == 'E' {
		save := l.savePoint()
		l.advance()
		if s := l.peekRune(); s == '+' || s == '-' {
			l.advance()
		}
		if !isDigit(l.peekRune()) {
			l.restore(save) // not an exponent; leave e for the next token
		} else {
			isFloat = true
			for isDigit(l.peekRune()) {
				l.advance()
			}
		}
	}
	k := Int
	if isFloat {
		k = Float
	}
	return Token{Kind: k, Text: l.src[start:l.pos], Pos: p, Line: ln, Col: cl}, nil
}

// scanString scans a single- or double-quoted string, resolving escapes into
// the decoded value held in Text.
func (l *Lexer) scanString(p, ln, cl int) (Token, error) {
	quote := l.advance()
	var b strings.Builder
	for {
		r, w := l.peek()
		if w == 0 || r == '\n' {
			return Token{}, &Error{Msg: "unterminated string literal", Pos: p, Line: ln, Col: cl}
		}
		l.advance()
		if r == quote {
			return Token{Kind: String, Text: b.String(), Pos: p, Line: ln, Col: cl}, nil
		}
		if r == '\\' {
			e, w2 := l.peek()
			if w2 == 0 {
				return Token{}, &Error{Msg: "unterminated string literal", Pos: p, Line: ln, Col: cl}
			}
			l.advance()
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '0':
				b.WriteByte(0)
			case '\\':
				b.WriteByte('\\')
			case '\'':
				b.WriteByte('\'')
			case '"':
				b.WriteByte('"')
			case '`':
				b.WriteByte('`')
			case 'u':
				cp, err := l.scanHexEscape(4, p, ln, cl)
				if err != nil {
					return Token{}, err
				}
				b.WriteRune(cp)
			case 'U':
				cp, err := l.scanHexEscape(8, p, ln, cl)
				if err != nil {
					return Token{}, err
				}
				b.WriteRune(cp)
			default:
				return Token{}, l.errf("invalid escape sequence \\" + string(e))
			}
			continue
		}
		b.WriteRune(r)
	}
}

// scanHexEscape reads exactly n hex digits for a \u/\U unicode escape.
func (l *Lexer) scanHexEscape(n, p, ln, cl int) (rune, error) {
	var cp rune
	for range n {
		r := l.peekRune()
		if !isHex(r) {
			return 0, &Error{Msg: "malformed unicode escape", Pos: p, Line: ln, Col: cl}
		}
		l.advance()
		cp = cp<<4 | rune(hexVal(r))
	}
	return cp, nil
}

// scanParam scans a $-prefixed parameter reference: $name or $0.
func (l *Lexer) scanParam(p, ln, cl int) (Token, error) {
	l.advance() // $
	r, w := l.peek()
	if w == 0 {
		return Token{}, l.errf("expected parameter name after '$'")
	}
	start := l.pos
	if isDigit(r) {
		for isDigit(l.peekRune()) {
			l.advance()
		}
	} else if isIdentStart(r) {
		for isIdentPart(l.peekRune()) {
			l.advance()
		}
	} else if r == '`' {
		t, err := l.scanBacktick(p, ln, cl)
		if err != nil {
			return Token{}, err
		}
		return Token{Kind: Param, Text: t.Text, Pos: p, Line: ln, Col: cl}, nil
	} else {
		return Token{}, l.errf("expected parameter name after '$'")
	}
	return Token{Kind: Param, Text: l.src[start:l.pos], Pos: p, Line: ln, Col: cl}, nil
}

// peekRune returns the next rune (0 at end), discarding the width.
func (l *Lexer) peekRune() rune { r, _ := l.peek(); return r }

// savePoint captures the cursor so a speculative scan can rewind.
type savePoint struct{ pos, line, col int }

func (l *Lexer) savePoint() savePoint { return savePoint{l.pos, l.line, l.col} }
func (l *Lexer) restore(s savePoint)  { l.pos, l.line, l.col = s.pos, s.line, s.col }

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentPart(r rune) bool  { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
func isDigit(r rune) bool      { return r >= '0' && r <= '9' }
func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

func hexVal(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	default:
		return int(r-'A') + 10
	}
}
