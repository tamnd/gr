package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tamnd/gr"
)

// runDot dispatches a dot-command line (doc 17 §6). The command word selects the
// action; arguments are shell-tokenized. An unknown command prints a diagnostic with a
// near-miss suggestion and returns to the prompt without ending the session.
func (s *shell) runDot(line string) {
	toks := tokenizeArgs(line)
	if len(toks) == 0 {
		return
	}
	cmd := toks[0]
	args := toks[1:]
	switch cmd {
	case ".help":
		s.dotHelp()
	case ".quit", ".exit":
		s.quitNow = true
	case ".version":
		fmt.Fprintln(s.errw, "gr", version)
	case ".mode":
		s.dotMode(args)
	case ".headers", ".header":
		s.dotToggle(args, &s.headers, &s.headerSet, "headers")
	case ".timer":
		s.dotToggle(args, &s.timer, nil, "timer")
	case ".echo":
		s.dotToggle(args, &s.echo, nil, "echo")
	case ".nullvalue":
		if len(args) > 0 {
			s.null = args[0]
		}
	case ".separator":
		if len(args) > 0 {
			s.separator = args[0]
		}
	case ".output":
		s.dotOutput(args)
	case ".once":
		s.dotOnce(args)
	case ".read":
		s.dotRead(args)
	case ".open":
		s.dotOpen(args)
	case ".begin":
		s.dotBegin(args)
	case ".commit":
		s.dotCommit()
	case ".rollback":
		s.dotRollback()
	case ".labels":
		s.dotLabels()
	case ".types":
		s.dotTypes()
	case ".indexes", ".index":
		s.dotIndexes()
	case ".schema":
		s.dotSchema()
	case ".databases":
		s.dotDatabases()
	case ".info":
		s.dotInfo()
	case ".health":
		s.dotHealth()
	case ".dump":
		s.dotDump(args)
	case ".load":
		s.dotLoad(args)
	case ".import":
		s.dotImport(args)
	case ".export":
		s.dotExport(args)
	case ".save":
		s.dotSave(args)
	case ".backup":
		s.dotBackup(args)
	case ".restore":
		s.dotRestore(args)
	case ".print":
		fmt.Fprintln(s.sink(), strings.Join(args, " "))
	default:
		fmt.Fprintf(s.errw, "Error: unknown command %q%s\n", cmd, suggestDot(cmd))
		s.code = worst(s.code, exitUsage)
	}
}

// dotHelp prints the dot-command catalogue summary (doc 17 §6.2).
func (s *shell) dotHelp() {
	const help = `.help                 Show this message
.mode MODE            Set output mode (table, column, csv, tsv, ascii,
                      json, jsonl, markdown, html, list, line, quote)
.headers on|off       Show or hide column headers
.nullvalue STR        Text printed for a null value
.separator STR        Field separator for csv/ascii modes
.output [FILE]        Send query output to FILE (no arg restores stdout)
.once FILE            Send the next query's output to FILE
.read FILE            Run the statements in FILE
.begin [read|write]   Open an explicit transaction
.commit               Commit the open transaction
.rollback             Discard the open transaction
.open FILE            Close the current database and open FILE
.labels               List the node labels
.types                List the relationship types
.indexes              List the schema indexes
.schema               Show the labels, types, keys, and indexes
.databases            Show the open database and its path
.info                 Show the database and engine facts
.health               Show the live health report (state, gauges, warnings)
.dump [FILE]          Write a Cypher dump of the database (--schema-only,
                      --data-only); no FILE writes to the output sink
.load FILE            Replay a Cypher dump into the database
.import FILE OPTS     Import CSV/TSV rows as nodes (--as LABEL) or as
                      relationships (--as-rel TYPE --from L:COL --to L:COL);
                      --id-col, --type COL:TYPE, --merge, --no-header
.export OPTS          Export nodes, relationships, or a query to CSV/TSV
                      (--nodes L | --rels T | --query Q) --to FILE;
                      --from-property/--to-property relink a --rels export
.backup FILE          Write a consistent physical backup to FILE
.restore FILE [--force]  Replace the database from a physical backup
.save FILE            Write the database to FILE as a standalone .gr file
.timer on|off         Show wall-clock timing after each statement
.echo on|off          Echo each statement before running it
.print TEXT           Print TEXT to the output
.version              Show the gr version
.quit, .exit          Exit the shell`
	fmt.Fprintln(s.errw, help)
}

