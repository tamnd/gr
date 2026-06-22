package main

import (
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
		{gr.Node{ID: 7}, "(:#7)"},
		{gr.Relationship{ID: 9}, "[#9]"},
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
		{gr.Node{ID: 4}, `{"_id":4}`},
		{[]byte{0x01}, `{"_bytes":"01"}`},
	}
	for _, c := range cases {
		if got := renderJSON(c.v); got != c.want {
			t.Errorf("renderJSON(%#v) = %q, want %q", c.v, got, c.want)
		}
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
