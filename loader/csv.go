// Package loader is gr's offline bulk loader: it builds a .gr file from node
// and relationship files (CSV or Parquet) far faster than the transactional
// write path can, by writing the columnar base segments and CSR arrays directly
// and only fsyncing once at the end (doc 19 §2).
//
// The loader is not a replacement for the write path; it is a specialized tool
// for the bulk, offline, add-only case (doc 19 §2.2). It produces a normal .gr
// file that opens with gr.Open, passes gr check, and is indistinguishable from
// a file grown transactionally (doc 19 §2.3).
//
// The entry point is [NewLoader] (for a fresh file) or [NewAppender] (for an
// existing file). Each accepts a set of node and relationship sources
// ([NodeSource], [RelSource]) and [Options], and the [Loader.Run] method runs
// the four-pass pipeline (doc 19 §4) to completion.
package loader

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// csvReader is a streaming RFC 4180 CSV parser (doc 19 §5.2).
//
// It handles the edge cases that trip naive parsers:
//   - A leading UTF-8 BOM on the first field of the first line is stripped.
//   - CRLF and LF line endings are both accepted.
//   - A quoted field may contain the delimiter, newlines, and doubled-quote escapes.
//   - A ragged row (too few or too many fields) is returned as-is; the caller
//     validates against the expected field count.
//   - Non-UTF-8 input is rejected with a clear error rather than mojibake.
//
// The reader is not goroutine-safe; each partition (doc 19 §8.2) must use its own.
type csvReader struct {
	r       *bufio.Reader
	delim   rune // field delimiter, default ','
	arrDelim rune // array delimiter inside list fields, default ';'
	lineno  int  // 1-based logical record number (not byte line count)
	fields  []string
	buf     strings.Builder
	firstLine bool
}

// newCSVReader wraps r as a streaming RFC 4180 CSV reader with the given
// field delimiter. A zero delim uses ','; a zero arrDelim uses ';'.
func newCSVReader(r io.Reader, delim, arrDelim rune) *csvReader {
	if delim == 0 {
		delim = ','
	}
	if arrDelim == 0 {
		arrDelim = ';'
	}
	return &csvReader{
		r:         bufio.NewReaderSize(r, 64*1024),
		delim:     delim,
		arrDelim:  arrDelim,
		firstLine: true,
	}
}

// LineNo returns the current 1-based logical record number.
func (c *csvReader) LineNo() int { return c.lineno }

// Fields returns the fields of the last record read by Next.
// The slice is owned by the reader; copy if you need to retain it.
func (c *csvReader) Fields() []string { return c.fields }

// Next reads the next logical record from the stream, populating Fields.
// It returns false on EOF and returns an error for IO problems or malformed UTF-8.
func (c *csvReader) Next() (bool, error) {
	c.fields = c.fields[:0]
	c.buf.Reset()

	// Read until we complete a logical record (accounting for quoted newlines).
	inQuote := false
	fieldStart := true

	for {
		ch, _, err := c.r.ReadRune()
		if err == io.EOF {
			if len(c.fields) == 0 && c.buf.Len() == 0 {
				return false, nil // clean EOF
			}
			// flush the last field
			c.fields = append(c.fields, c.buf.String())
			c.buf.Reset()
			c.lineno++
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if ch == utf8.RuneError {
			return false, fmt.Errorf("loader/csv: non-UTF-8 input at record %d", c.lineno+1)
		}

		// Strip the UTF-8 BOM from the very first field (first byte of first line).
		// The BOM is U+FEFF (0xEF 0xBB 0xBF in UTF-8).
		if c.firstLine && fieldStart && ch == '\ufeff' {
			c.firstLine = false
			continue
		}
		if c.firstLine {
			c.firstLine = false
		}

		switch {
		case inQuote:
			if ch == '"' {
				// Peek ahead: "" inside quotes is a literal quote; " followed by
				// delimiter or newline closes the quote.
				next, _, nerr := c.r.ReadRune()
				if nerr == io.EOF {
					inQuote = false
					c.fields = append(c.fields, c.buf.String())
					c.buf.Reset()
					c.lineno++
					return true, nil
				}
				if nerr != nil {
					return false, nerr
				}
				if next == '"' {
					c.buf.WriteRune('"') // escaped quote
				} else {
					inQuote = false
					// The character after the closing quote must be delimiter or newline.
					if next == rune(c.delim) {
						c.fields = append(c.fields, c.buf.String())
						c.buf.Reset()
						fieldStart = true
					} else if next == '\r' {
						// Consume optional LF after CR.
						if lf, _, _ := c.r.ReadRune(); lf != '\n' {
							_ = c.r.UnreadRune()
						}
						c.fields = append(c.fields, c.buf.String())
						c.buf.Reset()
						c.lineno++
						return true, nil
					} else if next == '\n' {
						c.fields = append(c.fields, c.buf.String())
						c.buf.Reset()
						c.lineno++
						return true, nil
					} else {
						// quote not followed by delimiter or newline — tolerate for
						// real-world files that do this, just include the char
						c.buf.WriteRune(next)
					}
				}
			} else if ch == '\r' {
				// embedded CR inside a quoted field — keep it (the field contains a
				// newline), but normalize CRLF → LF.
				if lf, _, _ := c.r.ReadRune(); lf != '\n' {
					_ = c.r.UnreadRune()
					c.buf.WriteRune('\n')
				} else {
					c.buf.WriteRune('\n')
				}
			} else {
				c.buf.WriteRune(ch)
			}

		default: // not in quote
			if ch == '"' && fieldStart {
				inQuote = true
				fieldStart = false
			} else if ch == rune(c.delim) {
				c.fields = append(c.fields, c.buf.String())
				c.buf.Reset()
				fieldStart = true
			} else if ch == '\r' {
				// CRLF: consume optional LF
				if lf, _, _ := c.r.ReadRune(); lf != '\n' {
					_ = c.r.UnreadRune()
				}
				c.fields = append(c.fields, c.buf.String())
				c.buf.Reset()
				c.lineno++
				return true, nil
			} else if ch == '\n' {
				c.fields = append(c.fields, c.buf.String())
				c.buf.Reset()
				c.lineno++
				return true, nil
			} else {
				c.buf.WriteRune(ch)
				fieldStart = false
			}
		}
	}
}

// splitArrayField splits a list-typed field on the array delimiter, trimming no
// whitespace (the RFC 4180 contract: what you put in is what you get). An empty
// field returns an empty slice (no elements), not a one-element slice with an
// empty string, because an empty CSV field means "no value" (doc 19 §6.6).
func splitArrayField(field string, arrDelim rune) []string {
	if field == "" {
		return nil
	}
	return strings.Split(field, string(arrDelim))
}

// bomReader strips the UTF-8 BOM from the beginning of r if present, then
// returns a reader that reads the rest. Used for the header-only-file path.
func bomReader(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	bs, _ := br.Peek(3)
	if bytes.Equal(bs, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	return br
}
