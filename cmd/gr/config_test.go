package main

import (
	"reflect"
	"testing"
)

func TestParseArgsPositional(t *testing.T) {
	cfg, err := parseArgs([]string{"social.gr", "RETURN 1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dbPath != "social.gr" {
		t.Errorf("dbPath = %q", cfg.dbPath)
	}
	if cfg.trailing != "RETURN 1" {
		t.Errorf("trailing = %q", cfg.trailing)
	}
}

func TestParseArgsFlags(t *testing.T) {
	cfg, err := parseArgs([]string{
		"db.gr", "--mode", "json", "-c", "RETURN 1", "-c", "RETURN 2",
		"--readonly", "--headers=off", "--separator", ";",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "json" {
		t.Errorf("mode = %q", cfg.mode)
	}
	if !reflect.DeepEqual(cfg.cyphers, []string{"RETURN 1", "RETURN 2"}) {
		t.Errorf("cyphers = %v", cfg.cyphers)
	}
	if !cfg.readonly {
		t.Error("readonly not set")
	}
	if cfg.headers || !cfg.headerSet {
		t.Errorf("headers=%v headerSet=%v, want false/true", cfg.headers, cfg.headerSet)
	}
	if cfg.separator != ";" {
		t.Errorf("separator = %q", cfg.separator)
	}
}

func TestParseArgsInlineValue(t *testing.T) {
	cfg, err := parseArgs([]string{"--mode=csv"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "csv" {
		t.Errorf("mode = %q, want csv", cfg.mode)
	}
}

func TestParseArgsTerminator(t *testing.T) {
	cfg, err := parseArgs([]string{"--", "--weird-name.gr"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dbPath != "--weird-name.gr" {
		t.Errorf("dbPath = %q", cfg.dbPath)
	}
}

func TestParseArgsUnknownFlag(t *testing.T) {
	_, err := parseArgs([]string{"--nope"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestParseArgsMissingValue(t *testing.T) {
	_, err := parseArgs([]string{"--mode"})
	if err == nil {
		t.Fatal("expected error for missing flag value")
	}
}

func TestParseArgsBadMode(t *testing.T) {
	_, err := parseArgs([]string{"--mode", "bogus"})
	if err == nil {
		t.Fatal("expected error for bad mode")
	}
}

func TestParseArgsTooMany(t *testing.T) {
	_, err := parseArgs([]string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error for too many positional args")
	}
}

func TestParseOnOff(t *testing.T) {
	for _, s := range []string{"on", "true", "1", "yes"} {
		v, err := parseOnOff(s)
		if err != nil || !v {
			t.Errorf("parseOnOff(%q) = %v, %v", s, v, err)
		}
	}
	for _, s := range []string{"off", "false", "0", "no"} {
		v, err := parseOnOff(s)
		if err != nil || v {
			t.Errorf("parseOnOff(%q) = %v, %v", s, v, err)
		}
	}
	if _, err := parseOnOff("maybe"); err == nil {
		t.Error("expected error for invalid on/off value")
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "ab", 1},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSuggestFlag(t *testing.T) {
	if s := suggestFlag("--mod"); s == "" {
		t.Error("expected a suggestion for --mod")
	}
	if s := suggestFlag("--zzzzz"); s != "" {
		t.Errorf("expected no suggestion, got %q", s)
	}
}
