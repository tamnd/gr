package tck

import (
	"fmt"
	"io"
	"strings"
)

// Report accumulates TCK run outcomes across all scenarios.
type Report struct {
	Pass          int
	Fail          int
	SkipUnimpl    int
	SkipDeviation int
	Total         int

	failures   []string
	unimplFeat map[string]int // unimplemented feature tag -> count
}

func (r *Report) record(s Scenario, out Outcome) {
	r.Total++
	switch out.Kind {
	case OutcomePass:
		r.Pass++
	case OutcomeFail:
		r.Fail++
		r.failures = append(r.failures, fmt.Sprintf("[%s] %s: %s", s.Feature, s.Name, out.Reason))
	case OutcomeSkipUnimplemented:
		r.SkipUnimpl++
		feat := extractFeature(out.Reason)
		if r.unimplFeat == nil {
			r.unimplFeat = make(map[string]int)
		}
		r.unimplFeat[feat]++
	case OutcomeSkipDeviation:
		r.SkipDeviation++
	}
}

// Write writes a human-readable report to w in the format described in doc 23 §2.6.
func (r *Report) Write(w io.Writer) {
	fmt.Fprintf(w, "TCK report (%d scenarios)\n", r.Total)
	fmt.Fprintf(w, "  pass:                %6d  (%4.1f%%)\n", r.Pass, pct(r.Pass, r.Total))
	fmt.Fprintf(w, "  skip(unimplemented): %6d  (%4.1f%%)\n", r.SkipUnimpl, pct(r.SkipUnimpl, r.Total))
	fmt.Fprintf(w, "  skip(deviation):     %6d  (%4.1f%%)\n", r.SkipDeviation, pct(r.SkipDeviation, r.Total))
	fmt.Fprintf(w, "  fail:                %6d\n", r.Fail)
	fmt.Fprintln(w, strings.Repeat("-", 52))

	conform := r.Pass + r.Fail
	if conform > 0 {
		fmt.Fprintf(w, "  conformant-subset pass rate: %.2f%%\n", pct(r.Pass, conform))
	}

	if len(r.failures) > 0 {
		fmt.Fprintln(w, "\nFAILURES:")
		for _, f := range r.failures {
			fmt.Fprintf(w, "  FAIL  %s\n", f)
		}
	}
}

// IsClean returns true iff there are zero failures.
func (r *Report) IsClean() bool { return r.Fail == 0 }

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func extractFeature(reason string) string {
	// reason is like "tag:@skip" or "unrecognized step: ..."
	if strings.HasPrefix(reason, "tag:") {
		return reason
	}
	if strings.Contains(reason, ":") {
		return reason[:strings.Index(reason, ":")+1]
	}
	return reason
}
