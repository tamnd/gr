package gr

import (
	"context"
	"sync"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// recordingSpan is a test Span that captures the attributes and outcome set on it, so a
// wiring test can assert what the query path put on the root gr.query span.
type recordingSpan struct {
	mu     sync.Mutex
	name   string
	strs   map[string]string
	ints   map[string]int64
	bools  map[string]bool
	status string
	ok     bool
	ended  bool
}

func (s *recordingSpan) SetString(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strs[k] = v
}

func (s *recordingSpan) SetInt(k string, v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ints[k] = v
}

func (s *recordingSpan) SetBool(k string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bools[k] = v
}

func (s *recordingSpan) SetStatus(ok bool, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ok, s.status = ok, desc
}

func (s *recordingSpan) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
}

// recordingTracer is a test Tracer that builds one recordingSpan per StartSpan and keeps them
// in order, so a test inspects the spans a run produced.
type recordingTracer struct {
	mu    sync.Mutex
	spans []*recordingSpan
}

func (tr *recordingTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s := &recordingSpan{
		name:  name,
		strs:  map[string]string{},
		ints:  map[string]int64{},
		bools: map[string]bool{},
	}
	tr.spans = append(tr.spans, s)
	return ctx, s
}

func (tr *recordingTracer) last() *recordingSpan {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.spans) == 0 {
		return nil
	}
	return tr.spans[len(tr.spans)-1]
}

// named returns the first span started under name, or nil if none, so a test asserts a phase
// span was emitted.
func (tr *recordingTracer) named(name string) *recordingSpan {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, s := range tr.spans {
		if s.name == name {
			return s
		}
	}
	return nil
}

// lastNamed returns the most recent span started under name, for a test that runs several
// statements and wants the last one's span.
func (tr *recordingTracer) lastNamed(name string) *recordingSpan {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for i := len(tr.spans) - 1; i >= 0; i-- {
		if tr.spans[i].name == name {
			return tr.spans[i]
		}
	}
	return nil
}

// TestDBTracerRootSpanOnWrite confirms a write through Run starts and ends a root gr.query span
// carrying the correlation id, the kind, the ok status, and the rows it returned (doc 20 §12.2,
// §12.3).
func TestDBTracerRootSpanOnWrite(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_write.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatal(err)
	}

	span := tr.named("gr.query")
	if span == nil {
		t.Fatal("no span started for a write")
	}
	if span.name != "gr.query" {
		t.Errorf("span name = %q, want gr.query", span.name)
	}
	if !span.ended {
		t.Error("root span was not ended")
	}
	if !span.ok || span.status != "ok" {
		t.Errorf("span status = %q ok=%v, want ok", span.status, span.ok)
	}
	if span.strs["gr.query.id"] == "" {
		t.Error("span has no gr.query.id attribute")
	}
	if span.strs["gr.query.kind"] != "write" {
		t.Errorf("gr.query.kind = %q, want write", span.strs["gr.query.kind"])
	}
}

// TestDBTracerRootSpanOnRead confirms a streaming read ends its root span at Close, not at the
// Run return, and reports the rows it yielded (doc 20 §12.2): the span must stay open across the
// stream the way the query metrics and the query log do.
func TestDBTracerRootSpanOnRead(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_read.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'}), (:Person {name: 'b'})", nil); err != nil {
		t.Fatal(err)
	}

	res, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p.name AS name", nil)
	if err != nil {
		t.Fatal(err)
	}
	readSpan := tr.lastNamed("gr.query")
	if readSpan == nil || readSpan.name != "gr.query" {
		t.Fatal("no root span for the read")
	}
	// The stream is open, so its span must not have ended yet.
	readSpan.mu.Lock()
	endedEarly := readSpan.ended
	readSpan.mu.Unlock()
	if endedEarly {
		t.Error("read span ended before the stream was drained")
	}

	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("read %d rows, want 2", rows)
	}

	if !readSpan.ended {
		t.Error("read span was not ended at Close")
	}
	if readSpan.strs["gr.query.kind"] != "read" {
		t.Errorf("gr.query.kind = %q, want read", readSpan.strs["gr.query.kind"])
	}
	if readSpan.ints["gr.execute.rows_returned"] != 2 {
		t.Errorf("gr.execute.rows_returned = %d, want 2", readSpan.ints["gr.execute.rows_returned"])
	}
}

// TestDBTracerRootSpanOnError confirms a parse failure still produces a root span marked failed
// (doc 20 §12.2): the span covers the whole statement, parse included.
func TestDBTracerRootSpanOnError(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_err.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "THIS IS NOT CYPHER", nil); err == nil {
		t.Fatal("a parse failure was accepted")
	}
	span := tr.named("gr.query")
	if span == nil {
		t.Fatal("no span started for a parse failure")
	}
	if !span.ended {
		t.Error("error span was not ended")
	}
	if span.ok {
		t.Error("error span was marked ok")
	}
	if span.strs["gr.query.status"] != "error" {
		t.Errorf("gr.query.status = %q, want error", span.strs["gr.query.status"])
	}
}

