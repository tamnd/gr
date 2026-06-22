package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/tamnd/gr"
)

// importOptions controls what .import / gr import loads (doc 17 §6.10, §7.2). This
// slice imports CSV/TSV rows as nodes; the relationship form (--as-rel/--from/--to)
// and Parquet are parsed and rejected with a clear message until their slices land.
type importOptions struct {
	file      string
	labels    []string          // --as (repeatable for multi-label)
	relType   string            // --as-rel TYPE
	from, to  string            // --from NS:COL / --to NS:COL
	idCol     string            // --id-col COL
	format    string            // csv|tsv|parquet (inferred from the extension)
	header    bool              // first row is a header (default yes)
	separator string            // field delimiter
	skip      int               // skip leading data rows
	limit     int               // cap rows imported (0 = no cap)
	types     map[string]string // --type COL:TYPE
	null      string            // --null STR
	merge     bool              // MERGE instead of CREATE
	batch     int               // commit every N rows
}

// importResult reports what an import wrote.
type importResult struct {
	nodes int
}

// parseImportArgs parses the import flags shared by the dot-command and the subcommand
// (doc 17 §6.10). Defaults: header on, batch 1000, format inferred from the file
// extension and falling back to csv.
func parseImportArgs(args []string) (importOptions, error) {
	opt := importOptions{header: true, batch: 1000, types: map[string]string{}}
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
		case "--as":
			var l string
			if l, err = next(); err == nil {
				opt.labels = append(opt.labels, l)
			}
		case "--as-rel":
			opt.relType, err = next()
		case "--from":
			opt.from, err = next()
		case "--to":
			opt.to, err = next()
		case "--id-col":
			opt.idCol, err = next()
		case "--format":
			opt.format, err = next()
		case "--separator":
			opt.separator, err = next()
		case "--null":
			opt.null, err = next()
		case "--header":
			opt.header = true
		case "--no-header":
			opt.header = false
		case "--merge":
			opt.merge = true
		case "--skip":
			opt.skip, err = nextInt(next)
		case "--limit":
			opt.limit, err = nextInt(next)
		case "--batch":
			opt.batch, err = nextInt(next)
		case "--type":
			var t string
			if t, err = next(); err == nil {
				col, typ, ok := strings.Cut(t, ":")
				if !ok {
					return opt, fmt.Errorf("--type wants COL:TYPE, got %q", t)
				}
				opt.types[col] = strings.ToUpper(typ)
			}
		default:
			if strings.HasPrefix(a, "-") {
				return opt, fmt.Errorf("unknown flag %q", a)
			}
			if opt.file != "" {
				return opt, fmt.Errorf("unexpected argument %q", a)
			}
			opt.file = a
		}
		if err != nil {
			return opt, err
		}
	}

	if opt.file == "" {
		return opt, fmt.Errorf("a source file is required")
	}
	if opt.relType != "" || opt.from != "" || opt.to != "" {
		return opt, fmt.Errorf("relationship import (--as-rel/--from/--to) is not yet supported; nodes only for now")
	}
	if len(opt.labels) == 0 {
		return opt, fmt.Errorf("--as LABEL is required")
	}
	if opt.merge && opt.idCol == "" {
		return opt, fmt.Errorf("--merge needs --id-col to key the upsert")
	}
	opt.format = resolveFormat(opt.format, opt.file)
	if opt.format == "parquet" {
		return opt, fmt.Errorf("parquet import is not yet supported")
	}
	if opt.batch < 1 {
		opt.batch = 1
	}
	return opt, nil
}

// nextInt reads the next argument as an integer.
func nextInt(next func() (string, error)) (int, error) {
	s, err := next()
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("expected a number, got %q", s)
	}
	return n, nil
}

// runImport opens the file and loads its rows as nodes (doc 17 §6.10). The reader and
// the database write path are wired so the file streams row by row and commits in
// batches, never holding the whole file in memory.
func runImport(db *gr.DB, opt importOptions) (importResult, error) {
	f, err := os.Open(opt.file)
	if err != nil {
		return importResult{}, err
	}
	defer func() { _ = f.Close() }()
	return importNodes(db, opt, f)
}

