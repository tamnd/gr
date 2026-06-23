package gr

import "context"

// Tracer is the seam gr emits spans through (doc 20 §12). It is deliberately a tiny subset of
// the OpenTelemetry tracer shape, just enough to start a span under a parent carried in a
// context, so the engine imports no tracing SDK and stays zero-dependency. An embedder bridges
// its OTel tracer to this interface (StartSpan calls the OTel tracer's Start, and the returned
// Span forwards to the OTel span), so a served gr drops into an existing tracing fleet the way
// the Prometheus endpoint drops into a metrics fleet, standard protocol and standard exporter,
// no bespoke glue.
//
// A nil Tracer disables tracing, the embedded-friendly default when no tracer is configured.
type Tracer interface {
	// StartSpan begins a span named name as a child of the span carried in ctx, or a root span
	// when ctx carries none (the propagation seam adopts a caller's trace context into ctx, doc
	// 20 §12.4). It returns a context carrying the new span as the parent for any child spans and
	// the span itself, which the caller ends.
	StartSpan(ctx context.Context, name string) (context.Context, Span)
}

// Span is one node in a query's trace (doc 20 §12.2): the root gr.query span, a phase span, or
// an operator span. The setters carry the bounded, value-free attributes the span is annotated
// with (doc 20 §12.3): keys from a fixed vocabulary and values that are kinds, counts, and ids,
// never parameter data or node ids, so a trace is safe to ship to a backend without leaking data
// or exploding cardinality. The typed setters map onto OTel's attribute.String, attribute.Int64,
// and attribute.Bool so an embedder's bridge is a one-line forward per setter.
type Span interface {
	SetString(key, value string)
	SetInt(key string, value int64)
	SetBool(key string, value bool)
	// SetStatus marks the span ok or failed with a short description, the span's outcome the way
	// the query-log status is the entry's outcome (doc 20 §12.3).
	SetStatus(ok bool, desc string)
	// End closes the span, stamping its duration. It is called once, at the same completion
	// boundary where the query metrics and the query log finalize.
	End()
}

// startQuerySpan begins the root gr.query span for one statement when a tracer is configured
// (doc 20 §12.2), tagging it with the correlation id and the statement kind so the trace joins
// the query-log entry and the metric series on the same vocabulary (doc 20 §12.3). A nil tracer
// returns the context unchanged and a nil span, so the query path calls this unconditionally and
// a database with no tracer pays only the nil check. The kind is empty when the statement failed
// to parse, the same empty kind the query log carries for a parse failure.
func (db *DB) startQuerySpan(ctx context.Context, id, kind string) (context.Context, Span) {
	if db.tracer == nil {
		return ctx, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, span := db.tracer.StartSpan(ctx, "gr.query")
	if span != nil {
		span.SetString("gr.query.id", id)
		if kind != "" {
			span.SetString("gr.query.kind", kind)
		}
	}
	return ctx, span
}

// startPhaseSpan begins a child span named name under the root span carried in ctx when a tracer
// is configured (doc 20 §12.2), for the per-phase decomposition of a query (gr.parse, gr.plan,
// gr.execute, gr.stream). A nil tracer returns the context unchanged and a nil span, so a phase
// boundary calls this unconditionally and an untraced database pays only the nil check. It
// returns the child context too so a phase that has children of its own (gr.execute and its
// operator spans) parents them correctly.
func (db *DB) startPhaseSpan(ctx context.Context, name string) (context.Context, Span) {
	if db.tracer == nil {
		return ctx, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return db.tracer.StartSpan(ctx, name)
}

// parseSpan begins the gr.parse phase span for a statement of the given text (doc 20 §12.2),
// tagging it with the statement length (doc 20 §12.3). Parse has no child spans, so it returns
// only the span, which the caller ends with endPhaseSpan once the parse returns.
func (db *DB) parseSpan(ctx context.Context, cypher string) Span {
	_, span := db.startPhaseSpan(ctx, "gr.parse")
	if span != nil {
		span.SetInt("gr.parse.query_len", int64(len(cypher)))
	}
	return span
}

// endPhaseSpan closes a phase span, marking it failed when the phase errored (doc 20 §12.2). It
// is nil-safe so a phase boundary ends the span whether or not tracing is on.
func endPhaseSpan(span Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.SetStatus(false, queryStatus(err))
	} else {
		span.SetStatus(true, "ok")
	}
	span.End()
}

// endExecuteSpan closes the gr.execute phase span when the executor finishes (doc 20 §12.2),
// recording the work and the output as attributes (doc 20 §12.3): rows_scanned is what the scans
// and expands touched and rows_returned is what the cursor yielded, the two numbers whose ratio
// is the amplification a scan-heavy query reveals. It is nil-safe so the read path ends the span
// whether or not tracing is on, and marks the span failed when the stream errored.
func endExecuteSpan(span Span, scanned, returned int, err error) {
	if span == nil {
		return
	}
	span.SetInt("gr.execute.rows_scanned", int64(scanned))
	span.SetInt("gr.execute.rows_returned", int64(returned))
	if err != nil {
		span.SetStatus(false, queryStatus(err))
	} else {
		span.SetStatus(true, "ok")
	}
	span.End()
}

// endQuerySpan closes the root span at a query's completion boundary (doc 20 §12.2), recording
// the outcome and the output cardinality as attributes (doc 20 §12.3) so a trace shows the
// status and the rows the same way the query-log entry does. It is nil-safe so the eager, error,
// and streaming-close paths call it unconditionally. A non-ok status fails the span so a tracing
// backend flags it.
func endQuerySpan(span Span, status string, rowsReturned int) {
	if span == nil {
		return
	}
	span.SetString("gr.query.status", status)
	span.SetInt("gr.execute.rows_returned", int64(rowsReturned))
	span.SetStatus(status == "ok", status)
	span.End()
}