// TestDBTracerShareIDWithQueryLog confirms the trace span and the query-log entry carry the same
// correlation id, the join the spec calls the key that ties the event surfaces together (doc 20
// §12.3).
func TestDBTracerShareIDWithQueryLog(t *testing.T) {
	tr := &recordingTracer{}
	ql, readLog := captureQueryLog(QueryLogAll, RedactAll, 0)
	db, err := Open("trace_join.gr", Options{VFS: vfs.NewMem(), SaltSeed: 1, Tracer: tr, QueryLog: ql})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatal(err)
	}

	span := tr.named("gr.query")
	if span == nil {
		t.Fatal("no span")
	}
	spanID := span.strs["gr.query.id"]
	if spanID == "" {
		t.Fatal("span has no gr.query.id")
	}
	var logged map[string]any
	for _, e := range readLog() {
		if e["event"] == "query" {
			logged = e
		}
	}
	if logged == nil {
		t.Fatalf("no query-log entry; entries = %v", readLog())
	}
	if logged["query_id"] != spanID {
		t.Errorf("query-log id = %v, span id = %v, want equal", logged["query_id"], spanID)
	}
}

// TestDBTracerParseSpan confirms a statement emits a gr.parse child span tagged with the
// statement length and ended (doc 20 §12.2, §12.3).
func TestDBTracerParseSpan(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_parse.gr", tr)
	defer func() { _ = db.Close() }()

	const stmt = "CREATE (:Person {name: 'a'})"
	if _, err := db.Run(context.Background(), stmt, nil); err != nil {
		t.Fatal(err)
	}

	parse := tr.named("gr.parse")
	if parse == nil {
		t.Fatal("no gr.parse span emitted")
	}
	if !parse.ended {
		t.Error("gr.parse span was not ended")
	}
	if !parse.ok {
		t.Error("gr.parse span was marked failed for a valid statement")
	}
	if parse.ints["gr.parse.query_len"] != int64(len(stmt)) {
		t.Errorf("gr.parse.query_len = %d, want %d", parse.ints["gr.parse.query_len"], len(stmt))
	}
}

// TestDBTracerParseSpanFailed confirms a parse failure ends the gr.parse span marked failed, so
// the phase that failed is visible in the trace (doc 20 §12.2).
func TestDBTracerParseSpanFailed(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_parsefail.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "NOT CYPHER AT ALL", nil); err == nil {
		t.Fatal("a parse failure was accepted")
	}
	parse := tr.named("gr.parse")
	if parse == nil {
		t.Fatal("no gr.parse span emitted for a parse failure")
	}
	if !parse.ended {
		t.Error("gr.parse span was not ended")
	}
	if parse.ok {
		t.Error("gr.parse span was marked ok for a parse failure")
	}
}

// TestDBTracerPlanSpan confirms a read emits a gr.plan child span carrying the plan-cache
// outcome (doc 20 §12.2, §12.3): the first run of a statement misses the plan cache and the
// second hits it.
func TestDBTracerPlanSpan(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_plan.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatal(err)
	}

	const read = "MATCH (p:Person) RETURN p.name AS name"
	res, err := db.Run(context.Background(), read, nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	plan := tr.lastNamed("gr.plan")
	if plan == nil {
		t.Fatal("no gr.plan span emitted for a read")
	}
	if !plan.ended {
		t.Error("gr.plan span was not ended")
	}
	if plan.strs["gr.plan.cache"] != "miss" {
		t.Errorf("first run gr.plan.cache = %q, want miss", plan.strs["gr.plan.cache"])
	}

	res2, err := db.Run(context.Background(), read, nil)
	if err != nil {
		t.Fatal(err)
	}
	for res2.Next() {
	}
	if err := res2.Close(); err != nil {
		t.Fatal(err)
	}
	plan2 := tr.lastNamed("gr.plan")
	if plan2 == nil {
		t.Fatal("no gr.plan span on the repeat read")
	}
	if plan2.strs["gr.plan.cache"] != "hit" {
		t.Errorf("repeat run gr.plan.cache = %q, want hit", plan2.strs["gr.plan.cache"])
	}
}

// TestDBTracerExecuteSpan confirms a streaming read emits a gr.execute child span ended at Close
// with the scanned and returned rows (doc 20 §12.2, §12.3): the span stays open across the stream
// the way the root span does, and reports the work and the output once the cursor drains.
func TestDBTracerExecuteSpan(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_exec.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'}), (:Person {name: 'b'})", nil); err != nil {
		t.Fatal(err)
	}

	res, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p.name AS name", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := tr.lastNamed("gr.execute")
	if exec == nil {
		t.Fatal("no gr.execute span emitted for a read")
	}
	// The stream is open, so the execute span must not have ended yet.
	exec.mu.Lock()
	endedEarly := exec.ended
	exec.mu.Unlock()
	if endedEarly {
		t.Error("gr.execute span ended before the stream was drained")
	}

	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("read %d rows, want 2", rows)
	}
	if !exec.ended {
		t.Error("gr.execute span was not ended at Close")
	}
	if !exec.ok {
		t.Error("gr.execute span was marked failed for a clean read")
	}
	if exec.ints["gr.execute.rows_returned"] != 2 {
		t.Errorf("gr.execute.rows_returned = %d, want 2", exec.ints["gr.execute.rows_returned"])
	}
	if exec.ints["gr.execute.rows_scanned"] < 2 {
		t.Errorf("gr.execute.rows_scanned = %d, want at least 2", exec.ints["gr.execute.rows_scanned"])
	}
}

