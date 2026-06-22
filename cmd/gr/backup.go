package main

import (
	"fmt"
	"io"
	"os"

	"github.com/tamnd/gr"
)

// dotSave writes the current database to a file as a standalone .gr image (doc 17
// §6.13). It is the in-memory-to-file operation, effectively .backup for a
// transient database, and is also valid for a file database.
func (s *shell) dotSave(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, "Error: .save needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	s.backupTo(".save", args[0])
}

// dotBackup makes a consistent physical backup of the current database to a file
// (doc 17 §6.13). The copy is a self-contained .gr file that opens directly.
func (s *shell) dotBackup(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, "Error: .backup needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	s.backupTo(".backup", args[0])
}

// backupTo copies the open database to target, refusing while an explicit
// transaction is open: a backup must capture committed state, and an open
// transaction's writes are not yet committed (doc 17 §6.13, §6.4).
func (s *shell) backupTo(cmd, target string) {
	if s.tx != nil {
		fmt.Fprintf(s.errw, "Error: commit or rollback the open transaction before %s\n", cmd)
		s.code = worst(s.code, exitUsage)
		return
	}
	f, err := os.Create(target)
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitIO)
		return
	}
	n, err := s.db.Backup(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
		return
	}
	fmt.Fprintf(s.errw, "backed up %s to %s\n", humanBytes(n), target)
}

// runBackupCmd implements the `gr backup` subcommand (doc 17 §7.6): it opens a
// database read-only and writes a consistent physical copy to a destination file,
// or to stdout when the destination is "-".
func runBackupCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "Usage: gr backup SOURCE DEST")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Copy a consistent physical image of SOURCE to DEST (\"-\" for stdout).")
		return exitUsage
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, "gr: backup needs a source and a destination")
		return exitUsage
	}
	src, dst := args[0], args[1]

	db, err := gr.Open(src, gr.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	w := stdout
	var out *os.File
	if dst != "-" {
		out, err = os.Create(dst)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitIO
		}
		w = out
	}
	n, err := db.Backup(w)
	if out != nil {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return classify(err)
	}
	if dst != "-" {
		fmt.Fprintf(stderr, "backed up %s to %s\n", humanBytes(n), dst)
	}
	return exitOK
}
