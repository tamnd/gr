package main

import (
	"reflect"
	"testing"
)

func TestTokenizeArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{".mode json", []string{".mode", "json"}},
		{"  .output  out.csv  ", []string{".output", "out.csv"}},
		{`.separator "\t"`, []string{".separator", "\t"}},
		{`.print 'hello world'`, []string{".print", "hello world"}},
		{`.print "a b" c`, []string{".print", "a b", "c"}},
		{`.separator "\x1f"`, []string{".separator", "\x1f"}},
		{`.print "line\none"`, []string{".print", "line\none"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := tokenizeArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenizeArgs(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestHexVal(t *testing.T) {
	cases := map[rune]int{'0': 0, '9': 9, 'a': 10, 'f': 15, 'A': 10, 'F': 15, 'g': -1, 'z': -1}
	for r, want := range cases {
		if got := hexVal(r); got != want {
			t.Errorf("hexVal(%q) = %d, want %d", r, got, want)
		}
	}
}
