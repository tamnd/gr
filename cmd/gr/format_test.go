package main

import (
	"strings"
	"testing"

	"github.com/tamnd/gr"
)

// formatAll runs a formatter's full lifecycle over keys and rows and returns the text.
func formatAll(mode string, opts formatOpts, keys []string, rows [][]gr.Value) string {
	var b strings.Builder
	f := newFormatter(mode, &b, opts)
	f.begin(keys)
	for _, r := range rows {
		f.row(r)
	}
	f.end()
	return b.String()
}

func TestFormatCSV(t *testing.T) {
	out := formatAll("csv", formatOpts{headers: true, separator: ",", null: ""},
		[]string{"a", "b"}, [][]gr.Value{{int64(1), "x,y"}})
	want := "a,b\n1,\"x,y\"\n"
	if out != want {
		t.Fatalf("csv = %q, want %q", out, want)
	}
}

func TestFormatTSV(t *testing.T) {
	out := formatAll("tsv", formatOpts{headers: false}, []string{"a"},
		[][]gr.Value{{int64(1)}, {int64(2)}})
	if out != "1\n2\n" {
		t.Fatalf("tsv = %q", out)
	}
}

func TestFormatJSONArray(t *testing.T) {
	out := formatAll("json", formatOpts{}, []string{"n"},
		[][]gr.Value{{int64(1)}, {int64(2)}})
	want := "[\n  {\"n\":1},\n  {\"n\":2}\n]\n"
	if out != want {
		t.Fatalf("json = %q, want %q", out, want)
	}
}

func TestFormatJSONArrayEmpty(t *testing.T) {
	out := formatAll("json", formatOpts{}, []string{"n"}, nil)
	if out != "[]\n" {
		t.Fatalf("empty json = %q, want []", out)
	}
}

func TestFormatJSONL(t *testing.T) {
	out := formatAll("jsonl", formatOpts{}, []string{"n"},
		[][]gr.Value{{int64(1)}, {int64(2)}})
	if out != "{\"n\":1}\n{\"n\":2}\n" {
		t.Fatalf("jsonl = %q", out)
	}
}

func TestFormatMarkdown(t *testing.T) {
	out := formatAll("markdown", formatOpts{headers: true}, []string{"a", "b"},
		[][]gr.Value{{int64(1), "x"}})
	want := "| a | b |\n|---|---|\n| 1 | x |\n"
	if out != want {
		t.Fatalf("markdown =\n%q\nwant\n%q", out, want)
	}
}

func TestFormatTable(t *testing.T) {
	out := formatAll("table", formatOpts{headers: true}, []string{"n"},
		[][]gr.Value{{int64(1)}})
	for _, want := range []string{"┌", "│ n │", "├", "│ 1 │", "└"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatHTML(t *testing.T) {
	out := formatAll("html", formatOpts{headers: true}, []string{"a"},
		[][]gr.Value{{"<b>"}})
	if !strings.Contains(out, "<table>") || !strings.Contains(out, "&lt;b&gt;") {
		t.Fatalf("html = %q", out)
	}
}

func TestFormatList(t *testing.T) {
	out := formatAll("list", formatOpts{}, []string{"a", "b"},
		[][]gr.Value{{int64(1), "x"}, {int64(2), "y"}})
	want := "a = 1\nb = x\n\na = 2\nb = y\n"
	if out != want {
		t.Fatalf("list = %q, want %q", out, want)
	}
}

func TestFormatQuote(t *testing.T) {
	out := formatAll("quote", formatOpts{}, []string{"a", "b"},
		[][]gr.Value{{int64(1), "x"}})
	if out != "1, 'x'\n" {
		t.Fatalf("quote = %q", out)
	}
}

func TestCanonicalMode(t *testing.T) {
	cases := map[string]string{
		"box": "table", "tabs": "tsv", "ndjson": "jsonl", "md": "markdown",
		"csv": "csv", "json": "json",
	}
	for in, want := range cases {
		if got := canonicalMode(in); got != want {
			t.Errorf("canonicalMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidMode(t *testing.T) {
	for _, m := range []string{"table", "csv", "json", "md", "box", "ndjson"} {
		if !validMode(m) {
			t.Errorf("validMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"bogus", "", "xml"} {
		if validMode(m) {
			t.Errorf("validMode(%q) = true, want false", m)
		}
	}
}
