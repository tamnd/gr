package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupRoundTrip confirms .backup writes a standalone .gr file that opens with
// the same data and schema, byte-identical to the source (doc 17 §6.13).
func TestBackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	dst := filepath.Join(dir, "dst.gr")
	seedDB(t, src)

	if _, errb, code := runCLI(t, []string{src, "-c", ".backup " + dst}, ""); code != exitOK {
		t.Fatalf(".backup: code=%d stderr=%q", code, errb)
	}

	// The copy opens and carries the data.
	out, _, code := runCLI(t, []string{dst, "--mode", "jsonl", "-c",
		"MATCH (a)-[r:KNOWS]->(b) RETURN a.name AS s, b.name AS e, r.since AS since"}, "")
	if code != exitOK || !strings.Contains(out, `"s":"Ada","e":"Lin","since":2019`) {
		t.Errorf("backup lost the relationship: code=%d out=%s", code, out)
	}
	// The index came along.
	ix, _, _ := runCLI(t, []string{dst, "-c", ".indexes"}, "")
	if !strings.Contains(ix, "person_age on :Person(age)") {
		t.Errorf("backup lost the index: %s", ix)
	}

	// A physical backup is byte-identical to the source.
	a, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("backup is not byte-identical: src %d bytes, dst %d bytes", len(a), len(b))
	}
}

// TestBackupSubcommandToStdout exercises gr backup SRC - streaming to stdout.
func TestBackupSubcommandToStdout(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	dst := filepath.Join(dir, "via-stream.gr")
	seedDB(t, src)

	out, _, code := runCLI(t, []string{"backup", src, "-"}, "")
	if code != exitOK {
		t.Fatalf("gr backup -: code=%d", code)
	}
	if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	q, _, code := runCLI(t, []string{dst, "-c", "MATCH (n:Person) RETURN count(n)"}, "")
	if code != exitOK || !strings.Contains(q, "2") {
		t.Errorf("streamed backup lost data: code=%d out=%s", code, q)
	}
}

// TestSaveInMemory confirms .save writes a transient in-memory database to a file
// that then opens with its data (doc 17 §6.13).
func TestSaveInMemory(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "snap.gr")
	script := "CREATE (:Note {t:'hi'});\n.save " + dst + "\n"
	if _, errb, code := runCLI(t, []string{}, script); code != exitOK {
		t.Fatalf(".save: code=%d stderr=%q", code, errb)
	}
	out, _, code := runCLI(t, []string{dst, "--mode", "jsonl", "-c", "MATCH (n:Note) RETURN n.t AS t"}, "")
	if code != exitOK || !strings.Contains(out, `"t":"hi"`) {
		t.Errorf("saved database lost data: code=%d out=%s", code, out)
	}
}

// TestBackupRefusesOpenTransaction confirms a backup is refused while an explicit
// transaction is open, since it must capture committed state.
func TestBackupRefusesOpenTransaction(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	out := filepath.Join(dir, "out.gr")
	seedDB(t, src)
	script := ".begin\nCREATE (:X);\n.backup " + out + "\n.rollback\n"
	_, errb, code := runCLI(t, []string{src}, script)
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "commit or rollback the open transaction") {
		t.Errorf("stderr = %q", errb)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("backup file was written despite the open transaction")
	}
}

// TestBackupSubcommandArgs confirms gr backup needs a source and a destination.
func TestBackupSubcommandArgs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gr")
	seedDB(t, src)
	if _, _, code := runCLI(t, []string{"backup", src}, ""); code != exitUsage {
		t.Errorf("one arg: code = %d, want exitUsage", code)
	}
}
