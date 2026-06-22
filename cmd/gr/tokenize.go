package main

import "strings"

// tokenizeArgs splits a dot-command line into shell-style tokens: whitespace
// separated, with 'single' and "double" quoting for arguments that contain spaces
// and backslash escaping inside double quotes (doc 17 §3.4). The leading dot-command
// word is the first token. A single-quoted token is literal; a double-quoted token
// honours \n, \t, \\, \" and \xNN escapes so a separator like "\x1f" can be written.
func tokenizeArgs(line string) []string {
	var toks []string
	var b strings.Builder
	inTok := false
	rs := []rune(strings.TrimSpace(line))
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			inTok = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				b.WriteRune(rs[i])
			}
		case c == '"':
			inTok = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
					b.WriteString(unescape(rs, &i))
					continue
				}
				b.WriteRune(rs[i])
			}
		case c == ' ' || c == '\t':
			if inTok {
				toks = append(toks, b.String())
				b.Reset()
				inTok = false
			}
		default:
			inTok = true
			b.WriteRune(c)
		}
	}
	if inTok {
		toks = append(toks, b.String())
	}
	return toks
}

// unescape decodes one backslash escape inside a double-quoted token, starting at
// rs[*i] (the character after the backslash). It advances *i past any extra runes it
// consumes (the two hex digits of a \xNN escape) and returns the decoded text.
func unescape(rs []rune, i *int) string {
	switch rs[*i] {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case '\\':
		return "\\"
	case '"':
		return "\""
	case 'x':
		if *i+2 < len(rs) {
			hi, lo := hexVal(rs[*i+1]), hexVal(rs[*i+2])
			if hi >= 0 && lo >= 0 {
				*i += 2
				return string(rune(hi*16 + lo))
			}
		}
		return "x"
	default:
		return string(rs[*i])
	}
}

// hexVal returns the value of a hex digit, or -1 if r is not one.
func hexVal(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	}
	return -1
}
