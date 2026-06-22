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

// importOptions controls what .import / gr import loads (doc 17 §6.10, §7.2). It loads
// CSV/TSV rows as nodes (--as) or as relationships (--as-rel with --from/--to);
// Parquet is parsed and rejected with a clear message until its slice lands.
type importOptions struct {
	file           string
	labels         []string          // --as (repeatable for multi-label)
	relType        string            // --as-rel TYPE
	from, to       string            // --from LABEL:COL / --to LABEL:COL
	fromKey, toKey string            // --from-key / --to-key (node property to match)
	idCol          string            // --id-col COL
	format         string            // csv|tsv|parquet (inferred from the extension)
	header         bool              // first row is a header (default yes)
	separator      string            // field delimiter
	skip           int               // skip leading data rows
	limit          int               // cap rows imported (0 = no cap)
	types          map[string]string // --type COL:TYPE
	null           string            // --null STR
	merge          bool              // MERGE instead of CREATE
	batch          int               // commit every N rows
}

// importResult reports what an import wrote.
type importResult struct {
	nodes    int
	rels     int
	dangling int // relationship rows whose endpoints were not found
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
		case "--from-key":
			opt.fromKey, err = next()
		case "--to-key":
			opt.toKey, err = next()
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
	isRel := opt.relType != "" || opt.from != "" || opt.to != ""
	if isRel {
		if len(opt.labels) > 0 {
			return opt, fmt.Errorf("use --as for nodes or --as-rel for relationships, not both")
		}
		if opt.relType == "" {
			return opt, fmt.Errorf("--as-rel TYPE is required for relationship import")
		}
		if opt.from == "" || opt.to == "" {
			return opt, fmt.Errorf("relationship import needs --from LABEL:COL and --to LABEL:COL")
		}
		if _, _, ok := strings.Cut(opt.from, ":"); !ok {
			return opt, fmt.Errorf("--from wants LABEL:COL, got %q", opt.from)
		}
		if _, _, ok := strings.Cut(opt.to, ":"); !ok {
			return opt, fmt.Errorf("--to wants LABEL:COL, got %q", opt.to)
		}
	} else {
		if len(opt.labels) == 0 {
			return opt, fmt.Errorf("--as LABEL or --as-rel TYPE is required")
		}
		if opt.merge && opt.idCol == "" {
			return opt, fmt.Errorf("--merge needs --id-col to key the upsert")
		}
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
	if opt.relType != "" {
		return importRels(db, opt, f)
	}
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
	header = opt.applyTypedHeader(header)

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

// importRels reads CSV/TSV rows and writes a relationship per row (doc 17 §6.10, doc 19
// §7.3). Each row names its two endpoints by the external-id columns --from and --to;
// the relationship is created between the nodes whose match key holds those ids, and the
// remaining columns become its properties. A row whose endpoints are not found is a
// dangling endpoint (doc 19 §11.3): it is counted and skipped, not an error.
func importRels(db *gr.DB, opt importOptions, r io.Reader) (importResult, error) {
	cr := csv.NewReader(r)
	cr.Comma = importComma(opt)
	cr.FieldsPerRecord = -1

	header, err := importHeader(cr, opt)
	if err != nil {
		return importResult{}, err
	}

	header = opt.applyTypedHeader(header)
	plan, err := opt.relPlan(header)
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
	rowNum := 0
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			abort()
			return res, err
		}
		rowNum++
		if skipped < opt.skip {
			skipped++
			continue
		}
		if opt.limit > 0 && res.rels+res.dangling >= opt.limit {
			break
		}

		stmt, params, ok := plan.statement(row)
		if !ok {
			res.dangling++ // an endpoint id column was blank
			continue
		}
		out, err := tx.Run(ctx, stmt, params)
		created := false
		if err == nil {
			for out.Next() {
				created = true
			}
			err = out.Err()
			_ = out.Close()
		}
		if err != nil {
			abort()
			return res, fmt.Errorf("row %d: %w", rowNum, err)
		}
		if created {
			res.rels++
		} else {
			res.dangling++ // MATCH found no endpoint
		}
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

// relPlan holds the per-file facts a relationship import resolves once: the endpoint
// labels, the columns that carry the endpoint ids, the node properties to match those
// ids against, and the columns that become relationship properties.
type relPlan struct {
	opt                importOptions
	fromLabel, toLabel string
	fromIdx, toIdx     int
	fromKey, toKey     string
	header             []string
}

// relPlan resolves the endpoint columns and keys against the header (doc 19 §7.3). The
// match key is --from-key/--to-key, falling back to --id-col, then to the endpoint
// column name, so the common case (the rel column and the stored node property share a
// name) needs no extra flags.
func (opt importOptions) relPlan(header []string) (relPlan, error) {
	fromLabel, fromCol, _ := strings.Cut(opt.from, ":")
	toLabel, toCol, _ := strings.Cut(opt.to, ":")
	fromIdx, ok := columnIndex(header, fromCol)
	if !ok {
		return relPlan{}, fmt.Errorf("--from column %q not found in the header", fromCol)
	}
	toIdx, ok := columnIndex(header, toCol)
	if !ok {
		return relPlan{}, fmt.Errorf("--to column %q not found in the header", toCol)
	}
	return relPlan{
		opt:       opt,
		fromLabel: fromLabel,
		toLabel:   toLabel,
		fromIdx:   fromIdx,
		toIdx:     toIdx,
		fromKey:   firstNonEmpty(opt.fromKey, opt.idCol, fromCol),
		toKey:     firstNonEmpty(opt.toKey, opt.idCol, toCol),
		header:    header,
	}, nil
}

// statement builds the Cypher and parameters for one relationship row. It returns ok
// false when an endpoint id cell is blank, so the caller can count the row as dangling
// without running a query. The values are parameters, never spliced, and the labels,
// keys, and type are fixed text (backtick-quoted).
func (p relPlan) statement(row []string) (string, gr.Params, bool) {
	if p.fromIdx >= len(row) || p.toIdx >= len(row) {
		return "", nil, false
	}
	fromVal, toVal := row[p.fromIdx], row[p.toIdx]
	if fromVal == "" || toVal == "" {
		return "", nil, false
	}
	params := gr.Params{"from": fromVal, "to": toVal}
	match := fmt.Sprintf("MATCH (a:%s {%s: $from}), (b:%s {%s: $to})",
		quoteIdent(p.fromLabel), quoteIdent(p.fromKey),
		quoteIdent(p.toLabel), quoteIdent(p.toKey))

	var props []string
	for i, cell := range row {
		if i == p.fromIdx || i == p.toIdx {
			continue
		}
		name := columnName(p.header, i)
		if name == "" || p.opt.isNull(cell) {
			continue
		}
		pn := "p_" + name
		props = append(props, fmt.Sprintf("%s = $%s", quoteIdent(name), pn))
		params[pn] = p.opt.coerce(name, cell)
	}

	var b strings.Builder
	b.WriteString(match)
	if p.opt.merge {
		fmt.Fprintf(&b, " MERGE (a)-[r:%s]->(b)", quoteIdent(p.opt.relType))
		if len(props) > 0 {
			fmt.Fprintf(&b, " SET %s", joinSet(props))
		}
	} else {
		fmt.Fprintf(&b, " CREATE (a)-[r:%s]->(b)", quoteIdent(p.opt.relType))
		if len(props) > 0 {
			fmt.Fprintf(&b, " SET %s", joinSet(props))
		}
	}
	b.WriteString(" RETURN r")
	return b.String(), params, true
}

// joinSet joins "n.x = $p_x" fragments for a SET clause.
func joinSet(frags []string) string {
	for i, f := range frags {
		frags[i] = "r." + f
	}
	return strings.Join(frags, ", ")
}

// firstNonEmpty returns the first non-empty string of its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// columnIndex returns the position of a named column: its header index, or, for a
// headerless import, the parsed N-1 of a positional colN name.
func columnIndex(header []string, name string) (int, bool) {
	for i, h := range header {
		if h == name {
			return i, true
		}
	}
	if len(header) == 0 && strings.HasPrefix(name, "col") {
		if n, err := strconv.Atoi(name[len("col"):]); err == nil && n >= 1 {
			return n - 1, true
		}
	}
	return 0, false
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

// applyTypedHeader rewrites a header that carries neo4j-admin-style type annotations
// (doc 19 §6.1): a cell like "born:int" declares the property "born" of type integer.
// It returns the bare property names and registers each declared type in opt.types when
// no explicit --type already set it (so --type wins). A header is untyped if it carries
// no recognized type, in which case the cell is the property name verbatim. opt.types is
// a map, so writing through this value receiver persists to the caller.
func (opt importOptions) applyTypedHeader(header []string) []string {
	if header == nil {
		return nil
	}
	out := make([]string, len(header))
	for i, cell := range header {
		name, typ, typed := splitTypedHeader(cell)
		out[i] = name
		if typed && typ != "" {
			if _, set := opt.types[name]; !set {
				opt.types[name] = typ
			}
		}
	}
	return out
}

// splitTypedHeader parses one header cell as an optional "name:type" annotation (doc 19
// §6.1, §6.2). It returns the bare name, the coercion type for the row path (empty for
// types this path stores as a string), and whether the cell was a recognized type
// annotation. A cell with no colon, an empty name (a neo4j special column such as :ID,
// out of scope here), or an unrecognized type is kept verbatim as the name.
func splitTypedHeader(cell string) (name, coerceType string, typed bool) {
	n, typ, ok := strings.Cut(cell, ":")
	if !ok || n == "" {
		return cell, "", false
	}
	base := strings.TrimSuffix(typ, "[]") // tolerate a list suffix; the row path stores it as text
	switch strings.ToLower(base) {
	case "int", "long", "short", "byte":
		return n, "INTEGER", true
	case "float", "double":
		return n, "FLOAT", true
	case "boolean", "bool":
		return n, "BOOLEAN", true
	case "string", "char", "date", "datetime", "localdatetime", "time", "localtime", "duration", "point":
		return n, "", true // recognized, but stored as a string until the value model carries the type
	default:
		return cell, "", false
	}
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
	fmt.Fprintln(s.errw, importSummary(opt, res))
}

// importSummary phrases what an import wrote: a node count, or a relationship count with
// a note when some rows were dangling (an endpoint was not found).
func importSummary(opt importOptions, res importResult) string {
	if opt.relType == "" {
		return fmt.Sprintf("imported %d nodes", res.nodes)
	}
	if res.dangling > 0 {
		return fmt.Sprintf("imported %d relationships (%d rows skipped: endpoint not found)", res.rels, res.dangling)
	}
	return fmt.Sprintf("imported %d relationships", res.rels)
}

// runImportCmd implements the `gr import` subcommand (doc 17 §7.2): it opens a database
// read-write and loads CSV/TSV rows as nodes.
func runImportCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "Usage: gr import DATABASE FILE (--as LABEL | --as-rel TYPE --from LABEL:COL --to LABEL:COL) [--id-col COL] [--from-key COL] [--to-key COL] [--type COL:TYPE] [--merge] [--no-header]")
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
	fmt.Fprintln(stderr, importSummary(opt, res))
	return exitOK
}
