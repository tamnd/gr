package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/tamnd/gr"
)

// formatOpts carries the display settings a formatter honours (doc 17 §4): whether to
// print a header row, the separator for the delimited modes, and the text to print for
// a null value.
type formatOpts struct {
	headers   bool
	separator string
	null      string
}

// formatter renders a result set in one output mode (doc 17 §5). It is a streaming
// lifecycle: begin once with the column names, row for each record, end once. The
// delimited and line modes write each row as it arrives; the fixed-width and
// structured modes (table, column, json, markdown, html) buffer the rows and lay them
// out in end, where the full set is needed to size columns or wrap an array.
type formatter interface {
	begin(keys []string)
	row(vals []gr.Value)
	end()
}

// newFormatter builds the formatter for a mode name (doc 17 §5.1), resolving the
// aliases. An unknown mode falls back to the table formatter, which the caller guards
// against by validating the mode name before constructing.
func newFormatter(mode string, w io.Writer, opts formatOpts) formatter {
	base := base{w: w, opts: opts}
	switch canonicalMode(mode) {
	case "csv":
		return &delimited{base: base, sep: opts.separator, rfc: true}
	case "tsv":
		return &delimited{base: base, sep: "\t"}
	case "ascii":
		return &delimited{base: base, sep: opts.separator}
	case "json":
		return &jsonArray{base: base}
	case "jsonl":
		return &jsonLines{base: base}
	case "markdown":
		return &boxed{base: base, style: markdownStyle}
	case "html":
		return &htmlTable{base: base}
	case "list", "line":
		return &lineMode{base: base}
	case "quote":
		return &quoteMode{base: base}
	case "column":
		return &boxed{base: base, style: columnStyle}
	default:
		return &boxed{base: base, style: tableStyle}
	}
}

// canonicalMode resolves a mode alias to its canonical name (doc 17 §5.1).
func canonicalMode(mode string) string {
	switch mode {
	case "box":
		return "table"
	case "tabs":
		return "tsv"
	case "ndjson":
		return "jsonl"
	case "md":
		return "markdown"
	case "cypher":
		return "insert"
	default:
		return mode
	}
}

// validMode reports whether name is a known mode or alias the CLI can render.
func validMode(name string) bool {
	switch canonicalMode(name) {
	case "table", "column", "csv", "tsv", "ascii", "json", "jsonl",
		"markdown", "html", "list", "line", "quote":
		return true
	}
	return false
}

// base holds the shared writer, options, and column names of a formatter.
type base struct {
	w    io.Writer
	opts formatOpts
	keys []string
}

func (b *base) begin(keys []string) { b.keys = keys }

// delimited renders csv, tsv, and ascii (doc 17 §5.3). With rfc set it quotes a field
// per RFC 4180 when it contains the separator, a quote, or a newline; otherwise it
// escapes the separator and newline minimally.
type delimited struct {
	base
	sep string
	rfc bool
}

func (d *delimited) begin(keys []string) {
	d.keys = keys
	if d.opts.headers {
		d.writeRow(keys)
	}
}

func (d *delimited) row(vals []gr.Value) {
	cells := make([]string, len(vals))
	for i, v := range vals {
		cells[i] = renderText(v, d.opts.null)
	}
	d.writeRow(cells)
}

func (d *delimited) writeRow(cells []string) {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = d.escape(c)
	}
	fmt.Fprintln(d.w, strings.Join(out, d.sep))
}