// dotMode sets the output mode, or prints the current mode with no argument (doc 17
// §5.1).
func (s *shell) dotMode(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, s.mode)
		return
	}
	if !validMode(args[0]) {
		fmt.Fprintf(s.errw, "Error: unknown output mode %q\n", args[0])
		s.code = worst(s.code, exitUsage)
		return
	}
	s.mode = args[0]
}

// dotToggle parses an on/off argument for a boolean setting (doc 17 §4.4, §4.7). With
// no argument it flips the current value. When setFlag is non-nil it records that the
// setting was explicitly chosen so the mode default no longer applies.
func (s *shell) dotToggle(args []string, target, setFlag *bool, name string) {
	if len(args) == 0 {
		*target = !*target
	} else {
		on, err := parseOnOff(args[0])
		if err != nil {
			fmt.Fprintf(s.errw, "Error: .%s %v\n", name, err)
			s.code = worst(s.code, exitUsage)
			return
		}
		*target = on
	}
	if setFlag != nil {
		*setFlag = true
	}
}

// dotOutput redirects data output to a file, or restores stdout (doc 17 §4.6).
func (s *shell) dotOutput(args []string) {
	if s.dataFile != nil {
		_ = s.dataFile.Close()
		s.dataFile = nil
	}
	if len(args) == 0 || args[0] == "stdout" {
		s.dataOut = s.stdout
		return
	}
	f, err := os.Create(args[0])
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitIO)
		return
	}
	s.dataFile = f
	s.dataOut = f
}

// dotOnce redirects only the next statement's output to a file (doc 17 §4.6).
func (s *shell) dotOnce(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, "Error: .once needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	f, err := os.Create(args[0])
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitIO)
		return
	}
	s.onceFile = f
}

// dotRead runs the statements in a script file (doc 17 §6.9).
func (s *shell) dotRead(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, "Error: .read needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	f, err := os.Open(args[0])
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, exitIO)
		return
	}
	defer func() { _ = f.Close() }()
	s.runScript(f)
}

// dotBegin opens an explicit transaction the following statements run inside until a
// .commit or .rollback (doc 17 §6.4). The mode defaults to write, or read on a
// read-only database; an explicit "read" or "write" argument overrides it. Statements
// run between .begin and .commit see each other's writes and land atomically.
func (s *shell) dotBegin(args []string) {
	if s.tx != nil {
		fmt.Fprintln(s.errw, "Error: a transaction is already open; commit or rollback it first")
		s.code = worst(s.code, exitUsage)
		return
	}
	mode := gr.Write
	if s.readonly {
		mode = gr.Read
	}
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "read":
			mode = gr.Read
		case "write":
			if s.readonly {
				fmt.Fprintln(s.errw, "Error: cannot begin a write transaction on a read-only database")
				s.code = worst(s.code, exitReadOnly)
				return
			}
			mode = gr.Write
		default:
			fmt.Fprintf(s.errw, "Error: .begin expects read or write, got %q\n", args[0])
			s.code = worst(s.code, exitUsage)
			return
		}
	}
	tx, err := s.db.Begin(context.Background(), mode)
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
		return
	}
	s.tx = tx
}

// dotCommit commits the open explicit transaction (doc 17 §6.4). With no transaction
// open it is a usage error rather than a silent no-op.
func (s *shell) dotCommit() {
	if s.tx == nil {
		fmt.Fprintln(s.errw, "Error: no transaction is open")
		s.code = worst(s.code, exitUsage)
		return
	}
	err := s.tx.Commit()
	s.tx = nil
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
	}
}

