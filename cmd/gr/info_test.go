package main

import (
	"strings"
	"testing"
)

// TestHumanBytes confirms the size formatter renders bytes and binary units.
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 bytes"},
		{512, "512 bytes"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 * 1024 * 1024, "3.0 MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPlural confirms the count noun is singular only for one.
func TestPlural(t *testing.T) {
	cases := []struct {
		n    int
		noun string
		want string
	}{
		{0, "node", "0 nodes"},
		{1, "node", "1 node"},
		{2, "node", "2 nodes"},
		{1, "property key", "1 property key"},
		{3, "property key", "3 property keys"},
	}
	for _, c := range cases {
		if got := plural(c.n, c.noun); got != c.want {
			t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.noun, got, c.want)
		}
	}
}

// TestInfoCommand exercises .info and gr info over a seeded database: both print the
// same nameplate, the catalog and element counts reflect the data, and the schema
// counts reflect the constraint and index (doc 17 §6.15, §7.8).
func TestInfoCommand(t *testing.T) {
	dir := t.TempDir()
	db := dir + "/social.gr"
	seedDB(t, db)

	want := []string{
		"File:            " + db,
		"Format version:  gr v1 (readable, writable)",
		"Page size:       4096 bytes",
		"Journal mode:    WAL",
		"Encryption:      none",
		"Catalog:         2 labels, 2 relationship types, 4 property keys",
		"Elements:        3 nodes, 2 relationships",
		"Indexes:         1",
		"Constraints:     1 (1 unique)",
	}

	// .info goes to the chatter channel (stderr).
	_, errb, code := runCLI(t, []string{db, "-c", ".info"}, "")
	if code != exitOK {
		t.Fatalf(".info: code=%d", code)
	}
	for _, w := range want {
		if !strings.Contains(errb, w) {
			t.Errorf(".info missing %q\ngot:\n%s", w, errb)
		}
	}

	// gr info goes to stdout so it can be captured.
	out, _, code := runCLI(t, []string{"info", db}, "")
	if code != exitOK {
		t.Fatalf("gr info: code=%d", code)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("gr info missing %q\ngot:\n%s", w, out)
		}
	}
}

// TestInfoElementCountsAreLive confirms the element counts come from a snapshot count,
// so a deleted element is not counted.
func TestInfoElementCountsAreLive(t *testing.T) {
	dir := t.TempDir()
	db := dir + "/x.gr"
	if _, _, code := runCLI(t, []string{db, "-c", "CREATE (a {name:'a'})-[:R]->(b {name:'b'})"}, ""); code != exitOK {
		t.Fatal("create failed")
	}
	if _, _, code := runCLI(t, []string{db, "-c", "MATCH (n) DETACH DELETE n"}, ""); code != exitOK {
		t.Fatal("delete failed")
	}
	_, errb, code := runCLI(t, []string{db, "-c", ".info"}, "")
	if code != exitOK {
		t.Fatalf(".info: code=%d", code)
	}
	if !strings.Contains(errb, "Elements:        0 nodes, 0 relationships") {
		t.Errorf("element counts not live after delete:\n%s", errb)
	}
}

// TestInfoCmdNeedsDatabase confirms gr info without a database argument is a usage
// error.
func TestInfoCmdNeedsDatabase(t *testing.T) {
	_, _, code := runCLI(t, []string{"info"}, "")
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}
