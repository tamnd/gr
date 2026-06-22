package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tamnd/gr"
)

// dumpOptions controls what a logical dump emits (doc 17 §13.2, §13.8). The default
// is a full, round-trippable dump; schemaOnly emits just the DDL and dataOnly just
// the data, the two composable halves of a full dump.
type dumpOptions struct {
	schemaOnly bool
	dataOnly   bool
}

// dumpResult reports what a dump wrote, so the caller can print a one-line summary.
type dumpResult struct {
	nodes, rels        int
	constraints, index int
}

// writeDump serializes a database to the textual Cypher dump form (doc 17 §13): a
// comment header, the schema DDL, the nodes as identity-preserving MERGE statements,
// the relationships as endpoint-matching MERGE statements, and a completion marker.
// The whole dump reads from one snapshot (db.View), so it is internally consistent
// even while the database is written concurrently. The "generated" header line is
// emitted only when generated is non-empty, since the dump otherwise has no clock.
func writeDump(db *gr.DB, w io.Writer, source, generated string, opt dumpOptions) (dumpResult, error) {
	bw := bufio.NewWriter(w)
	var res dumpResult
	err := db.View(func(tx *gr.Tx) error {
		cons, err := db.Constraints()
		if err != nil {
			return err
		}
		ixs, err := db.Indexes()
		if err != nil {
			return err
		}
		nodes, err := countOnTx(tx, "MATCH (n) RETURN count(n)")
		if err != nil {
			return err
		}
		rels, err := countOnTx(tx, "MATCH ()-[r]->() RETURN count(r)")
		if err != nil {
			return err
		}
		res = dumpResult{nodes: nodes, rels: rels, constraints: len(cons), index: len(ixs)}

		fmt.Fprintln(bw, "// gr dump v1")
		fmt.Fprintf(bw, "// source: %s (gr format v1, %d nodes, %d relationships)\n", source, nodes, rels)
		if generated != "" {
			fmt.Fprintf(bw, "// generated: %s\n", generated)
		}
		fmt.Fprintln(bw)

		if !opt.dataOnly {
			fmt.Fprintln(bw, "// --- schema ---")
			for _, c := range cons {
				fmt.Fprintln(bw, constraintDDL(c))
			}
			for _, ix := range ixs {
				fmt.Fprintln(bw, indexDDL(ix))
			}
			fmt.Fprintln(bw)
		}

		if !opt.schemaOnly {
			fmt.Fprintln(bw, "// --- nodes ---")
			if err := dumpNodes(tx, bw); err != nil {
				return err
			}
			fmt.Fprintln(bw)
			fmt.Fprintln(bw, "// --- relationships ---")
			if err := dumpRels(tx, bw); err != nil {
				return err
			}
			fmt.Fprintln(bw)
		}

		fmt.Fprintf(bw, "// gr dump complete: %d nodes, %d relationships, %d constraints, %d indexes\n",
			nodes, rels, len(cons), len(ixs))
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, bw.Flush()
}

// countOnTx runs a single-column count query on a transaction and returns the count.
func countOnTx(tx *gr.Tx, cypher string) (int, error) {
	r, err := tx.Run(context.Background(), cypher, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	if !r.Next() {
		return 0, nil
	}
	n, err := r.Record().GetInt(r.Keys()[0])
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// dumpNodes writes every node as an identity-preserving MERGE (doc 17 §13.3): the
// node is keyed by a synthetic `~id` property carrying its element id and tagged with
// a transient `~n` label so the relationship section can match it fast, then its real
// labels and properties are written with a trailing SET.
func dumpNodes(tx *gr.Tx, bw *bufio.Writer) error {
	r, err := tx.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		n, err := r.Record().GetNode("n")
		if err != nil {
			return err
		}
		fmt.Fprintf(bw, "MERGE (n:`~n` {`~id`:%s})", quoteCypherString(n.ElementId()))
		if set := nodeSetClause(n); set != "" {
			fmt.Fprintf(bw, " SET %s", set)
		}
		fmt.Fprintln(bw, ";")
	}
	return r.Err()
}

// nodeSetClause builds the SET tail that gives a merged node its real labels and
// properties: "n:Label1:Label2, n.key = value, ...". It is empty for a node with no
// labels and no properties, where the MERGE alone suffices.
func nodeSetClause(n gr.Node) string {
	var parts []string
	if labels := n.Labels(); len(labels) > 0 {
		var b strings.Builder
		b.WriteString("n")
		for _, l := range labels {
			b.WriteByte(':')
			b.WriteString(quoteIdent(l))
		}
		parts = append(parts, b.String())
	}
	for _, k := range n.Keys() {
		v, _ := n.Get(k)
		parts = append(parts, fmt.Sprintf("n.%s = %s", quoteIdent(k), renderQuote(v)))
	}
	return strings.Join(parts, ", ")
}

// dumpRels writes every relationship as a MATCH of its two endpoints by their `~id`
// followed by a MERGE of the relationship between them (doc 17 §13.3), so on reload
// each relationship re-links to the same two nodes. The relationship's own `~id` keeps
// a reload idempotent, and a trailing SET writes its properties.
func dumpRels(tx *gr.Tx, bw *bufio.Writer) error {
	r, err := tx.Run(context.Background(), "MATCH ()-[r]->() RETURN r", nil)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		rel, err := r.Record().GetRelationship("r")
		if err != nil {
			return err
		}
		fmt.Fprintf(bw, "MATCH (a:`~n` {`~id`:%s}), (b:`~n` {`~id`:%s})\n",
			quoteCypherString(rel.StartElementId()), quoteCypherString(rel.EndElementId()))
		fmt.Fprintf(bw, "  MERGE (a)-[r:%s {`~id`:%s}]->(b)",
			quoteIdent(rel.Type()), quoteCypherString(rel.ElementId()))
		if set := relSetClause(rel); set != "" {
			fmt.Fprintf(bw, " SET %s", set)
		}
		fmt.Fprintln(bw, ";")
	}
	return r.Err()
}

// relSetClause builds the SET tail that gives a merged relationship its properties.
func relSetClause(rel gr.Relationship) string {
	var parts []string
	for _, k := range rel.Keys() {
		v, _ := rel.Get(k)
		parts = append(parts, fmt.Sprintf("r.%s = %s", quoteIdent(k), renderQuote(v)))
	}
	return strings.Join(parts, ", ")
}

// constraintDDL renders a constraint as the CREATE CONSTRAINT ... IF NOT EXISTS DDL
// that re-creates it on load (doc 08 §4.1, doc 17 §13.2). The predicate after REQUIRE
// follows the constraint's kind: IS UNIQUE, IS NOT NULL, or IS :: TYPE.
func constraintDDL(c gr.ConstraintInfo) string {
	prop := "?"
	if len(c.Props) > 0 {
		prop = quoteIdent(c.Props[0])
	}
	var require string
	switch c.Kind {
	case "EXISTS":
		require = "IS NOT NULL"
	case "TYPE":
		require = "IS :: " + c.PropType
	default:
		require = "IS UNIQUE"
	}
	return fmt.Sprintf("CREATE CONSTRAINT %s IF NOT EXISTS FOR (n:%s) REQUIRE n.%s %s;",
		quoteIdent(c.Name), quoteIdent(c.Label), prop, require)
}

// indexDDL renders an index as the CREATE INDEX ... IF NOT EXISTS DDL (doc 07 §4, doc
// 17 §13.2).
func indexDDL(ix gr.IndexInfo) string {
	prop := "?"
	if len(ix.Props) > 0 {
		prop = quoteIdent(ix.Props[0])
	}
	return fmt.Sprintf("CREATE INDEX %s IF NOT EXISTS FOR (n:%s) ON (n.%s);",
		quoteIdent(ix.Name), quoteIdent(ix.Label), prop)
}

// dotDump serializes the database to a Cypher dump (doc 17 §6.12). With a FILE argument
// it writes there; with none it writes to the current output sink, so ".dump" pipes
// through an .output redirect. The --schema-only and --data-only flags split the two
// halves of a full dump.
func (s *shell) dotDump(args []string) {
	var (
		opt    dumpOptions
		target string
	)
	for _, a := range args {
		switch a {
		case "--schema-only":
			opt.schemaOnly = true
		case "--data-only":
			opt.dataOnly = true
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(s.errw, "Error: .dump: unknown flag %q\n", a)
				s.code = worst(s.code, exitUsage)
				return
			}
			target = a
		}
	}
	if opt.schemaOnly && opt.dataOnly {
		fmt.Fprintln(s.errw, "Error: .dump: --schema-only and --data-only are mutually exclusive")
		s.code = worst(s.code, exitUsage)
		return
	}

	w := s.sink()
	if target != "" {
		f, err := os.Create(target)
		if err != nil {
			fmt.Fprintln(s.errw, "Error:", err)
			s.code = worst(s.code, exitIO)
			return
		}
		defer func() { _ = f.Close() }()
		w = f
	}

	res, err := writeDump(s.db, w, s.dumpSource(), time.Now().UTC().Format(time.RFC3339), opt)
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
		return
	}
	if target != "" {
		fmt.Fprintf(s.errw, "dumped %d nodes, %d relationships to %s\n", res.nodes, res.rels, target)
	}
}

