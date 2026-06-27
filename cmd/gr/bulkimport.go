package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/tamnd/gr/loader"
)

// bulkImportOptions holds the parsed flags for the bulk loader path of
// `gr import` (doc 17 §7.2). It is used when --nodes or --rels appears in the
// argument list, signaling the 4-pass offline loader rather than the
// transactional row-by-row path.
type bulkImportOptions struct {
	dbPath    string
	nodes     []nodeImportSpec
	rels      []relImportSpec
	skipBad   bool
	append    bool // --append: open for appending (not yet supported; reserved)
	delimiter string
}

// nodeImportSpec is one --nodes LABEL=FILE entry.
type nodeImportSpec struct {
	label string
	file  string
}

// relImportSpec is one --rels TYPE=FILE entry.
type relImportSpec struct {
	relType string
	file    string
}

// parseBulkImportArgs parses the bulk-import flag set. Returns (opts, true) when
// it finds --nodes or --rels; (zero, false) otherwise, so the caller can fall
// back to the transactional path.
func parseBulkImportArgs(args []string) (bulkImportOptions, bool, error) {
	// Quick check: is this a bulk import invocation?
	isBulk := false
	for _, a := range args {
		if a == "--nodes" || strings.HasPrefix(a, "--nodes=") ||
			a == "--rels" || strings.HasPrefix(a, "--rels=") {
			isBulk = true
			break
		}
	}
	if !isBulk {
		return bulkImportOptions{}, false, nil
	}

	var opt bulkImportOptions
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--nodes" || strings.HasPrefix(a, "--nodes="):
			var v string
			var err error
			if strings.HasPrefix(a, "--nodes=") {
				v = strings.TrimPrefix(a, "--nodes=")
			} else {
				v, err = next()
			}
			if err != nil {
				return opt, true, err
			}
			label, file, ok := strings.Cut(v, "=")
			if !ok {
				return opt, true, fmt.Errorf("--nodes expects LABEL=FILE, got %q", v)
			}
			opt.nodes = append(opt.nodes, nodeImportSpec{label: label, file: file})
		case a == "--rels" || strings.HasPrefix(a, "--rels="):
			var v string
			var err error
			if strings.HasPrefix(a, "--rels=") {
				v = strings.TrimPrefix(a, "--rels=")
			} else {
				v, err = next()
			}
			if err != nil {
				return opt, true, err
			}
			relType, file, ok := strings.Cut(v, "=")
			if !ok {
				return opt, true, fmt.Errorf("--rels expects TYPE=FILE, got %q", v)
			}
			opt.rels = append(opt.rels, relImportSpec{relType: relType, file: file})
		case a == "--skip-bad-rows":
			opt.skipBad = true
		case a == "--append":
			opt.append = true
		case a == "--separator":
			v, err := next()
			if err != nil {
				return opt, true, err
			}
			opt.delimiter = v
		case strings.HasPrefix(a, "-"):
			return opt, true, fmt.Errorf("unknown bulk import flag %q", a)
		default:
			if opt.dbPath != "" {
				return opt, true, fmt.Errorf("unexpected argument %q", a)
			}
			opt.dbPath = a
		}
	}
	if opt.dbPath == "" {
		return opt, true, fmt.Errorf("a database path is required")
	}
	if len(opt.nodes) == 0 && len(opt.rels) == 0 {
		return opt, true, fmt.Errorf("at least one --nodes or --rels source is required")
	}
	return opt, true, nil
}

// runBulkImport builds the output .gr file using the 4-pass loader (doc 19 §2).
// It writes to opt.dbPath and prints a final summary to stderr.
func runBulkImport(opt bulkImportOptions, stderr io.Writer) int {
	onBad := loader.Skip
	if !opt.skipBad {
		onBad = loader.Fail
	}

	var delim rune
	if opt.delimiter != "" {
		delim = []rune(opt.delimiter)[0]
	}

	nodeSrcs := make([]loader.NodeSource, len(opt.nodes))
	for i, ns := range opt.nodes {
		nodeSrcs[i] = loader.NodeSource{
			Files: []string{ns.file},
			Label: ns.label,
		}
	}
	relSrcs := make([]loader.RelSource, len(opt.rels))
	for i, rs := range opt.rels {
		relSrcs[i] = loader.RelSource{
			Files: []string{rs.file},
			Type:  rs.relType,
		}
	}

	opts := loader.Options{
		Nodes:         nodeSrcs,
		Relationships: relSrcs,
		OnDuplicateID: onBad,
		OnMissingID:   onBad,
		OnDangling:    loader.Skip,
	}
	if delim != 0 {
		opts.Delimiter = delim
	}

	l := loader.New(opts)
	if err := l.Run(opt.dbPath); err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitData
	}

	stats := l.Stats()
	bad := l.Bad()
	fmt.Fprintf(stderr, "imported %d nodes, %d relationships", stats.Nodes, stats.Rels)
	if stats.DupNodes > 0 || stats.DanglingRels > 0 {
		fmt.Fprintf(stderr, " (%d dup nodes skipped, %d dangling rels skipped)",
			stats.DupNodes, stats.DanglingRels)
	}
	fmt.Fprintln(stderr)
	if bad.Total() > 0 {
		fmt.Fprintf(stderr, "warning: %d bad rows skipped\n", bad.Total())
	}
	return exitOK
}

// bulkImportUsage prints the bulk import usage string.
func bulkImportUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: gr import DATABASE --nodes LABEL=FILE [--nodes LABEL=FILE]...")
	fmt.Fprintln(w, "                         [--rels TYPE=FILE [--rels TYPE=FILE]...]")
	fmt.Fprintln(w, "                         [--skip-bad-rows] [--append] [--separator SEP]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Bulk-import CSV node and relationship files using the 4-pass loader.")
	fmt.Fprintln(w, "Use `gr import DATABASE FILE --as LABEL` for single-file transactional import.")
}

// isBulkImport reports whether the args contain --nodes or --rels, indicating
// the bulk-loader path should be taken instead of the transactional path.
func isBulkImport(args []string) bool {
	for _, a := range args {
		if a == "--nodes" || strings.HasPrefix(a, "--nodes=") ||
			a == "--rels" || strings.HasPrefix(a, "--rels=") {
			return true
		}
	}
	return false
}
