package metric

import (
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// WritePrometheus renders a snapshot in the Prometheus text exposition format (doc 20 §7.5,
// §33.1): for each metric a HELP line (the catalogue's meaning), a TYPE line (counter, gauge,
// or histogram), and the value lines. A counter and a gauge render as their value per label
// set; a histogram renders as its cumulative le buckets plus _sum and _count, the form a
// Prometheus histogram_quantile consumes. This is the byte-for-byte counterpart of the expvar
// bridge and the programmatic snapshot: one registry projected three ways (doc 20 invariant 4).
//
// The snapshot is already sorted by name then label key (doc 20 §7.3), so a family's series are
// contiguous and the HELP and TYPE header is written once per name.
func WritePrometheus(w io.Writer, snap Snapshot) error {
	bw := &errWriter{w: w}
	var lastName string
	for _, m := range snap.Metrics() {
		if m.Name != lastName {
			bw.put("# HELP ")
			bw.put(m.Name)
			bw.putByte(' ')
			bw.put(escapeHelp(m.Help))
			bw.putByte('\n')
			bw.put("# TYPE ")
			bw.put(m.Name)
			bw.putByte(' ')
			bw.put(m.Type.String())
			bw.putByte('\n')
			lastName = m.Name
		}
		switch m.Type {
		case TypeCounter:
			writeSample(bw, m.Name, m.Labels, "", "", formatUint(m.Counter))
		case TypeGauge:
			writeSample(bw, m.Name, m.Labels, "", "", formatInt(m.Gauge))
		case TypeHistogram:
			writeHistogram(bw, m)
		}
	}
	return bw.err
}

// writeHistogram renders one histogram series as its cumulative buckets plus _sum and _count
// (doc 20 §33.1). Each bucket line carries the series labels plus an le label naming the
// bucket's upper bound, with the +Inf catch-all rendered as le="+Inf".
func writeHistogram(bw *errWriter, m MetricSnapshot) {
	h := m.Histogram
	for i, bound := range h.Bounds {
		le := "+Inf"
		if !math.IsInf(bound, 1) {
			le = formatFloat(bound)
		}
		writeSample(bw, m.Name+"_bucket", m.Labels, "le", le, formatUint(h.Counts[i]))
	}
	writeSample(bw, m.Name+"_sum", m.Labels, "", "", formatFloat(h.Sum))
	writeSample(bw, m.Name+"_count", m.Labels, "", "", formatUint(h.Count))
}

// writeSample writes one sample line: the metric name, its labels (with an optional extra
// label such as le for a histogram bucket), and the value. The labels are rendered sorted so
// the output is stable, and the extra label is appended last, the Prometheus convention for le.
func writeSample(bw *errWriter, name string, labels Labels, extraKey, extraVal, value string) {
	bw.put(name)
	if len(labels) > 0 || extraKey != "" {
		bw.putByte('{')
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		first := true
		for _, k := range keys {
			if !first {
				bw.putByte(',')
			}
			first = false
			bw.put(k)
			bw.put(`="`)
			bw.put(escapeLabel(labels[k]))
			bw.putByte('"')
		}
		if extraKey != "" {
			if !first {
				bw.putByte(',')
			}
			bw.put(extraKey)
			bw.put(`="`)
			bw.put(escapeLabel(extraVal))
			bw.putByte('"')
		}
		bw.putByte('}')
	}
	bw.putByte(' ')
	bw.put(value)
	bw.putByte('\n')
}

// escapeLabel escapes a label value for the Prometheus text format: backslash, double quote,
// and newline are the three characters the format requires escaped.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, "\"", `\"`, "\n", `\n`)
	return r.Replace(s)
}

// escapeHelp escapes a HELP string: backslash and newline are the two the format requires
// escaped there (a double quote is literal in a HELP line).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, "\n", `\n`)
	return r.Replace(s)
}

func formatUint(v uint64) string { return strconv.FormatUint(v, 10) }
func formatInt(v int64) string   { return strconv.FormatInt(v, 10) }

// formatFloat renders a float in the shortest form that round-trips (the 'g' format), so a sum
// or a bucket bound reads as 0.001 and 412.7 rather than a padded decimal.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// errWriter defers error handling: it stops writing after the first error and reports it once,
// so the render path is not a chain of error checks.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) put(s string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, s)
}

func (e *errWriter) putByte(b byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write([]byte{b})
}
