package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tamnd/gr"
)

// validateBackup confirms path opens as a valid gr file before a restore overwrites
// anything (doc 17 §7.6). A corrupt or wrong-format source is rejected here, so a bad
// backup never replaces a live database.
func validateBackup(path string) error {
	db, err := gr.Open(path, gr.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	return db.Close()
}

// replaceFile copies src over dst and drops dst's stale -wal sidecar (doc 19 §5). It
// writes a temporary file in dst's directory and renames it into place, so a failure
// partway through leaves the old dst intact rather than a half-written file. The -wal
// is removed because the restored main file is a self-contained committed image and a
// leftover write-ahead log would otherwise shadow it on the next open.
func replaceFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".gr-restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = os.Remove(dst + "-wal")
	return nil
}

// runRestoreCmd implements the `gr restore` subcommand (doc 17 §7.6): it replaces a
// destination database with a physical backup. The order matches the spec example,
// `gr restore DEST SRC`, so the live file is named first. The operation is destructive,
// so it confirms on a terminal and requires --force off one.
func runRestoreCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	force := false
	var rest []string
	for _, a := range args {
		switch a {
		case "-h", "--help":
			fmt.Fprintln(stderr, "Usage: gr restore DEST SRC [--force]")
			fmt.Fprintln(stderr)
			fmt.Fprintln(stderr, "Replace DEST with the physical backup SRC. Prompts unless --force.")
			return exitUsage
		case "--force", "-f":
			force = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "gr: restore needs a destination and a source")
		return exitUsage
	}
	dst, src := rest[0], rest[1]

	if err := validateBackup(src); err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	ok, err := confirmDestructive(fmt.Sprintf("Replace %s with %s? This cannot be undone.", dst, src), force, stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	if !ok {
		fmt.Fprintln(stderr, "restore canceled")
		return exitOK
	}
	if err := replaceFile(dst, src); err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitIO
	}
	fmt.Fprintf(stderr, "restored %s from %s\n", dst, src)
	return exitOK
}

// confirmDestructive asks the user to confirm a destructive operation and returns
// true to proceed (doc 17 §7.6). With force it never asks; on a terminal it prompts
// and reads a yes/no answer; off a terminal without force it refuses, so a piped or
// scripted run never destroys data without an explicit --force.
func confirmDestructive(prompt string, force bool, stdin io.Reader, stderr io.Writer) (bool, error) {
	if force {
		return true, nil
	}
	if !isTerminal(stdin) {
		return false, fmt.Errorf("refusing a destructive operation without confirmation; pass --force")
	}
	fmt.Fprint(stderr, prompt+" [y/N] ")
	sc := bufio.NewScanner(stdin)
	if !sc.Scan() {
		return false, nil
	}
	return isYes(sc.Text()), nil
}

// isYes reports whether an answer is an affirmative y/yes (case-insensitive).
func isYes(s string) bool {
	a := strings.ToLower(strings.TrimSpace(s))
	return a == "y" || a == "yes"
}

// dotRestore replaces the open database with a physical backup (doc 17 §6.13). It is
// destructive, so it confirms on an interactive terminal and requires --force in a
// script. It refuses while an explicit transaction is open, and on a transient
// in-memory session, where there is no file to replace (use .open to load a backup).
func (s *shell) dotRestore(args []string) {
	force := false
	var rest []string
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		fmt.Fprintln(s.errw, "Error: .restore needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	if s.tx != nil {
		fmt.Fprintln(s.errw, "Error: commit or rollback the open transaction before .restore")
		s.code = worst(s.code, exitUsage)
		return
	}
	path := s.db.Path()
	if path == memPath {
		fmt.Fprintln(s.errw, "Error: .restore needs a file-backed database; use .open to load a backup")
		s.code = worst(s.code, exitUsage)
		return
	}
	src := rest[0]
	if err := validateBackup(src); err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitOpen)
		return
	}
	if !force {
		if !s.interactive {
			fmt.Fprintln(s.errw, "Error: .restore is destructive; pass --force to confirm in a script")
			s.code = worst(s.code, exitUsage)
			return
		}
		fmt.Fprintf(s.errw, "Replace %s with %s? This cannot be undone. [y/N] ", path, src)
		if s.in == nil || !s.in.Scan() || !isYes(s.in.Text()) {
			fmt.Fprintln(s.errw, "restore canceled")
			return
		}
	}
	_ = s.db.Close()
	if err := replaceFile(path, src); err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitIO)
	}
	db, err := gr.Open(path, gr.Options{ReadOnly: s.readonly})
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitOpen)
		s.db = nil
		return
	}
	s.db = db
	fmt.Fprintf(s.errw, "restored %s from %s\n", path, src)
}
