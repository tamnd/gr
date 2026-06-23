// Command gr is the command-line front end to the gr graph database (spec 2060 doc
// 17): an interactive Cypher shell, a one-shot query runner, and a script runner over
// a single .gr file or a transient in-memory database. It speaks the same library API
// the embedded use does, so a query behaves the same from the shell as from Go.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

const version = "0.1.0"

// memPath is the synthetic path a transient in-memory database is opened under (doc
// 17 §2.4). It is never written to a real filesystem; the in-memory VFS backs it.
const memPath = ":memory:.gr"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable entry point: it parses the arguments, opens the database, runs
// the requested work (a one-shot statement, a script, or the interactive shell), and
// returns the process exit code (doc 17 §10). Data goes to stdout, chatter to stderr,
// so a pipeline reads clean output.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return runServe(args[1:], stdout, stderr, startHTTP, startBolt)
		case "dump":
			return runDumpCmd(args[1:], stdout, stderr)
		case "load":
			return runLoadCmd(args[1:], stdin, stdout, stderr)
		case "import":
			return runImportCmd(args[1:], stdout, stderr)
		case "export":
			return runExportCmd(args[1:], stdout, stderr)
		case "info":
			return runInfoCmd(args[1:], stdout, stderr)
		case "health":
			return runHealthCmd(args[1:], stdout, stderr)
		case "backup":
			return runBackupCmd(args[1:], stdout, stderr)
		case "restore":
			return runRestoreCmd(args[1:], stdin, stdout, stderr)
		}
	}
	cfg, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	if cfg.showHelp {
		printUsage(stdout)
		return exitOK
	}
	if cfg.showVersion {
		fmt.Fprintln(stdout, "gr", version)
		return exitOK
	}

	db, err := openDatabase(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	interactive := decideInteractive(cfg, stdin, stdout)
	sh := newShell(db, cfg, stdout, stderr, interactive)
	defer sh.closeSinks()

	if cfg.output != "" {
		sh.dotOutput([]string{cfg.output})
	}

	switch {
	case len(cfg.cyphers) > 0 || cfg.trailing != "":
		for _, c := range cfg.cyphers {
			sh.runStatement(c)
			if sh.bail && sh.code != exitOK {
				return sh.code
			}
		}
		if cfg.trailing != "" {
			sh.runStatement(cfg.trailing)
		}
	case cfg.file != "":
		f, err := os.Open(cfg.file)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitOpen
		}
		sh.runScript(f)
		_ = f.Close()
		if cfg.interactive {
			sh.repl(stdin)
		}
	case interactive:
		sh.repl(stdin)
	default:
		sh.runScript(stdin)
	}
	return sh.code
}

// openDatabase opens the database the config names, resolving the create/read-only
// rules (doc 17 §2.3, §2.4). A transient in-memory database is opened over an
// in-memory VFS; a file database honours --readonly and --no-create against whether
// the file exists.
func openDatabase(cfg config) (*gr.DB, error) {
	if cfg.dbPath == "" || cfg.dbPath == ":memory:" {
		return gr.Open(memPath, gr.Options{VFS: vfs.NewMem()})
	}
	_, statErr := os.Stat(cfg.dbPath)
	exists := statErr == nil
	if !exists {
		if cfg.readonly {
			return nil, fmt.Errorf("cannot create a read-only database: %s", cfg.dbPath)
		}
		if cfg.noCreate {
			return nil, fmt.Errorf("file not found: %s", cfg.dbPath)
		}
	}
	return gr.Open(cfg.dbPath, gr.Options{ReadOnly: cfg.readonly})
}

// dotOpen closes the current database and opens another file in its place (doc 17
// §3.2, §6.3). The session settings carry over; only the database changes.
func (s *shell) dotOpen(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, "Error: .open needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	if s.tx != nil {
		fmt.Fprintln(s.errw, "Error: commit or rollback the open transaction before .open")
		s.code = worst(s.code, exitUsage)
		return
	}
	path := args[0]
	var db *gr.DB
	var err error
	if path == ":memory:" {
		db, err = gr.Open(memPath, gr.Options{VFS: vfs.NewMem()})
	} else {
		db, err = gr.Open(path, gr.Options{ReadOnly: s.readonly})
	}
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitOpen)
		return
	}
	_ = s.db.Close()
	s.db = db
}

// decideInteractive resolves whether to run the interactive shell (doc 17 §9.1). The
// explicit --interactive and --batch flags win; otherwise the shell is interactive
// only when there is no one-shot or script work and both stdin and stdout are a
// terminal.
func decideInteractive(cfg config, stdin io.Reader, stdout io.Writer) bool {
	if cfg.interactive {
		return true
	}
	if cfg.batch {
		return false
	}
	if len(cfg.cyphers) > 0 || cfg.trailing != "" || cfg.file != "" {
		return false
	}
	return isTerminal(stdin) && isTerminal(stdout)
}

// isTerminal reports whether an I/O value is a character device (a TTY). It works
// without a build dependency by inspecting the file mode, and returns false for any
// value that is not an *os.File (a pipe, a buffer, a redirected file).
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// printUsage prints the tool's help text (doc 17 §2.2, §7.13).
func printUsage(w io.Writer) {
	const usage = `gr - embedded graph database (Cypher)

Usage:
  gr [flags] [database] [statement]
  gr serve [flags] [database]
  gr dump  [flags] database
  gr load  [flags] database
  gr import database file --as label | --as-rel type --from L:col --to L:col
  gr export database --nodes|--rels|--query ... --to file
  gr info  database
  gr health database
  gr backup source dest
  gr restore dest source

Open the interactive shell on a database, run a one-shot statement, or run a
script. With no database argument gr opens a transient in-memory database.

The serve subcommand serves the HTTP/JSON API over a database; the dump and
load subcommands write and replay a logical Cypher dump; the info subcommand
prints a database's facts; the health subcommand prints its live health report
and exits non-zero when the engine cannot serve; the backup and restore
subcommands copy a consistent physical image and replace a database from one. Run
"gr serve -h", "gr dump -h", or "gr load -h" for their flags.

Flags:
  -c, --cypher STMT     Run one statement and exit (repeatable)
  -f, --file FILE       Run a script of statements, then exit (unless -i)
      --mode MODE       Output mode: table, column, csv, tsv, ascii, json,
                        jsonl, markdown, html, list, line, quote
      --headers on|off  Show column headers
      --separator STR   Field separator for csv/ascii modes
      --nullvalue STR   Text printed for a null value
  -o, --output FILE     Send query output to FILE
      --timer on|off    Print timing after each statement
      --echo            Echo each statement before running it
      --bail            Stop at the first error in a script
  -r, --readonly        Open the database read-only
      --no-create       Do not create the database if it is missing
  -i, --interactive     Force the interactive shell
      --batch           Force non-interactive mode
  -q, --quiet           Suppress the banner and summaries
  -V, --version         Print the version and exit
  -h, --help            Print this help and exit

Examples:
  gr social.gr
  gr social.gr -c "MATCH (p:Person) RETURN count(p)"
  gr social.gr -f setup.cypher
  gr social.gr --mode json -c "MATCH (n) RETURN n LIMIT 10"`
	fmt.Fprintln(w, strings.TrimSpace(usage))
}
