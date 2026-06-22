package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tamnd/gr"
)

// exportOptions controls what .export / gr export writes (doc 17 §6.11, §7.3). Exactly
// one of nodes, rels, or query selects the source; to names the output file ("-" for
// stdout); format and the delimited settings shape the file.
type exportOptions struct {
	nodes     string // --nodes LABEL
	rels      string // --rels TYPE
	query     string // --query CYPHER
	to        string // --to FILE ("-" for stdout)
	format    string // csv|tsv (inferred from the extension when empty)
	header    bool
	separator string
	idCol     string // the emitted node id column name (default _id)
}

// parseExportArgs parses the export flags shared by the dot-command and the subcommand
// (doc 17 §6.11). Defaults: header on, id column _id, format inferred from the target
// extension and falling back to csv.
func parseExportArgs(args []string) (exportOptions, error) {
	opt := exportOptions{header: true, idCol: "_id"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "--nodes":
			opt.nodes, err = next()
		case "--rels":
			opt.rels, err = next()
		case "--query":
			opt.query, err = next()
		case "--to":
			opt.to, err = next()
		case "--format":
			opt.format, err = next()
		case "--separator":
			opt.separator, err = next()
		case "--id-col":
			opt.idCol, err = next()
		case "--header":
			opt.header = true
		case "--no-header":
			opt.header = false
		default:
			return opt, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return opt, err
		}
	}

	set := 0
	for _, s := range []string{opt.nodes, opt.rels, opt.query} {
		if s != "" {
			set++
		}
	}
	if set == 0 {
		return opt, fmt.Errorf("one of --nodes, --rels, or --query is required")
	}
	if set > 1 {
		return opt, fmt.Errorf("--nodes, --rels, and --query are mutually exclusive")
	}
	if opt.to == "" {
		return opt, fmt.Errorf("--to FILE is required")
	}
	opt.format = resolveFormat(opt.format, opt.to)
	if opt.format != "csv" && opt.format != "tsv" {
		return opt, fmt.Errorf("unsupported format %q (csv or tsv)", opt.format)
	}
	return opt, nil
}

// resolveFormat picks the output format from an explicit flag, then the target's
// extension, defaulting to csv (doc 17 §6.11). Parquet is named by the spec but not
// yet built, so it is not resolved here.
func resolveFormat(format, to string) string {
	if format != "" {
		return format
	}
	switch strings.ToLower(filepath.Ext(to)) {
	case ".tsv":
		return "tsv"
	case ".parquet":
		return "parquet"
	default:
		return "csv"
	}
}

// newDelimited builds the csv or tsv row writer for an export, matching the quoting the
// shell's delimited formatter uses (RFC 4180 for csv, tab-separated for tsv).
func (opt exportOptions) newDelimited(w io.Writer) *delimited {
	b := base{w: w, opts: formatOpts{headers: opt.header, separator: opt.separator, null: ""}}
	if opt.format == "tsv" {
		return &delimited{base: b, sep: "\t"}
	}
	sep := opt.separator
	if sep == "" {
		sep = ","
	}
	return &delimited{base: b, sep: sep, rfc: true}
}

// runExport opens the file (or stdout) and writes the selected data, returning the row
// count and a noun for the summary (doc 17 §6.11, §7.3). The read runs under one
// snapshot so the export is internally consistent.
func runExport(db *gr.DB, opt exportOptions, stdout io.Writer) (int, string, error) {
	var w io.Writer = stdout
	var out *os.File
	if opt.to != "-" {
		f, err := os.Create(opt.to)
		if err != nil {
			return 0, "", err
		}
		out = f
		w = f
	}
	count, noun, err := exportData(db, opt, w)
	if out != nil {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}
	return count, noun, err
}

// exportData writes the selected source to w and returns the count and its noun.
func exportData(db *gr.DB, opt exportOptions, w io.Writer) (int, string, error) {
	d := opt.newDelimited(w)
	var (
		count int
		noun  string
	)
	err := db.View(func(tx *gr.Tx) error {
		switch {
		case opt.query != "":
			noun = "rows"
			sep := opt.separator
			if opt.format == "csv" && sep == "" {
				sep = ","
			}
			n, err := exportQuery(tx, opt.query, opt.format, opt.header, sep, w)
			count = n
			return err
		case opt.nodes != "":
			noun = "nodes"
			n, err := exportNodes(tx, opt, d)
			count = n
			return err
		default:
			noun = "relationships"
			n, err := exportRels(tx, opt, d)
			count = n
			return err
		}
	})
	return count, noun, err
}