// importNodes reads CSV/TSV rows and writes a node per row (doc 17 §6.10). Each row's
// columns become the node's properties, coerced by --type and dropped when blank or
// equal to --null. With --merge it upserts on the --id-col key; otherwise it creates.
func importNodes(db *gr.DB, opt importOptions, r io.Reader) (importResult, error) {
	cr := csv.NewReader(r)
	cr.Comma = importComma(opt)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; short rows leave properties unset

	header, err := importHeader(cr, opt)
	if err != nil {
		return importResult{}, err
	}

	ctx := context.Background()
	var res importResult
	skipped := 0
	tx, err := db.Begin(ctx, gr.Write)
	if err != nil {
		return res, err
	}
	inBatch := 0
	abort := func() {
		if tx != nil {
			_ = tx.Rollback()
			tx = nil
		}
	}
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			abort()
			return res, err
		}
		if skipped < opt.skip {
			skipped++
			continue
		}
		if opt.limit > 0 && res.nodes >= opt.limit {
			break
		}

		stmt, params := opt.nodeStatement(header, row)
		out, err := tx.Run(ctx, stmt, params)
		if err == nil {
			err = out.Err()
			_ = out.Close()
		}
		if err != nil {
			abort()
			return res, fmt.Errorf("row %d: %w", res.nodes+skipped+1, err)
		}
		res.nodes++
		inBatch++
		if inBatch >= opt.batch {
			if err := tx.Commit(); err != nil {
				return res, err
			}
			if tx, err = db.Begin(ctx, gr.Write); err != nil {
				return res, err
			}
			inBatch = 0
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// importComma resolves the field delimiter: an explicit --separator, else a tab for
// tsv, else a comma.
func importComma(opt importOptions) rune {
	if opt.separator != "" {
		return []rune(opt.separator)[0]
	}
	if opt.format == "tsv" {
		return '\t'
	}
	return ','
}

// importHeader returns the column names: the first row when --header is set, or
// generated positional names (col1, col2, ...) when --no-header, sized to the first
// data row, which is then pushed back by re-reading. With --no-header the reader cannot
// unread, so the names are sized from a peeked first row that the caller does not skip.
func importHeader(cr *csv.Reader, opt importOptions) ([]string, error) {
	if opt.header {
		row, err := cr.Read()
		if err == io.EOF {
			return nil, fmt.Errorf("empty file: no header row")
		}
		return row, err
	}
	// No header: peek the first row to size positional names. encoding/csv has no
	// unread, so the names are needed before the row is consumed; instead generate a
	// generous set and index by position, tolerating short rows.
	return nil, nil
}

// nodeStatement builds the Cypher and parameters for one node row (doc 17 §6.10). The
// property values are passed as a parameter map so the values never go through string
// quoting, and the labels and merge key are fixed text.
func (opt importOptions) nodeStatement(header, row []string) (string, gr.Params) {
	props := gr.Params{}
	for i, cell := range row {
		name := columnName(header, i)
		if name == "" || opt.isNull(cell) {
			continue
		}
		props[name] = opt.coerce(name, cell)
	}

	var labels strings.Builder
	for _, l := range opt.labels {
		labels.WriteByte(':')
		labels.WriteString(quoteIdent(l))
	}

	if opt.merge {
		key := opt.idCol
		keyVal := props[key]
		var b strings.Builder
		fmt.Fprintf(&b, "MERGE (n%s {%s: $key})", labels.String(), quoteIdent(key))
		set := make([]string, 0, len(props))
		params := gr.Params{"key": keyVal}
		for name, v := range props {
			if name == key {
				continue
			}
			p := "p_" + name
			set = append(set, fmt.Sprintf("n.%s = $%s", quoteIdent(name), p))
			params[p] = v
		}
		if len(set) > 0 {
			fmt.Fprintf(&b, " SET %s", strings.Join(set, ", "))
		}
		return b.String(), params
	}

	assigns := make([]string, 0, len(props))
	params := gr.Params{}
	for name, v := range props {
		p := "p_" + name
		assigns = append(assigns, fmt.Sprintf("%s: $%s", quoteIdent(name), p))
		params[p] = v
	}
	return fmt.Sprintf("CREATE (n%s {%s})", labels.String(), strings.Join(assigns, ", ")), params
}

// columnName returns the name of column i: the header name when present, else a
// positional col<N> name for a headerless import.
func columnName(header []string, i int) string {
	if i < len(header) {
		return header[i]
	}
	return fmt.Sprintf("col%d", i+1)
}

// isNull reports whether a cell should be dropped rather than stored: an empty cell is
// always absent, and a cell equal to --null is null (doc 17 §4.5).
func (opt importOptions) isNull(cell string) bool {
	if cell == "" {
		return true
	}
	return opt.null != "" && cell == opt.null
}

// coerce converts a cell to the type named for its column by --type, defaulting to a
// string when no type is given or the value does not parse (doc 17 §6.10). An
// unparseable typed cell falls back to the raw string rather than failing the row.
func (opt importOptions) coerce(name, cell string) gr.Value {
	switch opt.types[name] {
	case "INTEGER", "INT":
		if n, err := strconv.ParseInt(cell, 10, 64); err == nil {
			return n
		}
	case "FLOAT", "DOUBLE":
		if f, err := strconv.ParseFloat(cell, 64); err == nil {
			return f
		}
	case "BOOLEAN", "BOOL":
		if b, err := strconv.ParseBool(cell); err == nil {
			return b
		}
	}
	return cell
}

// dotImport loads a file into the graph (doc 17 §6.10). It refuses while an explicit
// transaction is open, since it manages its own batched transactions.
func (s *shell) dotImport(args []string) {
	opt, err := parseImportArgs(args)
	if err != nil {
		fmt.Fprintf(s.errw, "Error: .import: %v\n", err)
		s.code = worst(s.code, exitUsage)
		return
	}
	if s.tx != nil {
		fmt.Fprintln(s.errw, "Error: commit or rollback the open transaction before .import")
		s.code = worst(s.code, exitUsage)
		return
	}
	res, err := runImport(s.db, opt)
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
		return
	}
	fmt.Fprintf(s.errw, "imported %d nodes\n", res.nodes)
}

// runImportCmd implements the `gr import` subcommand (doc 17 §7.2): it opens a database
// read-write and loads CSV/TSV rows as nodes.
func runImportCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "Usage: gr import DATABASE FILE --as LABEL [--id-col COL] [--type COL:TYPE] [--merge] [--no-header]")
		return exitUsage
	}
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gr: import needs a database")
		return exitUsage
	}
	dbPath := args[0]
	opt, err := parseImportArgs(args[1:])
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitUsage
	}
	db, err := gr.Open(dbPath, gr.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()
	res, err := runImport(db, opt)
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return classify(err)
	}
	fmt.Fprintf(stderr, "imported %d nodes\n", res.nodes)
	return exitOK
}
