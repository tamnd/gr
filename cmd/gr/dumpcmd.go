package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// parseInterleaved parses a flag set that allows flags to appear after positional
// arguments, which Go's flag package does not do on its own: it parses flags up to the
// first positional, records it, and resumes, so "gr dump db.gr -o out.cypher" works
// like "gr dump -o out.cypher db.gr". It returns the collected positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// runDumpCmd implements the `gr dump` subcommand (doc 17 §7.4): it opens a database
// read-only and writes its logical Cypher dump to a file or stdout. It parses its own
// flag set, since the dump options do not overlap the shell's.
func runDumpCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gr dump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var out string
	fs.StringVar(&out, "output", "", "write the dump to FILE (default stdout)")
	fs.StringVar(&out, "o", "", "write the dump to FILE (shorthand)")
	schemaOnly := fs.Bool("schema-only", false, "dump only the schema (constraints and indexes)")
	dataOnly := fs.Bool("data-only", false, "dump only the data (nodes and relationships)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gr dump [flags] DATABASE")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Write a logical Cypher dump of a database to a file or stdout.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}
	if *schemaOnly && *dataOnly {
		fmt.Fprintln(stderr, "gr: --schema-only and --data-only are mutually exclusive")
		return exitUsage
	}
	if len(pos) == 0 {
		fmt.Fprintln(stderr, "gr: dump needs a database argument")
		return exitUsage
	}
	path := pos[0]

	db, err := gr.Open(path, gr.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	w := stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitIO
		}
		defer func() { _ = f.Close() }()
		w = f
	}

	opt := dumpOptions{schemaOnly: *schemaOnly, dataOnly: *dataOnly}
	res, err := writeDump(db, w, path, time.Now().UTC().Format(time.RFC3339), opt)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return classify(err)
	}
	if out != "" {
		fmt.Fprintf(stderr, "dumped %d nodes, %d relationships to %s\n", res.nodes, res.rels, out)
	}
	return exitOK
}

// runLoadCmd implements the `gr load` subcommand (doc 17 §7.5): it opens (creating if
// missing) a database and replays a dump into it from a file or stdin.
func runLoadCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gr load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var file string
	fs.StringVar(&file, "file", "", "the dump to load (default stdin)")
	fs.StringVar(&file, "f", "", "the dump to load (shorthand)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gr load [flags] DATABASE")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Replay a logical Cypher dump into a database, creating it if missing.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) == 0 {
		fmt.Fprintln(stderr, "gr: load needs a database argument")
		return exitUsage
	}
	path := pos[0]

	var src = stdin
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			fmt.Fprintln(stderr, "gr:", err)
			return exitIO
		}
		defer func() { _ = f.Close() }()
		src = f
	}

	var db *gr.DB
	if path == ":memory:" {
		db, err = gr.Open(memPath, gr.Options{VFS: vfs.NewMem()})
	} else {
		db, err = gr.Open(path, gr.Options{})
	}
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	rep, err := loadDump(db, src)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", dumpErrText(err))
		return classifyDumpErr(err)
	}
	fmt.Fprintf(stderr, "loaded %d nodes, %d relationships\n", rep.nodes, rep.rels)
	return exitOK
}