// exportQuery writes a query result as delimited rows, reusing the shell's formatter so
// the file matches what `.mode csv` would print (doc 17 §6.11). The header follows the
// --header flag.
func exportQuery(tx *gr.Tx, query, format string, header bool, sep string, w io.Writer) (int, error) {
	r, err := tx.Run(context.Background(), query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	f := newFormatter(format, w, formatOpts{headers: header, separator: sep, null: ""})
	f.begin(r.Keys())
	n := 0
	for r.Next() {
		f.row(r.Record().Values())
		n++
	}
	f.end()
	return n, r.Err()
}

// exportNodes writes every node of a label as a row of its external id and properties
// (doc 17 §6.11). The columns are the id column followed by the union of property keys
// across the matched nodes, so the file round-trips back through an import. It scans
// once to settle the columns, then once to write the rows.
func exportNodes(tx *gr.Tx, opt exportOptions, d *delimited) (int, error) {
	q := "MATCH (n:" + quoteIdent(opt.nodes) + ") RETURN n"
	keys, err := collectKeys(tx, q, func(r *gr.Record) ([]string, error) {
		n, err := r.GetNode("n")
		if err != nil {
			return nil, err
		}
		return n.Keys(), nil
	})
	if err != nil {
		return 0, err
	}
	if opt.header {
		d.writeRow(append([]string{opt.idCol}, keys...))
	}
	r, err := tx.Run(context.Background(), q, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	n := 0
	for r.Next() {
		node, err := r.Record().GetNode("n")
		if err != nil {
			return n, err
		}
		cells := make([]string, 0, len(keys)+1)
		cells = append(cells, node.ElementId())
		for _, k := range keys {
			v, _ := node.Get(k)
			cells = append(cells, renderText(v, ""))
		}
		d.writeRow(cells)
		n++
	}
	return n, r.Err()
}

// exportRels writes every relationship of a type as a row of its endpoint ids and
// properties (doc 17 §6.11). The columns are _start, _end, then the union of property
// keys, the convention an import reads back with --from/--to.
func exportRels(tx *gr.Tx, opt exportOptions, d *delimited) (int, error) {
	q := "MATCH ()-[r:" + quoteIdent(opt.rels) + "]->() RETURN r"
	keys, err := collectKeys(tx, q, func(rec *gr.Record) ([]string, error) {
		rel, err := rec.GetRelationship("r")
		if err != nil {
			return nil, err
		}
		return rel.Keys(), nil
	})
	if err != nil {
		return 0, err
	}
	if opt.header {
		d.writeRow(append([]string{"_start", "_end"}, keys...))
	}
	r, err := tx.Run(context.Background(), q, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	n := 0
	for r.Next() {
		rel, err := r.Record().GetRelationship("r")
		if err != nil {
			return n, err
		}
		cells := make([]string, 0, len(keys)+2)
		cells = append(cells, rel.StartElementId(), rel.EndElementId())
		for _, k := range keys {
			v, _ := rel.Get(k)
			cells = append(cells, renderText(v, ""))
		}
		d.writeRow(cells)
		n++
	}
	return n, r.Err()
}

// collectKeys scans a query once and returns the sorted union of the property keys the
// extractor pulls from each record, so the export emits a stable column set even when
// elements carry different property sets.
func collectKeys(tx *gr.Tx, query string, keysOf func(*gr.Record) ([]string, error)) ([]string, error) {
	r, err := tx.Run(context.Background(), query, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	seen := map[string]struct{}{}
	for r.Next() {
		ks, err := keysOf(r.Record())
		if err != nil {
			return nil, err
		}
		for _, k := range ks {
			seen[k] = struct{}{}
		}
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// dotExport writes the graph or a query result to a file (doc 17 §6.11). It is the
// shell front for the same export the gr export subcommand runs.
func (s *shell) dotExport(args []string) {
	opt, err := parseExportArgs(args)
	if err != nil {
		fmt.Fprintf(s.errw, "Error: .export: %v\n", err)
		s.code = worst(s.code, exitUsage)
		return
	}
	count, noun, err := runExport(s.db, opt, s.stdout)
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
		return
	}
	fmt.Fprintf(s.errw, "exported %d %s to %s\n", count, noun, opt.to)
}

// runExportCmd implements the `gr export` subcommand (doc 17 §7.3): it opens a database
// read-only and writes the selected nodes, relationships, or query result to a file.
func runExportCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "Usage: gr export DATABASE (--nodes LABEL | --rels TYPE | --query CYPHER) --to FILE [--format csv|tsv] [--no-header]")
		return exitUsage
	}
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gr: export needs a database")
		return exitUsage
	}
	dbPath := args[0]
	opt, err := parseExportArgs(args[1:])
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	db, err := gr.Open(dbPath, gr.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()
	count, noun, err := runExport(db, opt, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return classify(err)
	}
	if opt.to != "-" {
		fmt.Fprintf(stderr, "exported %d %s to %s\n", count, noun, opt.to)
	}
	return exitOK
}
