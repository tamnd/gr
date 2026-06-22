package main

import (
	"reflect"
	"testing"
)

func TestFirstStatement(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		stmt     string
		rest     string
		complete bool
	}{
		{"plain", "RETURN 1;", "RETURN 1", "", true},
		{"trailing", "RETURN 1; RETURN 2", "RETURN 1", " RETURN 2", true},
		{"incomplete", "RETURN 1", "RETURN 1", "", false},
		{"semicolon in single", "RETURN 'a;b';", "RETURN 'a;b'", "", true},
		{"semicolon in double", `RETURN "a;b";`, `RETURN "a;b"`, "", true},
		{"semicolon in tick", "MATCH (`a;b`) RETURN 1;", "MATCH (`a;b`) RETURN 1", "", true},
		{"semicolon in line comment", "// a;b\nRETURN 1;", "// a;b\nRETURN 1", "", true},
		{"semicolon in block comment", "/* a;b */ RETURN 1;", "/* a;b */ RETURN 1", "", true},
		{"escaped quote", `RETURN 'a\';b';`, `RETURN 'a\';b'`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmt, rest, complete := firstStatement(c.in)
			if stmt != c.stmt || rest != c.rest || complete != c.complete {
				t.Fatalf("firstStatement(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.in, stmt, rest, complete, c.stmt, c.rest, c.complete)
			}
		})
	}
}

func TestSplitStatements(t *testing.T) {
	stmts, rem := splitStatements("RETURN 1; RETURN 2; RETURN 3")
	want := []string{"RETURN 1", "RETURN 2"}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("stmts = %v, want %v", stmts, want)
	}
	if rem != "RETURN 3" {
		t.Fatalf("remainder = %q, want %q", rem, "RETURN 3")
	}
}

func TestSplitStatementsDropsEmpty(t *testing.T) {
	stmts, rem := splitStatements(" ; ;RETURN 1;; ")
	want := []string{"RETURN 1"}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("stmts = %v, want %v", stmts, want)
	}
	if rem != "" {
		t.Fatalf("remainder = %q, want empty", rem)
	}
}

func TestIsDotCommand(t *testing.T) {
	cases := map[string]bool{
		".help":         true,
		"  .mode json":  true,
		"RETURN 1":      false,
		"":              false,
		"MATCH (n.x) 1": false,
	}
	for in, want := range cases {
		if got := isDotCommand(in); got != want {
			t.Errorf("isDotCommand(%q) = %v, want %v", in, got, want)
		}
	}
}