// dotRollback discards the open explicit transaction (doc 17 §6.4).
func (s *shell) dotRollback() {
	if s.tx == nil {
		fmt.Fprintln(s.errw, "Error: no transaction is open")
		s.code = worst(s.code, exitUsage)
		return
	}
	err := s.tx.Rollback()
	s.tx = nil
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
	}
}

// dotLabels prints the node labels the catalog holds, one per line in sorted order
// (doc 17 §6.5). The listing goes to the data sink, so it can be piped.
func (s *shell) dotLabels() {
	names, err := s.db.Labels()
	if err != nil {
		s.reportListErr(err)
		return
	}
	s.printNames(names)
}

// dotTypes prints the relationship types the catalog holds (doc 17 §6.5).
func (s *shell) dotTypes() {
	names, err := s.db.RelationshipTypes()
	if err != nil {
		s.reportListErr(err)
		return
	}
	s.printNames(names)
}

// dotIndexes prints the schema indexes, one per line as name on label(props) (doc 17
// §6.5).
func (s *shell) dotIndexes() {
	ixs, err := s.db.Indexes()
	if err != nil {
		s.reportListErr(err)
		return
	}
	lines := make([]string, len(ixs))
	for i, ix := range ixs {
		lines[i] = fmt.Sprintf("%s on :%s(%s)", ix.Name, ix.Label, strings.Join(ix.Props, ", "))
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Fprintln(s.sink(), l)
	}
}

// dotSchema prints a summary of the database schema: its labels, relationship types,
// property keys, and indexes, each section labelled (doc 17 §6.5). It is the at-a-
// glance view a session opens with, so it goes to the chatter channel like the other
// descriptive commands.
func (s *shell) dotSchema() {
	labels, err := s.db.Labels()
	if err != nil {
		s.reportListErr(err)
		return
	}
	types, err := s.db.RelationshipTypes()
	if err != nil {
		s.reportListErr(err)
		return
	}
	keys, err := s.db.PropertyKeys()
	if err != nil {
		s.reportListErr(err)
		return
	}
	ixs, err := s.db.Indexes()
	if err != nil {
		s.reportListErr(err)
		return
	}
	section := func(title string, items []string) {
		sorted := append([]string(nil), items...)
		sort.Strings(sorted)
		if len(sorted) == 0 {
			fmt.Fprintf(s.errw, "%s: (none)\n", title)
			return
		}
		fmt.Fprintf(s.errw, "%s: %s\n", title, strings.Join(sorted, ", "))
	}
	section("Labels", labels)
	section("Relationship types", types)
	section("Property keys", keys)
	ixLines := make([]string, len(ixs))
	for i, ix := range ixs {
		ixLines[i] = fmt.Sprintf("%s on :%s(%s)", ix.Name, ix.Label, strings.Join(ix.Props, ", "))
	}
	section("Indexes", ixLines)
}

// printNames writes a sorted list of names to the data sink, one per line.
func (s *shell) printNames(names []string) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for _, n := range sorted {
		fmt.Fprintln(s.sink(), n)
	}
}

// reportListErr reports a schema-listing error and folds its code into the exit code.
func (s *shell) reportListErr(err error) {
	fmt.Fprintln(s.errw, "Error:", err)
	s.code = worst(s.code, classify(err))
}

// dotDatabases prints the open database and its path (doc 17 §6.3).
func (s *shell) dotDatabases() {
	path := s.db.Path()
	if path == memPath {
		path = ":memory:"
	}
	fmt.Fprintf(s.errw, "main: %s\n", path)
}

// suggestDot offers a near-miss suggestion for an unknown dot-command (doc 17 §3.4).
func suggestDot(cmd string) string {
	known := []string{
		".help", ".quit", ".exit", ".version", ".mode", ".headers", ".timer",
		".echo", ".nullvalue", ".separator", ".output", ".once", ".read",
		".begin", ".commit", ".rollback", ".open", ".labels", ".types",
		".indexes", ".schema", ".databases", ".info", ".health", ".dump",
		".load", ".import", ".export", ".backup", ".restore", ".save", ".print",
	}
	for _, k := range known {
		if near(cmd, k) {
			return fmt.Sprintf("; did you mean %q?", k)
		}
	}
	return ""
}
