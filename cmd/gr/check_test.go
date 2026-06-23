package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckCleanDB(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	// Create a small database then check it.
	_, _, code := runCLI(t, []string{"-c", "CREATE (a:Person {name:'A'})-[:K]->(b:Person {name:'B'})", db}, "")
	if code != exitOK {
		t.Fatalf("setup: code=%d", code)
	}

	out, errb, code := runCLI(t, []string{"check", db}, "")
	if code != exitOK {
		t.Fatalf("check: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "OK:") {
		t.Errorf("expected OK in output: %q", out)
	}
}

func TestCheckDefaultLevel(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	runCLI(t, []string{"-c", "CREATE (p:Person)", db}, "")

	out, _, code := runCLI(t, []string{"check", db, "--level", "default"}, "")
	if code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "OK:") {
		t.Errorf("expected OK: %q", out)
	}
}

func TestCheckFullLevel(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	runCLI(t, []string{"-c", "CREATE (a:A)-[:R]->(b:B)", db}, "")

	out, _, code := runCLI(t, []string{"check", db, "--level", "full"}, "")
	if code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "OK:") {
		t.Errorf("expected OK: %q", out)
	}
}

func TestCheckJSONFormat(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	runCLI(t, []string{"-c", "CREATE (p:Person)", db}, "")

	out, _, code := runCLI(t, []string{"check", db, "--format", "json"}, "")
	if code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, `"clean": true`) {
		t.Errorf("expected clean:true in JSON: %q", out)
	}
}

func TestCheckMissingDB(t *testing.T) {
	_, _, code := runCLI(t, []string{"check", "/no/such/file.gr"}, "")
	if code == exitOK {
		t.Error("expected non-zero exit for missing file")
	}
}

func TestCheckBadLevel(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	runCLI(t, []string{"-c", "CREATE (p:Person)", db}, "")

	_, _, code := runCLI(t, []string{"check", db, "--level", "bogus"}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for bad level, got %d", code)
	}
}

func TestCheckBadFormat(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	runCLI(t, []string{"-c", "CREATE (p:Person)", db}, "")

	_, _, code := runCLI(t, []string{"check", db, "--format", "xml"}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for bad format, got %d", code)
	}
}

func TestCheckHelp(t *testing.T) {
	_, errb, code := runCLI(t, []string{"check", "--help"}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for --help, got %d", code)
	}
	if !strings.Contains(errb, "quick") {
		t.Errorf("help text missing level options: %q", errb)
	}
}

func TestCheckUnknownFlag(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "c.gr")
	_, _, code := runCLI(t, []string{"check", db, "--no-such-flag"}, "")
	if code != exitUsage {
		t.Errorf("expected exitUsage for unknown flag, got %d", code)
	}
}
