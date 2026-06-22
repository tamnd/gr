package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI drives the run entry point with a script on stdin and returns the data
// (stdout), the chatter (stderr), and the exit code.
func runCLI(t *testing.T, args []string, stdin string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), code
}

func TestRunOneShot(t *testing.T) {
	out, _, code := runCLI(t, []string{"--mode", "csv", "-c", "RETURN 1 AS n"}, "")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("out = %q, want it to contain 1", out)
	}
}

func TestRunModeJSON(t *testing.T) {
	out, _, code := runCLI(t, []string{"--mode", "json", "-c", "RETURN 1 AS n"}, "")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, `"n":1`) {
		t.Fatalf("json out = %q", out)
	}
}

func TestRunVersion(t *testing.T) {
	out, _, code := runCLI(t, []string{"--version"}, "")
	if code != exitOK || !strings.Contains(out, version) {
		t.Fatalf("version: out=%q code=%d", out, code)
	}
}

func TestRunHelp(t *testing.T) {
	out, _, code := runCLI(t, []string{"--help"}, "")
	if code != exitOK || !strings.Contains(out, "Usage:") {
		t.Fatalf("help: out=%q code=%d", out, code)
	}
}

func TestRunUsageError(t *testing.T) {
	_, errb, code := runCLI(t, []string{"--bogus"}, "")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errb, "unknown flag") {
		t.Fatalf("stderr = %q", errb)
	}
}

func TestRunSyntaxError(t *testing.T) {
	_, errb, code := runCLI(t, []string{"-c", "THIS IS NOT CYPHER"}, "")
	if code == exitOK {
		t.Fatal("expected non-zero exit for a syntax error")
	}
	if !strings.Contains(errb, "Error:") {
		t.Fatalf("stderr = %q", errb)
	}
}

func TestRunScriptViaStdin(t *testing.T) {
	script := "CREATE (:Person {name: 'Ada'});\nMATCH (p:Person) RETURN count(p) AS c;\n"
	out, _, code := runCLI(t, []string{"--mode", "csv"}, script)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("out = %q, want count 1", out)
	}
}

func TestRunPersistsToFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "social.gr")
	_, _, code := runCLI(t, []string{dbPath, "-c", "CREATE (:Person {name: 'Ada'})"}, "")
	if code != exitOK {
		t.Fatalf("create code = %d", code)
	}
	out, _, code := runCLI(t, []string{dbPath, "--mode", "csv", "-c", "MATCH (p:Person) RETURN count(p) AS c"}, "")
	if code != exitOK {
		t.Fatalf("read code = %d", code)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("out = %q, want the persisted node", out)
	}
}

func TestRunReadOnlyMissingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "missing.gr")
	_, errb, code := runCLI(t, []string{"--readonly", dbPath, "-c", "RETURN 1"}, "")
	if code != exitOpen {
		t.Fatalf("code = %d, want %d; stderr=%q", code, exitOpen, errb)
	}
}

func TestRunNoCreateMissingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "missing.gr")
	_, _, code := runCLI(t, []string{"--no-create", dbPath, "-c", "RETURN 1"}, "")
	if code != exitOpen {
		t.Fatalf("code = %d, want %d", code, exitOpen)
	}
}

func TestRunDotMode(t *testing.T) {
	// A dot-command switches mode mid-script; the second query renders as JSON.
	script := ".mode json\nRETURN 1 AS n;\n"
	out, _, code := runCLI(t, []string{}, script)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, `"n":1`) {
		t.Fatalf("out = %q", out)
	}
}

func TestRunBail(t *testing.T) {
	// With --bail a failing statement stops the script before the next runs.
	script := "THIS IS NOT CYPHER;\nCREATE (:Person);\n"
	_, _, code := runCLI(t, []string{"--bail"}, script)
	if code == exitOK {
		t.Fatal("expected non-zero exit under --bail")
	}
}
