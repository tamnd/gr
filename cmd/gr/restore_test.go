package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestoreSubcommand confirms gr restore --force replaces a destination database
// with a backup and removes the destination's stale -wal sidecar (doc 17 §7.6).
func TestRestoreSubcommand(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.gr")
	dst := filepath.Join(dir, "live.gr")
	seedDB(t, src)

	// Start the destination as a different database with leftover content.
	if _, _, code := runCLI(t, []string{dst, "-c", "CREATE (:Old {tag:'stale'})"}, ""); code != exitOK {
		t.Fatalf("seed dst: code=%d", code)
	}
	// A leftover -wal must not survive the restore and shadow the new file.
	if err := os.WriteFile(dst+"-wal", []byte("stale wal"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, errb, code := runCLI(t, []string{"restore", dst, src, "--force"}, "")
	if code != exitOK {
		t.Fatalf("restore: code=%d stderr=%q", code, errb)
	}
	if _, err := os.Stat(dst + "-wal"); !os.IsNotExist(err) {
		t.Errorf("restore left the stale -wal sidecar in place")
	}

	// The destination now holds the backup's graph, not its own old content.
	out, _, code := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (n:Person) RETURN count(n) AS c"}, "")
	if code != exitOK || !strings.Contains(out, `"c":2`) {
		t.Errorf("restored data missing the Person nodes: code=%d out=%s", code, out)
	}
	gone, _, _ := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (n:Old) RETURN count(n) AS c"}, "")
	if !strings.Contains(gone, `"c":0`) {
		t.Errorf("restore did not replace the old content: %s", gone)
	}
}

// TestRestoreSubcommandNeedsForceWhenPiped confirms a non-terminal restore without
// --force refuses rather than destroying the destination unprompted.
func TestRestoreSubcommandNeedsForceWhenPiped(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.gr")
	dst := filepath.Join(dir, "live.gr")
	seedDB(t, src)
	seedDB(t, dst)
	before, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}

	_, errb, code := runCLI(t, []string{"restore", dst, src}, "")
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "pass --force") {
		t.Errorf("stderr = %q", errb)
	}
	after, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("destination changed despite the refusal")
	}
}

// TestRestoreSubcommandRejectsBadBackup confirms an invalid source is rejected before
// the destination is touched.
func TestRestoreSubcommandRejectsBadBackup(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "junk.gr")
	dst := filepath.Join(dir, "live.gr")
	if err := os.WriteFile(src, []byte("this is not a gr file"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDB(t, dst)
	before, _ := os.ReadFile(dst)

	if _, _, code := runCLI(t, []string{"restore", dst, src, "--force"}, ""); code != exitOpen {
		t.Errorf("code = %d, want exitOpen", code)
	}
	after, _ := os.ReadFile(dst)
	if len(before) != len(after) {
		t.Errorf("a bad backup overwrote the destination")
	}
}

// TestRestoreSubcommandArgs confirms gr restore needs both a destination and a source.
func TestRestoreSubcommandArgs(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "live.gr")
	seedDB(t, dst)
	if _, _, code := runCLI(t, []string{"restore", dst}, ""); code != exitUsage {
		t.Errorf("one arg: code = %d, want exitUsage", code)
	}
}

// TestDotRestoreInScript confirms .restore --force replaces the open database in a
// script run, where there is no terminal to prompt.
func TestDotRestoreInScript(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.gr")
	live := filepath.Join(dir, "live.gr")
	seedDB(t, src)
	if _, _, code := runCLI(t, []string{live, "-c", "CREATE (:Old {tag:'stale'})"}, ""); code != exitOK {
		t.Fatalf("seed live: code=%d", code)
	}

	script := ".restore " + src + " --force\nMATCH (n:Person) RETURN count(n);\n"
	out, errb, code := runCLI(t, []string{live, "--mode", "jsonl"}, script)
	if code != exitOK {
		t.Fatalf("restore script: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "2") {
		t.Errorf("restored database missing data: out=%s", out)
	}
	// The query ran against the restored database in the same session.
	gone, _, _ := runCLI(t, []string{live, "--mode", "jsonl", "-c", "MATCH (n:Old) RETURN count(n) AS c"}, "")
	if !strings.Contains(gone, `"c":0`) {
		t.Errorf(".restore did not replace the open database: %s", gone)
	}
}

// TestDotRestoreNeedsForceInScript confirms .restore without --force refuses in a
// non-interactive script rather than destroying the open database.
func TestDotRestoreNeedsForceInScript(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.gr")
	live := filepath.Join(dir, "live.gr")
	seedDB(t, src)
	seedDB(t, live)

	script := ".restore " + src + "\n"
	_, errb, code := runCLI(t, []string{live}, script)
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "pass --force") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestDotRestoreRefusesInMemory confirms .restore refuses on a transient database,
// where there is no file to replace.
func TestDotRestoreRefusesInMemory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.gr")
	seedDB(t, src)

	script := ".restore " + src + " --force\n"
	_, errb, code := runCLI(t, []string{}, script)
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "file-backed database") {
		t.Errorf("stderr = %q", errb)
	}
}

// TestDotRestoreRefusesOpenTransaction confirms .restore is refused while an explicit
// transaction is open.
func TestDotRestoreRefusesOpenTransaction(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "backup.gr")
	live := filepath.Join(dir, "live.gr")
	seedDB(t, src)
	seedDB(t, live)

	script := ".begin\nCREATE (:X);\n.restore " + src + " --force\n.rollback\n"
	_, errb, code := runCLI(t, []string{live}, script)
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "commit or rollback the open transaction") {
		t.Errorf("stderr = %q", errb)
	}
}
