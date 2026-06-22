package main

import (
	"fmt"
	"io"

	"github.com/tamnd/gr"
)

// dotInfo prints the open database's static and structural facts (doc 17 §6.15).
// It is the nameplate of the file: where it lives, its format and page geometry,
// its size and free space, the catalog and element counts, and the schema-object
// counts. The listing goes to the chatter channel like the other descriptive
// commands.
func (s *shell) dotInfo() {
	info, err := s.db.Info()
	if err != nil {
		fmt.Fprintln(s.errw, "Error:", err)
		s.code = worst(s.code, classify(err))
		return
	}
	writeInfo(s.errw, info)
}

// runInfoCmd implements the `gr info` subcommand (doc 17 §7.8): it opens a
// database read-only and prints the same facts as `.info` non-interactively.
func runInfoCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "gr: info needs a database argument")
		return exitUsage
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stderr, "Usage: gr info DATABASE")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Print the static and structural facts of a database file.")
		return exitUsage
	}
	path := args[0]

	db, err := gr.Open(path, gr.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	info, err := db.Info()
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return classify(err)
	}
	writeInfo(stdout, info)
	return exitOK
}

// writeInfo renders an info block in the spec's aligned label-and-value form (doc
// 17 §6.15). The size line carries the page count and free-page count, the
// catalog line the three token counts, and the schema lines the object counts.
func writeInfo(w io.Writer, info gr.DBInfo) {
	path := info.Path
	if path == memPath {
		path = ":memory:"
	}
	fmt.Fprintf(w, "File:            %s\n", path)
	fmt.Fprintf(w, "Format version:  gr v%d (readable, writable)\n", info.FormatVersion)
	fmt.Fprintf(w, "Page size:       %d bytes\n", info.PageSize)
	fmt.Fprintf(w, "Database size:   %s (%d pages, %d free)\n", humanBytes(info.SizeBytes), info.PageCount, info.FreePages)
	fmt.Fprintf(w, "Journal mode:    WAL\n")
	fmt.Fprintf(w, "Encryption:      none\n")
	fmt.Fprintf(w, "Catalog:         %s, %s, %s\n",
		plural(info.Labels, "label"),
		plural(info.RelTypes, "relationship type"),
		plural(info.PropertyKeys, "property key"))
	fmt.Fprintf(w, "Elements:        %s, %s\n",
		plural(info.Nodes, "node"),
		plural(info.Relationships, "relationship"))
	fmt.Fprintf(w, "Indexes:         %d\n", info.Indexes)
	if info.UniqueCons > 0 && info.UniqueCons == info.Constraints {
		fmt.Fprintf(w, "Constraints:     %d (%d unique)\n", info.Constraints, info.UniqueCons)
	} else {
		fmt.Fprintf(w, "Constraints:     %d\n", info.Constraints)
	}
}

// humanBytes renders a byte count in binary units (KiB, MiB) for the size line.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// plural renders a count with its noun, adding an "s" for any count other than one.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