// TestDBTracerWritePhaseSpans confirms an eager write emits gr.plan and gr.execute child spans,
// both ended, with the plan marked a cache miss and the execute carrying the rows it wrote (doc
// 20 §12.2): a write never uses the plan cache, so its plan is always a miss.
func TestDBTracerWritePhaseSpans(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_write_phases.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'}) RETURN 1 AS one", nil); err != nil {
		t.Fatal(err)
	}

	plan := tr.named("gr.plan")
	if plan == nil {
		t.Fatal("no gr.plan span emitted for a write")
	}
	if !plan.ended {
		t.Error("write gr.plan span was not ended")
	}
	if plan.strs["gr.plan.cache"] != "miss" {
		t.Errorf("write gr.plan.cache = %q, want miss", plan.strs["gr.plan.cache"])
	}

	exec := tr.named("gr.execute")
	if exec == nil {
		t.Fatal("no gr.execute span emitted for a write")
	}
	if !exec.ended {
		t.Error("write gr.execute span was not ended")
	}
	if !exec.ok {
		t.Error("write gr.execute span was marked failed for a clean write")
	}
	if exec.ints["gr.execute.rows_returned"] != 1 {
		t.Errorf("write gr.execute.rows_returned = %d, want 1", exec.ints["gr.execute.rows_returned"])
	}
}

// TestDBTracerTxPhaseSpans confirms a read inside a managed write transaction emits gr.plan and
// gr.execute child spans (doc 20 §12.2): the structural bind inside the transaction is a plan-cache
// miss, and the execute span ends at Close with the rows it yielded.
func TestDBTracerTxPhaseSpans(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_tx_phases.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'}), (:Person {name: 'b'})", nil); err != nil {
		t.Fatal(err)
	}

	tx, err := db.Begin(context.Background(), Write)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Run(context.Background(), "MATCH (p:Person) RETURN p.name AS name", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("read %d rows, want 2", rows)
	}

	plan := tr.lastNamed("gr.plan")
	if plan == nil || plan.strs["gr.plan.cache"] != "miss" {
		t.Fatalf("tx gr.plan span missing or not a miss: %+v", plan)
	}
	exec := tr.lastNamed("gr.execute")
	if exec == nil {
		t.Fatal("no gr.execute span for a tx read")
	}
	if !exec.ended {
		t.Error("tx gr.execute span was not ended at Close")
	}
	if exec.ints["gr.execute.rows_returned"] != 2 {
		t.Errorf("tx gr.execute.rows_returned = %d, want 2", exec.ints["gr.execute.rows_returned"])
	}
}

// TestDBNoTracerDisabled confirms a database opened without a tracer neither panics nor starts
// spans, the embedded default.
func TestDBNoTracerDisabled(t *testing.T) {
	db := openMem(t, "trace_off.gr")
	defer func() { _ = db.Close() }()
	if db.tracer != nil {
		t.Errorf("tracer = %v, want nil when none configured", db.tracer)
	}
	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatal(err)
	}
}

// TestDBTracerOperatorSpans confirms that setting tracing_detail=operator emits per-operator
// child spans under gr.execute (doc 20 §12.2): a read query over two nodes emits at least
// a NodeScan span with a non-negative row count, and the spans are ended when Close returns.
func TestDBTracerOperatorSpans(t *testing.T) {
	tr := &recordingTracer{}
	db := openMemTraced(t, "trace_ops.gr", tr)
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'Ada'}), (:Person {name: 'Bob'})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Run(context.Background(), "PRAGMA tracing_detail = 'operator'", nil); err != nil {
		t.Fatal(err)
	}

	res, err := db.Query("MATCH (n:Person) RETURN n.name", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}

	var opSpans []*recordingSpan
	for _, s := range tr.spans {
		if len(s.name) > len("gr.operator.") && s.name[:len("gr.operator.")] == "gr.operator." {
			opSpans = append(opSpans, s)
		}
	}
	if len(opSpans) == 0 {
		t.Fatalf("no gr.operator.* spans emitted at detail=operator: spans = %v", spanNames(tr.spans))
	}
	for _, s := range opSpans {
		if !s.ended {
			t.Errorf("operator span %q was not ended", s.name)
		}
		if s.ints["gr.operator.rows"] < 0 {
			t.Errorf("operator span %q has negative rows: %d", s.name, s.ints["gr.operator.rows"])
		}
	}
}

// spanNames returns the names of all spans in order, for diagnostic messages.
func spanNames(spans []*recordingSpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.name
	}
	return names
}

// openMemTraced opens an in-memory database with a tracer installed.
func openMemTraced(t *testing.T, path string, tr Tracer) *DB {
	t.Helper()
	db, err := Open(path, Options{VFS: vfs.NewMem(), SaltSeed: 1, Tracer: tr})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