func (d *delimited) escape(s string) string {
	if d.rfc {
		if strings.ContainsAny(s, d.sep+"\"\n\r") {
			return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		return s
	}
	r := strings.NewReplacer("\n", "\\n", "\r", "\\r", d.sep, "\\"+d.sep)
	return r.Replace(s)
}

func (d *delimited) end() {}

// jsonLines renders one JSON object per line (doc 17 §5.4), streaming.
type jsonLines struct{ base }

func (j *jsonLines) row(vals []gr.Value) {
	fmt.Fprintln(j.w, jsonObject(j.keys, vals))
}

func (j *jsonLines) end() {}

// jsonArray renders one JSON array of row objects (doc 17 §5.4). It buffers rows so
// the array brackets and commas wrap the whole result.
type jsonArray struct {
	base
	rows [][]gr.Value
}

func (j *jsonArray) row(vals []gr.Value) {
	cp := make([]gr.Value, len(vals))
	copy(cp, vals)
	j.rows = append(j.rows, cp)
}

func (j *jsonArray) end() {
	if len(j.rows) == 0 {
		fmt.Fprintln(j.w, "[]")
		return
	}
	fmt.Fprintln(j.w, "[")
	for i, r := range j.rows {
		comma := ","
		if i == len(j.rows)-1 {
			comma = ""
		}
		fmt.Fprintf(j.w, "  %s%s\n", jsonObject(j.keys, r), comma)
	}
	fmt.Fprintln(j.w, "]")
}

// jsonObject builds the JSON object for one row, keyed by column name.
func jsonObject(keys []string, vals []gr.Value) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		var v gr.Value
		if i < len(vals) {
			v = vals[i]
		}
		parts[i] = jsonString(k) + ":" + renderJSON(v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// lineMode renders list and line modes: one "name = value" per line, a blank line
// between records (doc 17 §5.6), streaming.
type lineMode struct {
	base
	wrote bool
}

func (l *lineMode) row(vals []gr.Value) {
	if l.wrote {
		fmt.Fprintln(l.w)
	}
	l.wrote = true
	for i, k := range l.keys {
		var v gr.Value
		if i < len(vals) {
			v = vals[i]
		}
		fmt.Fprintf(l.w, "%s = %s\n", k, renderText(v, l.opts.null))
	}
}

func (l *lineMode) end() {}

// quoteMode renders each value as a Cypher literal, comma-joined per row (doc 17
// §5.6), streaming.
type quoteMode struct{ base }

func (q *quoteMode) row(vals []gr.Value) {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = renderQuote(v)
	}
	fmt.Fprintln(q.w, strings.Join(parts, ", "))
}

func (q *quoteMode) end() {}

// htmlTable renders an HTML <table> with escaped cells (doc 17 §5.5), buffering only
// to keep the begin/end structure tidy.
type htmlTable struct {
	base
	started bool
}

func (h *htmlTable) row(vals []gr.Value) {
	if !h.started {
		fmt.Fprintln(h.w, "<table>")
		if h.opts.headers {
			fmt.Fprint(h.w, "<tr>")
			for _, k := range h.keys {
				fmt.Fprintf(h.w, "<th>%s</th>", htmlEscape(k))
			}
			fmt.Fprintln(h.w, "</tr>")
		}
		h.started = true
	}
	fmt.Fprint(h.w, "<tr>")
	for _, v := range vals {
		fmt.Fprintf(h.w, "<td>%s</td>", htmlEscape(renderText(v, h.opts.null)))
	}
	fmt.Fprintln(h.w, "</tr>")
}

func (h *htmlTable) end() {
	if !h.started {
		fmt.Fprintln(h.w, "<table>")
	}
	fmt.Fprintln(h.w, "</table>")
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// boxStyle parameterises the three fixed-width grid renderers (doc 17 §5.2, §5.5):
// the Unicode box, the dashed-rule column mode, and the Markdown pipe table.
type boxStyle int

const (
	tableStyle    boxStyle = iota // Unicode box-drawing frame
	columnStyle                   // space-aligned columns, dashed header rule
	markdownStyle                 // GitHub pipe table
)

// boxed renders the fixed-width grid modes. It buffers all rows so column widths can
// be sized to the widest cell, then lays the grid out in end.
type boxed struct {
	base
	style boxStyle
	rows  [][]string
	numc  []bool
}

func (t *boxed) begin(keys []string) {
	t.keys = keys
	t.numc = make([]bool, len(keys))
}

func (t *boxed) row(vals []gr.Value) {
	cells := make([]string, len(t.keys))
	for i := range t.keys {
		var v gr.Value
		if i < len(vals) {
			v = vals[i]
		}
		cells[i] = renderText(v, t.opts.null)
		if !isNumeric(v) {
			t.numc[i] = false
		} else if len(t.rows) == 0 {
			t.numc[i] = true
		}
	}
	t.rows = append(t.rows, cells)
}

func (t *boxed) end() {
	widths := make([]int, len(t.keys))
	for i, k := range t.keys {
		widths[i] = utf8.RuneCountInString(k)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c); n > widths[i] {
				widths[i] = n
			}
		}
	}
	switch t.style {
	case tableStyle:
		t.renderTable(widths)
	case columnStyle:
		t.renderColumn(widths)
	case markdownStyle:
		t.renderMarkdown(widths)
	}
}

func pad(s string, width int, right bool) string {
	n := width - utf8.RuneCountInString(s)
	if n <= 0 {
		return s
	}
	fill := strings.Repeat(" ", n)
	if right {
		return fill + s
	}
	return s + fill
}

func (t *boxed) renderTable(widths []int) {
	rule := func(l, m, r string) {
		var b strings.Builder
		b.WriteString(l)
		for i, w := range widths {
			b.WriteString(strings.Repeat("─", w+2))
			if i < len(widths)-1 {
				b.WriteString(m)
			}
		}
		b.WriteString(r)
		fmt.Fprintln(t.w, b.String())
	}
	emit := func(cells []string) {
		var b strings.Builder
		b.WriteString("│")
		for i, c := range cells {
			b.WriteString(" " + pad(c, widths[i], t.numc[i]) + " │")
		}
		fmt.Fprintln(t.w, b.String())
	}
	rule("┌", "┬", "┐")
	if t.opts.headers {
		emit(t.keys)
		rule("├", "┼", "┤")
	}
	for _, r := range t.rows {
		emit(r)
	}
	rule("└", "┴", "┘")
}

func (t *boxed) renderColumn(widths []int) {
	emit := func(cells []string) {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = pad(c, widths[i], t.numc[i])
		}
		fmt.Fprintln(t.w, strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	if t.opts.headers {
		emit(t.keys)
		rules := make([]string, len(widths))
		for i, w := range widths {
			rules[i] = strings.Repeat("-", w)
		}
		fmt.Fprintln(t.w, strings.Join(rules, "  "))
	}
	for _, r := range t.rows {
		emit(r)
	}
}

func (t *boxed) renderMarkdown(widths []int) {
	emit := func(cells []string) {
		var b strings.Builder
		b.WriteString("|")
		for i, c := range cells {
			b.WriteString(" " + pad(c, widths[i], false) + " |")
		}
		fmt.Fprintln(t.w, b.String())
	}
	emit(t.keys)
	var b strings.Builder
	b.WriteString("|")
	for _, w := range widths {
		b.WriteString(strings.Repeat("-", w+2) + "|")
	}
	fmt.Fprintln(t.w, b.String())
	for _, r := range t.rows {
		emit(r)
	}
}
