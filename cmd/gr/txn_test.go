package main

import (
	"strings"
	"testing"
)

func TestShellTransactionCommit(t *testing.T) {
	// A node created inside .begin survives after .commit.
	script := strings.Join([]string{
		".begin",
		"CREATE (:Person {name: 'Ada'});",
		".commit",
		"MATCH (p:Person) RETURN count(p) AS c;",
	}, "\n") + "\n"
	out, errb, code := runCLI(t, []string{"--mode", "csv"}, script)
	if code != exitOK {
		t.Fatalf("code = %d; stderr=%q", code, errb)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("out = %q, want the committed node", out)
	}
}

func TestShellTransactionRollback(t *testing.T) {
	// A node created inside .begin is gone after .rollback.
	script := strings.Join([]string{
		".begin",
		"CREATE (:Person {name: 'Ada'});",
		".rollback",
		"MATCH (p:Person) RETURN count(p) AS c;",
	}, "\n") + "\n"
	out, errb, code := runCLI(t, []string{"--mode", "csv"}, script)
	if code != exitOK {
		t.Fatalf("code = %d; stderr=%q", code, errb)
	}
	if !strings.Contains(out, "0") {
		t.Fatalf("out = %q, want zero after rollback", out)
	}
}

func TestShellTransactionImplicitRollbackOnExit(t *testing.T) {
	// An uncommitted .begin discards when the script ends, so a second
	// invocation against the same file sees no node.
	dir := t.TempDir()
	dbPath := dir + "/social.gr"
	script := ".begin\nCREATE (:Person {name: 'Ada'});\n"
	_, errb, code := runCLI(t, []string{dbPath}, script)
	if code != exitOK {
		t.Fatalf("first code = %d; stderr=%q", code, errb)
	}
	out, _, code := runCLI(t, []string{dbPath, "--mode", "csv", "-c", "MATCH (p:Person) RETURN count(p) AS c"}, "")
	if code != exitOK {
		t.Fatalf("read code = %d", code)
	}
	if !strings.Contains(out, "0") {
		t.Fatalf("out = %q, want zero (uncommitted tx discarded on exit)", out)
	}
}

func TestShellCommitWithoutBegin(t *testing.T) {
	_, errb, code := runCLI(t, []string{}, ".commit\n")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errb, "no transaction is open") {
		t.Fatalf("stderr = %q", errb)
	}
}

func TestShellDoubleBegin(t *testing.T) {
	_, errb, code := runCLI(t, []string{}, ".begin\n.begin\n.rollback\n")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errb, "already open") {
		t.Fatalf("stderr = %q", errb)
	}
}

func TestShellBeginReadRejectsWrite(t *testing.T) {
	script := ".begin read\nCREATE (:Person);\n.rollback\n"
	_, _, code := runCLI(t, []string{}, script)
	if code == exitOK {
		t.Fatal("expected a write in a read transaction to fail")
	}
}

func TestShellBeginBadMode(t *testing.T) {
	_, errb, code := runCLI(t, []string{}, ".begin sideways\n")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errb, "read or write") {
		t.Fatalf("stderr = %q", errb)
	}
}