// dotLoad replays a dump into the current database (doc 17 §6.12).
func (s *shell) dotLoad(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(s.errw, "Error: .load needs a file argument")
		s.code = worst(s.code, exitUsage)
		return
	}
	if s.tx != nil {
		fmt.Fprintln(s.errw, "Error: commit or rollback the open transaction before .load")
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
	rep, err := loadDump(s.db, f)
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", dumpErrText(err))
		s.code = worst(s.code, classifyDumpErr(err))
		return
	}
	fmt.Fprintf(s.errw, "loaded %d nodes, %d relationships\n", rep.nodes, rep.rels)
}

// dumpSource is the source name a dump's header records: the database file path, or
// :memory: for a transient database.
func (s *shell) dumpSource() string {
	if p := s.db.Path(); p != memPath {
		return p
	}
	return ":memory:"
}

// dumpErrText prefixes a truncated-dump failure with "data error:" so the message
// reads as the spec's example does (doc 17 §13.6); other failures print as-is.
func dumpErrText(err error) string {
	if errors.Is(err, errTruncatedDump) {
		return "data error: " + err.Error()
	}
	return err.Error()
}

// classifyDumpErr maps a load failure to an exit code: a file that is not a dump is a
// format error (doc 17 §13.5), a truncated dump is a data error (§13.6), anything else
// follows the general classifier.
func classifyDumpErr(err error) int {
	switch {
	case errors.Is(err, errNotADump):
		return exitFormat
	case errors.Is(err, errTruncatedDump):
		return exitData
	default:
		return classify(err)
	}
}

