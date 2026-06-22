package main

import "strings"

// firstStatement scans s for the first complete Cypher statement, one terminated by
// a semicolon outside a string, a comment, or a backtick-quoted identifier (doc 17
// §3.3). It returns the statement text without the trailing semicolon, the rest of
// the input after the semicolon, and whether a terminator was found. When no
// top-level semicolon is present the whole input is returned as an incomplete
// statement (complete=false), which is the REPL's continuation signal.
//
// The scanner tracks the same lexical contexts the engine's lexer does so a
// semicolon inside a string literal ('a; b'), a line comment (// a; b), a block
// comment (/* a; b */), or a backtick identifier (`a;b`) does not terminate the
// statement.
func firstStatement(s string) (stmt, rest string, complete bool) {
	var (
		inSingle bool // inside a '...' string
		inDouble bool // inside a "..." string
		inTick   bool // inside a `...` identifier
		inLine   bool // inside a // line comment
		inBlock  bool // inside a /* */ block comment
		escaped  bool // previous char was a backslash inside a string
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inBlock:
			if c == '*' && i+1 < len(rs) && rs[i+1] == '/' {
				inBlock = false
				i++
			}
		case inSingle:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
		case inTick:
			if c == '`' {
				inTick = false
			}
		default:
			switch c {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '`':
				inTick = true
			case '/':
				if i+1 < len(rs) && rs[i+1] == '/' {
					inLine = true
					i++
				} else if i+1 < len(rs) && rs[i+1] == '*' {
					inBlock = true
					i++
				}
			case ';':
				return string(rs[:i]), string(rs[i+1:]), true
			}
		}
	}
	return s, "", false
}

// splitStatements breaks a whole script into its complete statements (doc 17 §2.6).
// A trailing fragment with no terminating semicolon is returned as the remainder, so
// a caller streaming a file can carry it into the next chunk; a script that ends
// without a final semicolon still runs its last statement when the caller flushes the
// remainder. Empty statements (a stray semicolon, whitespace, or a bare comment) are
// dropped.
func splitStatements(script string) (stmts []string, remainder string) {
	for {
		stmt, rest, complete := firstStatement(script)
		if !complete {
			return stmts, strings.TrimSpace(stmt)
		}
		if t := strings.TrimSpace(stmt); t != "" {
			stmts = append(stmts, t)
		}
		script = rest
	}
}

// isDotCommand reports whether a line is a dot-command: its first non-whitespace
// character is a dot (doc 17 §3.4). A dot-command is line-oriented and is recognised
// only when no Cypher statement is mid-entry.
func isDotCommand(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, ".")
}
