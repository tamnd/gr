package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/engine"
)

// runCheckCmd implements `gr check DATABASE [--level quick|default|full|forensic]
// [--format text|json]` (doc 23 §8.11). It opens the database read-only, runs the
// integrity checker, and prints the report. Exit code is 0 for a clean file, non-zero
// if any Corruption, Fatal, or Inconsistency finding is present.
func runCheckCmd(args []string, stdout, stderr io.Writer) int {
	level := gr.CheckDefault
	format := "text"
	var dbPath string

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
		case a == "--level" || strings.HasPrefix(a, "--level="):
			var v string
			var err error
			if strings.HasPrefix(a, "--level=") {
				v = strings.TrimPrefix(a, "--level=")
			} else {
				v, err = next()
			}
			if err != nil {
				fmt.Fprintln(stderr, "gr check:", err)
				return exitUsage
			}
			switch v {
			case "quick":
				level = gr.CheckQuick
			case "default":
				level = gr.CheckDefault
			case "full":
				level = gr.CheckFull
			case "forensic":
				level = gr.CheckForensic
			default:
				fmt.Fprintf(stderr, "gr check: unknown level %q (quick|default|full|forensic)\n", v)
				return exitUsage
			}
		case a == "--format" || strings.HasPrefix(a, "--format="):
			var v string
			var err error
			if strings.HasPrefix(a, "--format=") {
				v = strings.TrimPrefix(a, "--format=")
			} else {
				v, err = next()
			}
			if err != nil {
				fmt.Fprintln(stderr, "gr check:", err)
				return exitUsage
			}
			if v != "text" && v != "json" {
				fmt.Fprintf(stderr, "gr check: unknown format %q (text|json)\n", v)
				return exitUsage
			}
			format = v
		case a == "-h" || a == "--help":
			printCheckUsage(stderr)
			return exitUsage
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "gr check: unknown flag %q\n", a)
			return exitUsage
		default:
			if dbPath != "" {
				fmt.Fprintf(stderr, "gr check: unexpected argument %q\n", a)
				return exitUsage
			}
			dbPath = a
		}
	}

	if dbPath == "" {
		printCheckUsage(stderr)
		return exitUsage
	}

	db, err := gr.Open(dbPath, gr.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "gr check:", err)
		return exitOpen
	}
	defer func() { _ = db.Close() }()

	report, err := db.Check(level)
	if err != nil {
		fmt.Fprintln(stderr, "gr check:", err)
		return exitData
	}

	if format == "json" {
		printCheckJSON(stdout, report)
	} else {
		printCheckText(stdout, report)
	}

	for _, f := range report.Findings {
		if f.Severity >= engine.Inconsistency {
			return exitData
		}
	}
	return exitOK
}

func printCheckText(w io.Writer, r gr.CheckReport) {
	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "OK: no inconsistencies found (%d pages scanned, %d edges visited, %v)\n",
			r.Stats.PagesScanned, r.Stats.EdgesVisited, r.Stats.Duration.Round(1000000))
		return
	}
	for _, f := range r.Findings {
		page := ""
		if f.Page != 0 {
			page = fmt.Sprintf(" page=%d", f.Page)
		}
		elem := ""
		if f.Element != 0 {
			elem = fmt.Sprintf(" elem=%d", f.Element)
		}
		fmt.Fprintf(w, "[%s] %s%s%s: %s\n", f.Severity, f.Code, page, elem, f.Detail)
	}
	fmt.Fprintf(w, "%d finding(s) (%d pages scanned, %d edges visited, %v)\n",
		len(r.Findings), r.Stats.PagesScanned, r.Stats.EdgesVisited, r.Stats.Duration.Round(1000000))
}

func printCheckJSON(w io.Writer, r gr.CheckReport) {
	type jsonFinding struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Page     uint64 `json:"page,omitempty"`
		Element  uint64 `json:"element,omitempty"`
		Detail   string `json:"detail,omitempty"`
	}
	type jsonReport struct {
		Level    int           `json:"level"`
		Clean    bool          `json:"clean"`
		Findings []jsonFinding `json:"findings"`
		Stats    struct {
			PagesScanned uint64 `json:"pages_scanned"`
			EdgesVisited uint64 `json:"edges_visited"`
			DurationMs   int64  `json:"duration_ms"`
		} `json:"stats"`
	}
	out := jsonReport{
		Level: int(r.Level),
		Clean: len(r.Findings) == 0,
	}
	out.Stats.PagesScanned = r.Stats.PagesScanned
	out.Stats.EdgesVisited = r.Stats.EdgesVisited
	out.Stats.DurationMs = r.Stats.Duration.Milliseconds()
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, jsonFinding{
			Severity: f.Severity.String(),
			Code:     f.Code,
			Page:     uint64(f.Page),
			Element:  f.Element,
			Detail:   f.Detail,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func printCheckUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: gr check DATABASE [--level quick|default|full|forensic] [--format text|json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Check the structural integrity of a .gr database file.")
	fmt.Fprintln(w, "Exits 0 when the file is clean; non-zero when Corruption, Fatal,")
	fmt.Fprintln(w, "or Inconsistency findings are present.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Levels:")
	fmt.Fprintln(w, "  quick     Page checksums only (fast)")
	fmt.Fprintln(w, "  default   + free-list, CSR offsets, catalog bijection")
	fmt.Fprintln(w, "  full      + adjacency symmetry, constraint satisfaction")
	fmt.Fprintln(w, "  forensic  Same as full, always prints all findings")
}
