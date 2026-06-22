package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tamnd/gr"
)

// shell is the interactive and batch driver over an open database (doc 17 §3). It
// holds the mutable session settings a dot-command changes (output mode, headers,
// the null and separator strings, the timer and echo toggles, the data sink) and the
// accumulated exit code, and it runs both Cypher statements and dot-commands against
// the database.
type shell struct {
	db  *gr.DB
	cfg config

	mode      string
	headers   bool
	headerSet bool
	separator string
	null      string
	timer     bool
	echo      bool
	bail      bool
	readonly  bool

	interactive bool

	dataOut  io.Writer // current data sink: stdout or an .output file
	dataFile *os.File  // the open .output file, nil when the sink is stdout
	onceFile *os.File  // an .once file, redirected for the next statement only
	stdout   io.Writer // the default data sink
	errw     io.Writer // the chatter sink (stderr)

	tx *gr.Tx // the open explicit transaction, nil outside one

	code    int  // accumulated worst exit code
	quitNow bool // set by .quit/.exit
}

// newShell builds a shell over an open database with the settings the config resolved
// (doc 17 §2.2). The output mode defaults to table on an interactive terminal and csv
// off one, so the same command is pretty for a human and parseable for a pipe.
func newShell(db *gr.DB, cfg config, stdout, errw io.Writer, interactive bool) *shell {
	mode := cfg.mode
	if mode == "" {
		if interactive {
			mode = "table"
		} else {
			mode = "csv"
		}
	}
	sh := &shell{
		db:          db,
		cfg:         cfg,
		mode:        mode,
		headers:     cfg.headers,
		headerSet:   cfg.headerSet,
		separator:   cfg.separator,
		null:        cfg.null,
		timer:       cfg.timer,
		echo:        cfg.echo,
		bail:        cfg.bail,
		readonly:    cfg.readonly,
		interactive: interactive,
		stdout:      stdout,
		dataOut:     stdout,
		errw:        errw,
	}
	return sh
}

// effectiveHeaders resolves whether a header row prints (doc 17 §4.4): table and
// markdown default to on, the delimited modes follow the flag (off unless set), and
// json/jsonl ignore the setting entirely.
func (s *shell) effectiveHeaders() bool {
	if s.headerSet {
		return s.headers
	}
	switch canonicalMode(s.mode) {
	case "table", "column", "markdown", "html":
		return true
	default:
		return false
	}
}

// sink returns the data writer for the next statement, honouring a pending .once
// redirect (doc 17 §4.6).
func (s *shell) sink() io.Writer {
	if s.onceFile != nil {
		return s.onceFile
	}
	return s.dataOut
}

// afterStatement reverts a .once redirect once the statement it captured has run.
func (s *shell) afterStatement() {
	if s.onceFile != nil {
		_ = s.onceFile.Close()
		s.onceFile = nil
	}
}

// runStatement runs one Cypher statement and renders its result (doc 17 §3.1). A
// compile or runtime error prints a diagnostic to the chatter channel and updates the
// accumulated exit code without ending the session.
func (s *shell) runStatement(stmt string) {
	if s.echo {
		fmt.Fprintln(s.errw, stmt)
	}
	start := time.Now()
	var (
		res *gr.Result
		err error
	)
	if s.tx != nil {
		res, err = s.tx.Run(context.Background(), stmt, nil)
	} else {
		res, err = s.db.Run(context.Background(), stmt, nil)
	}
	if err != nil {
		s.reportError(err)
		return
	}
	rows := s.render(res)
	elapsed := time.Since(start)
	if cerr := res.Err(); cerr != nil {
		_ = res.Close()
		s.reportError(cerr)
		return
	}
	summary := res.Summary()
	_ = res.Close()
	s.printSummary(rows, elapsed, summary)
	s.afterStatement()
}

// render streams the result rows through the current formatter to the data sink and
// returns the row count (doc 17 §5). The header decision is resolved per mode.
func (s *shell) render(res *gr.Result) int {
	opts := formatOpts{
		headers:   s.effectiveHeaders(),
		separator: s.separator,
		null:      s.nullText(),
	}
	f := newFormatter(s.mode, s.sink(), opts)
	f.begin(res.Keys())
	rows := 0
	for res.Next() {
		f.row(res.Record().Values())
		rows++
	}
	f.end()
	return rows
}

// nullText is the text printed for a null value (doc 17 §4.5): the configured null
// string, or the table mode's (null) placeholder when none is set.
func (s *shell) nullText() string {
	if s.null != "" {
		return s.null
	}
	if canonicalMode(s.mode) == "table" {
		return "(null)"
	}
	return ""
}

