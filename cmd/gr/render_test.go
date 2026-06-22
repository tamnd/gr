package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tamnd/gr"
)

func TestRenderText(t *testing.T) {
	cases := []struct {
		v    gr.Value
		want string
	}{
		{nil, "(null)"},
		{true, "true"},
		{false, "false"},
		{int64(42), "42"},
		{float64(1.5), "1.5"},
		{"hello", "hello"},
		{[]byte{0xde, 0xad}, "0xdead"},
		{[]gr.Value{int64(1), "a"}, "[1, a]"},
		{map[string]gr.Value{"b": int64(2), "a": int64(1)}, "{a: 1, b: 2}"},
	}
	for _, c := range cases {
		if got := renderText(c.v, "(null)"); got != c.want {
			t.Errorf("renderText(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestRenderTextNullString(t *testing.T) {
	if got := renderText(nil, "NULL"); got != "NULL" {
		t.Errorf("renderText(nil, NULL) = %q, want NULL", got)
	}
}

func TestRenderQuote(t *testing.T) {
	cases := []struct {
		v    gr.Value
		want string
	}{
		{nil, "null"},
		{int64(3), "3"},
		{"a'b", `'a\'b'`},
		{"line\nbreak", `'line\nbreak'`},
		{[]gr.Value{int64(1), "x"}, `[1, 'x']`},
	}
	for _, c := range cases {
		if got := renderQuote(c.v); got != c.want {
			t.Errorf("renderQuote(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestRenderJSON(t *testing.T) {
	cases := []struct {
		v    gr.Value
		want string
	}{
		{nil, "null"},
		{int64(5), "5"},
		{"a\"b", `"a\"b"`},
		{[]gr.Value{int64(1), int64(2)}, "[1,2]"},
		{map[string]gr.Value{"a": int64(1)}, `{"a":1}`},
		{[]byte{0x01}, `{"_bytes":"01"}`},
	}
	for _, c := range cases {
		if got := renderJSON(c.v); got != c.want {
			t.Errorf("renderJSON(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestRenderGraphObjectsJSON drives a node, relationship, and path through a real
// query and the JSON renderer, so the graph-object surface (doc 16 §10) is exercised
// end to end: labels, type, endpoints, and properties all reach the output, not just
// the element id.
func TestRenderGraphObjectsJSON(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.gr")
	_, errb, code := runCLI(t, []string{dbPath, "-c", "CREATE (a:Person {name: 'Ada', age: 36})-[:KNOWS {since: 2019}]->(b:Person {name: 'Lin'})"}, "")
	if code != exitOK {
		t.Fatalf("create code = %d; stderr=%q", code, errb)
	}
	out, errb, code := runCLI(t, []string{dbPath, "--mode", "jsonl", "-c", "MATCH (a:Person)-[r:KNOWS]->(b) RETURN a, r"}, "")
	if code != exitOK {
		t.Fatalf("query code = %d; stderr=%q", code, errb)
	}
	for _, want := range []string{`"_labels":["Person"]`, `"name":"Ada"`, `"age":36`, `"_type":"KNOWS"`, `"since":2019`, `"_start":"n`, `"_end":"n`} {
		if !strings.Contains(out, want) {
			t.Errorf("jsonl output missing %q\ngot: %s", want, out)
		}
	}
}

// TestRenderGraphObjectsText drives a node through the flat text renderer, confirming
// the compact pattern form carries the label and properties (doc 17 §5.7).
func TestRenderGraphObjectsText(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.gr")
	_, errb, code := runCLI(t, []string{dbPath, "-c", "CREATE (:City {name: 'Hue', founded: 1802})"}, "")
	if code != exitOK {
		t.Fatalf("create code = %d; stderr=%q", code, errb)
	}
	out, errb, code := runCLI(t, []string{dbPath, "--mode", "csv", "-c", "MATCH (c:City) RETURN c"}, "")
	if code != exitOK {
		t.Fatalf("query code = %d; stderr=%q", code, errb)
	}
	if !strings.Contains(out, "(:City {founded: 1802, name: Hue})") {
		t.Errorf("text output missing the node pattern form\ngot: %s", out)
	}
}

func TestIsNumeric(t *testing.T) {
	if !isNumeric(int64(1)) || !isNumeric(float64(1)) {
		t.Error("integers and floats should be numeric")
	}
	if isNumeric("1") || isNumeric(nil) || isNumeric(true) {
		t.Error("strings, null, and booleans should not be numeric")
	}
}