// errNotADump and errTruncatedDump are the two load failures the caller maps to exit
// codes: a file that is not a dump at all (no header marker) and a dump cut off before
// its completion marker (doc 17 §13.5, §13.6).
var (
	errNotADump      = fmt.Errorf("not a gr dump (no \"// gr dump\" header found)")
	errTruncatedDump = fmt.Errorf("dump is truncated (no completion marker found)")
)

// loadReport reports what a load replayed, drawn from the post-load element counts.
type loadReport struct {
	nodes, rels int
}

// loadDump reads a textual dump and replays it into db (doc 17 §13.5): it detects the
// dump header, refuses a truncated dump, creates a transient endpoint index so the
// relationship matches are seeks, replays the statements, strips the `~id`/`~n`
// scaffolding, and drops the transient index, leaving the clean graph. The replay
// runs statement by statement on the auto-commit path; a statement that fails aborts
// the load and surfaces the error.
func loadDump(db *gr.DB, r io.Reader) (loadReport, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return loadReport{}, err
	}
	text := string(data)
	if !strings.Contains(text, "// gr dump") {
		return loadReport{}, errNotADump
	}
	if !strings.Contains(text, "// gr dump complete") {
		return loadReport{}, errTruncatedDump
	}

	// The transient endpoint index makes each relationship's `~id` MATCH a seek
	// rather than a scan, which keeps relationship loading near-linear (doc 17 §13.5).
	if err := execLoad(db, "CREATE INDEX `~n~id` IF NOT EXISTS FOR (n:`~n`) ON (n.`~id`)"); err != nil {
		return loadReport{}, err
	}

	stmts, _ := splitStatements(text)
	for _, st := range stmts {
		if err := execLoad(db, st); err != nil {
			return loadReport{}, fmt.Errorf("replaying %.60q: %w", st, err)
		}
	}

	// Strip the scaffolding: remove the `~n` marker label and the `~id` property from
	// every element, leaving exactly the source's labels and properties (doc 17 §13.3).
	if err := execLoad(db, "MATCH (n:`~n`) REMOVE n:`~n` SET n.`~id` = null"); err != nil {
		return loadReport{}, err
	}
	if err := execLoad(db, "MATCH ()-[r]->() SET r.`~id` = null"); err != nil {
		return loadReport{}, err
	}
	if err := execLoad(db, "DROP INDEX `~n~id` IF EXISTS"); err != nil {
		return loadReport{}, err
	}

	var rep loadReport
	err = db.View(func(tx *gr.Tx) error {
		if rep.nodes, err = countOnTx(tx, "MATCH (n) RETURN count(n)"); err != nil {
			return err
		}
		rep.rels, err = countOnTx(tx, "MATCH ()-[r]->() RETURN count(r)")
		return err
	})
	return rep, err
}

// execLoad runs one load statement and discards its result rows.
func execLoad(db *gr.DB, cypher string) error {
	res, err := db.Run(context.Background(), cypher, nil)
	if err != nil {
		return err
	}
	if cerr := res.Err(); cerr != nil {
		_ = res.Close()
		return cerr
	}
	return res.Close()
}

// quoteIdent returns a Cypher identifier, backtick-quoting it when it is not a plain
// identifier (a leading letter or underscore followed by letters, digits, or
// underscores). The scaffolding names `~n` and `~id` always quote, since `~` is not a
// plain identifier character.
func quoteIdent(s string) string {
	if isPlainIdent(s) {
		return s
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// isPlainIdent reports whether s is a bare Cypher identifier needing no backticks.
func isPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