// printSummary writes the one-line statement summary to the chatter channel (doc 17
// §4.2). It is suppressed in the machine-readable modes unless --verbose, and in quiet
// mode, so a pipe never sees a human-readable trailer on its data.
func (s *shell) printSummary(rows int, elapsed time.Duration, sum gr.Summary) {
	if s.cfg.quiet {
		return
	}
	switch canonicalMode(s.mode) {
	case "csv", "tsv", "ascii", "json", "jsonl", "html", "markdown":
		return
	}
	noun := "rows"
	if rows == 1 {
		noun = "row"
	}
	line := fmt.Sprintf("%d %s (%.1f ms)", rows, noun, float64(elapsed.Microseconds())/1000)
	if w := writeCounts(sum); w != "" {
		line += "  [" + w + "]"
	}
	fmt.Fprintln(s.errw, line)
}

// reportError prints a diagnostic and folds the error's code into the accumulated
// exit code (doc 17 §8.1, §8.2).
func (s *shell) reportError(err error) {
	fmt.Fprintln(s.errw, "Error:", err)
	s.code = worst(s.code, classify(err))
	s.afterStatement()
}

// writeCounts renders the write-count bracket from a statement summary (doc 17 §4.3).
// It lists only the non-zero counters, so a read reports nothing and a write reports
// exactly what it changed.
func writeCounts(s gr.Summary) string {
	var parts []string
	add := func(n int, one, many string) {
		if n == 0 {
			return
		}
		if n == 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, one))
		} else {
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(s.NodesCreated, "node created", "nodes created")
	add(s.NodesDeleted, "node deleted", "nodes deleted")
	add(s.RelationshipsCreated, "relationship created", "relationships created")
	add(s.RelationshipsDeleted, "relationship deleted", "relationships deleted")
	add(s.PropertiesSet, "property set", "properties set")
	add(s.LabelsAdded, "label added", "labels added")
	add(s.LabelsRemoved, "label removed", "labels removed")
	add(s.IndexesAdded, "index created", "indexes created")
	add(s.IndexesRemoved, "index dropped", "indexes dropped")
	add(s.ConstraintsAdded, "constraint created", "constraints created")
	add(s.ConstraintsRemoved, "constraint dropped", "constraints dropped")
	return strings.Join(parts, ", ")
}

// runScript reads statements and dot-commands from r and runs them in order (doc 17
// §2.6). A Cypher statement is assembled across lines until its terminating
// semicolon; a dot-command runs on its own line when no statement is mid-entry. With
// --bail the run stops at the first error.
func (s *shell) runScript(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var buf strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if buf.Len() == 0 && isDotCommand(line) {
			s.runDot(line)
			if s.quitNow || (s.bail && s.code != exitOK) {
				return
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		stmts, rest := splitStatements(buf.String())
		buf.Reset()
		buf.WriteString(rest)
		for _, st := range stmts {
			s.runStatement(st)
			if s.bail && s.code != exitOK {
				return
			}
		}
	}
	if t := strings.TrimSpace(buf.String()); t != "" {
		s.runStatement(t)
	}
}

// repl runs the interactive read-eval-print loop (doc 17 §3.1). It prints the prompt,
// reads a line, runs a dot-command immediately or accumulates Cypher until its
// terminator, and renders each statement's result. It ends on .quit, .exit, or
// end-of-input.
func (s *shell) repl(r io.Reader) {
	if !s.cfg.quiet {
		fmt.Fprintln(s.errw, "gr - embedded graph database (Cypher).  Type \".help\" for commands, \".quit\" to exit.")
		if s.db.Path() == memPath {
			fmt.Fprintln(s.errw, "Connected to a transient in-memory database.")
		} else {
			fmt.Fprintf(s.errw, "Connected to %s.\n", s.db.Path())
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var buf strings.Builder
	for {
		fmt.Fprint(s.errw, s.prompt(buf.Len() > 0))
		if !sc.Scan() {
			fmt.Fprintln(s.errw)
			return
		}
		line := sc.Text()
		if buf.Len() == 0 && isDotCommand(line) {
			s.runDot(line)
			if s.quitNow {
				return
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		stmts, rest := splitStatements(buf.String())
		buf.Reset()
		buf.WriteString(rest)
		for _, st := range stmts {
			s.runStatement(st)
		}
	}
}

// prompt returns the prompt string for the current state (doc 17 §3.2): the
// continuation prompt mid-statement, the read-only marker on a read-only database, and
// the primary prompt otherwise.
func (s *shell) prompt(midStatement bool) string {
	if midStatement {
		return "...> "
	}
	if s.tx != nil {
		return "gr*> "
	}
	if s.readonly {
		return "gr(ro)> "
	}
	return "gr> "
}

// closeSinks closes any open .output or .once file at shell shutdown, and rolls back
// an open explicit transaction so an uncommitted .begin discards rather than commits
// on exit (doc 17 §3.18, §6.4). An interrupted session leaves the database unchanged.
func (s *shell) closeSinks() {
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
	}
	if s.dataFile != nil {
		_ = s.dataFile.Close()
		s.dataFile = nil
	}
	if s.onceFile != nil {
		_ = s.onceFile.Close()
		s.onceFile = nil
	}
}
