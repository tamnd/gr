package httpd

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// metrics is the server's running counters and gauges (doc 18 §13.5, doc 20). It records
// only what the HTTP handler can observe honestly: error counts by status code, auth
// outcomes, (read live from the transaction store) the open-transaction gauge and the
// reaped-transaction counter, and (when a gate is configured) the in-flight-query gauge
// and the shed counter. The metrics that need a connection pool (connections, watermark
// age) are not emitted here, because a plain http.Handler does not own those; emitting
// them as constant zeros would mislead an operator.
type metrics struct {
	mu          sync.Mutex
	errors      map[string]int64 // by Neo4j status code
	authOK      atomic.Int64
	authFail    atomic.Int64
	authLockout atomic.Int64
}

// newMetrics returns an empty metrics registry.
func newMetrics() *metrics {
	return &metrics{errors: make(map[string]int64)}
}

// countError records one error response by its Neo4j status code.
func (m *metrics) countError(code string) {
	m.mu.Lock()
	m.errors[code]++
	m.mu.Unlock()
}

// authOutcome is the result of an authentication attempt for the metrics.
type authOutcome int

const (
	authSuccess authOutcome = iota
	authFailure
	authLocked
)

// countAuth records one authentication outcome (doc 18 §10.3): the success/failure/lockout
// split is the brute-force signal an operator alerts on.
func (m *metrics) countAuth(outcome authOutcome) {
	switch outcome {
	case authSuccess:
		m.authOK.Add(1)
	case authLocked:
		m.authLockout.Add(1)
	default:
		m.authFail.Add(1)
	}
}

// handleMetrics serves GET /metrics in the Prometheus text exposition format (doc 18
// §9.7). Like the health probes it stays open, since a metrics scraper is part of the
// operational plane and runs without a user credential. It reports the open-transaction
// gauge and the reaped counter from the transaction store, and the error and auth
// counters from the registry.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	var b strings.Builder

	open, reaped := s.txns.stats()
	writeMetric(&b, "gr_server_open_transactions", "gauge",
		"explicit transactions held open", "", float64(open))
	writeMetric(&b, "gr_server_tx_reaped_total", "counter",
		"transactions force-rolled-back by the sweeper", "", float64(reaped))

	// The in-flight gauge and shed counter come from the shared admission gate (doc 18
	// §8.8, §13.5). They are emitted only when a gate is configured, so an ungated server
	// does not report a misleading constant zero.
	if s.admission != nil {
		writeMetric(&b, "gr_server_in_flight_queries", "gauge",
			"queries executing right now across all connections", "", float64(s.admission.InFlight()))
		writeMetric(&b, "gr_server_queries_shed_total", "counter",
			"queries shed because the in-flight gate was full", "", float64(s.admission.Shed()))
	}

	s.metrics.mu.Lock()
	codes := make([]string, 0, len(s.metrics.errors))
	for code := range s.metrics.errors {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	help(&b, "gr_server_errors_total", "counter", "errors by Neo4j status code")
	for _, code := range codes {
		fmt.Fprintf(&b, "gr_server_errors_total{code=%q} %d\n", code, s.metrics.errors[code])
	}
	s.metrics.mu.Unlock()

	help(&b, "gr_server_auth_total", "counter", "authentication outcomes")
	fmt.Fprintf(&b, "gr_server_auth_total{outcome=\"success\"} %d\n", s.metrics.authOK.Load())
	fmt.Fprintf(&b, "gr_server_auth_total{outcome=\"failure\"} %d\n", s.metrics.authFail.Load())
	fmt.Fprintf(&b, "gr_server_auth_total{outcome=\"lockout\"} %d\n", s.metrics.authLockout.Load())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// writeMetric writes a single-sample metric with its HELP and TYPE lines.
func writeMetric(b *strings.Builder, name, typ, helpText, labels string, value float64) {
	help(b, name, typ, helpText)
	if labels == "" {
		fmt.Fprintf(b, "%s %g\n", name, value)
		return
	}
	fmt.Fprintf(b, "%s{%s} %g\n", name, labels, value)
}

// help writes the HELP and TYPE comment lines for a metric.
func help(b *strings.Builder, name, typ, helpText string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, helpText)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}
