package main

import (
	"fmt"
	"io"

	"github.com/tamnd/gr"
)

// dotHealth prints the database's live health report (doc 17 §6.17, doc 20 §13.3).
// It is the operator's at-a-glance view at the prompt: the engine state, whether it
// can serve, the liveness gauges, the commit and checkpoint progress, and any active
// warnings. It renders the same DB.Health the served /healthz/detail endpoint does,
// so the prompt and the endpoint never disagree. The listing goes to the chatter
// channel like the other descriptive commands.
func (s *shell) dotHealth() {
	writeHealth(s.errw, s.db.Health())
}

// runHealthCmd implements the `gr health` subcommand (doc 17 §7.9): it opens a
// database read-only and prints the same report as `.health` non-interactively. It
// exits non-zero when the engine is not ready, so a shell script or a watchdog can
// gate on the database being serveable.
func runHealthCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "gr: health needs a database argument")
		return exitUsage
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stderr, "Usage: gr health DATABASE")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Print the live health report of a database file.")
		fmt.Fprintln(stderr, "Exits non-zero when the engine is not ready to serve.")
		return exitUsage
	}
	path := args[0]

	db, err := gr.Open(path, gr.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "gr:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	rep := db.Health()
	writeHealth(stdout, rep)
	if !rep.Ready {
		return exitGeneric
	}
	return exitOK
}

// writeHealth renders a health report in the aligned label-and-value form the info
// block uses (doc 17 §6.17). The state line carries the ready flag, the gauge lines
// the live counts, and the warnings section the "what to look at" the report carries,
// one per line, or "(none)" when the engine is healthy.
func writeHealth(w io.Writer, rep gr.HealthReport) {
	const f = "%-18s%s\n"
	fmt.Fprintf(w, f, "State:", rep.State)
	fmt.Fprintf(w, f, "Ready:", yesNo(rep.Ready))
	fmt.Fprintf(w, f, "Inflight queries:", fmt.Sprint(rep.InflightQueries))
	fmt.Fprintf(w, f, "Open transactions:", fmt.Sprint(rep.OpenTransactions))
	fmt.Fprintf(w, f, "Open sessions:", fmt.Sprint(rep.OpenSessions))
	fmt.Fprintf(w, f, "Commits:", fmt.Sprint(rep.Commits))
	fmt.Fprintf(w, f, "Checkpoints:", fmt.Sprint(rep.Checkpoints))
	if rep.LastCheckpoint.IsZero() {
		fmt.Fprintf(w, f, "Last checkpoint:", "(none)")
	} else {
		fmt.Fprintf(w, f, "Last checkpoint:", rep.LastCheckpoint.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Fprintf(w, f, "WAL fsync errors:", fmt.Sprint(rep.WALFsyncErrors))
	if len(rep.Warnings) == 0 {
		fmt.Fprintf(w, f, "Warnings:", "(none)")
		return
	}
	fmt.Fprintln(w, "Warnings:")
	for _, msg := range rep.Warnings {
		fmt.Fprintf(w, "  - %s\n", msg)
	}
}

// yesNo renders a boolean as a yes or no for the health report's ready line.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
