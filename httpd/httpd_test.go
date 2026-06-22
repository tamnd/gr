package httpd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// newTestDB opens an in-memory database for a test and registers its close.
func newTestDB(t *testing.T) *gr.DB {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// post sends a JSON query body to the query endpoint and returns the recorder.
func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decode unmarshals a JSON response body into a generic map.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

func TestDiscovery(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	doc := decode(t, rec.Body.Bytes())
	if doc["gr_version"] != Version {
		t.Errorf("gr_version = %v, want %q", doc["gr_version"], Version)
	}
	if doc["query"] != "/db/neo4j/query/v2" {
		t.Errorf("query = %v", doc["query"])
	}
}

func TestHealthAndReady(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
}

func TestReadyzAfterClose(t *testing.T) {
	db := newTestDB(t)
	h := Handler(db, Options{})
	_ = db.Close()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz after close = %d, want 503", rec.Code)
	}
}

func TestBufferedQuery(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"statement":"RETURN 1 AS n, 'hi' AS s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	data := out["data"].(map[string]any)
	fields := data["fields"].([]any)
	if len(fields) != 2 || fields[0] != "n" || fields[1] != "s" {
		t.Fatalf("fields = %v", fields)
	}
	values := data["values"].([]any)
	if len(values) != 1 {
		t.Fatalf("values = %v", values)
	}
	row := values[0].([]any)
	if row[0] != float64(1) || row[1] != "hi" {
		t.Errorf("row = %v", row)
	}
	if out["queryType"] != "r" {
		t.Errorf("queryType = %v, want r", out["queryType"])
	}
}

// TestGraphObjectJSON confirms a returned node and relationship serialize with their
// labels, type, endpoints, and properties, not just an element id (doc 18 §9.4): the
// graph-object surface reaching the wire.
func TestGraphObjectJSON(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	if rec := post(t, h, `{"statement":"CREATE (:Person {name:'Ada', age:36})-[:KNOWS {since:2019}]->(:Person {name:'Lin'})"}`); rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, `{"statement":"MATCH (a:Person)-[r:KNOWS]->(b) RETURN a, r"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	row := out["data"].(map[string]any)["values"].([]any)[0].([]any)

	node := row[0].(map[string]any)
	if node["elementId"] == nil || node["elementId"] == "" {
		t.Errorf("node elementId = %v", node["elementId"])
	}
	labels := node["labels"].([]any)
	if len(labels) != 1 || labels[0] != "Person" {
		t.Errorf("node labels = %v", labels)
	}
	props := node["properties"].(map[string]any)
	if props["name"] != "Ada" || props["age"] != float64(36) {
		t.Errorf("node properties = %v", props)
	}

	rel := row[1].(map[string]any)
	if rel["type"] != "KNOWS" {
		t.Errorf("rel type = %v", rel["type"])
	}
	if rel["startNodeElementId"] == nil || rel["endNodeElementId"] == nil {
		t.Errorf("rel endpoints = %v / %v", rel["startNodeElementId"], rel["endNodeElementId"])
	}
	if rel["properties"].(map[string]any)["since"] != float64(2019) {
		t.Errorf("rel properties = %v", rel["properties"])
	}
}

func TestWriteCounters(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"statement":"CREATE (:Person {name:'Ada'})","includeCounters":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	if out["queryType"] != "w" {
		t.Errorf("queryType = %v, want w", out["queryType"])
	}
	c := out["counters"].(map[string]any)
	if c["containsUpdates"] != true {
		t.Errorf("containsUpdates = %v", c["containsUpdates"])
	}
	if c["nodesCreated"] != float64(1) {
		t.Errorf("nodesCreated = %v", c["nodesCreated"])
	}
}

func TestParameters(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"statement":"RETURN $x + $y AS z","parameters":{"x":2,"y":3}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	row := out["data"].(map[string]any)["values"].([]any)[0].([]any)
	if row[0] != float64(5) {
		t.Errorf("z = %v, want 5", row[0])
	}
}

func TestLargeIntTagged(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	// 2^53 + 1 is outside the JavaScript-safe range, so it must be string-tagged.
	rec := post(t, h, `{"statement":"RETURN 9007199254740993 AS big"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	row := out["data"].(map[string]any)["values"].([]any)[0].([]any)
	tagged, ok := row[0].(map[string]any)
	if !ok {
		t.Fatalf("big = %v (%T), want tagged object", row[0], row[0])
	}
	if tagged["$type"] != "Integer" || tagged["_value"] != "9007199254740993" {
		t.Errorf("tagged = %v", tagged)
	}
}

func TestIntegerEncodingString(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2?integerEncoding=string",
		strings.NewReader(`{"statement":"RETURN 7 AS n"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := decode(t, rec.Body.Bytes())
	row := out["data"].(map[string]any)["values"].([]any)[0].([]any)
	tagged, ok := row[0].(map[string]any)
	if !ok || tagged["_value"] != "7" {
		t.Errorf("n = %v, want string-tagged 7", row[0])
	}
}

func TestReadModeRejectsWrite(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"statement":"CREATE (:X)","accessMode":"READ"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	errs := out["errors"].([]any)
	first := errs[0].(map[string]any)
	if first["code"] != "Neo.ClientError.Statement.AccessMode" {
		t.Errorf("code = %v", first["code"])
	}
}

func TestSyntaxError(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"statement":"RETURE 1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	first := out["errors"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(first["code"].(string), "Neo.ClientError") {
		t.Errorf("code = %v, want a client error", first["code"])
	}
}

func TestMissingStatement(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"parameters":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	req := httptest.NewRequest(http.MethodGet, "/db/neo4j/query/v2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestUnknownDatabase(t *testing.T) {
	h := Handler(newTestDB(t), Options{Name: "graph"})
	req := httptest.NewRequest(http.MethodPost, "/db/other/query/v2",
		strings.NewReader(`{"statement":"RETURN 1"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestNDJSONStream(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2?stream=true",
		strings.NewReader(`{"statement":"UNWIND [1,2,3] AS n RETURN n"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q, body = %s", ct, rec.Body.String())
	}
	var header, summary map[string]any
	var rows []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(rec.Body.Bytes()))
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		m := decode(t, line)
		switch {
		case m["header"] != nil:
			header = m
		case m["row"] != nil:
			rows = append(rows, m)
		case m["summary"] != nil:
			summary = m
		}
	}
	if header == nil {
		t.Error("missing header line")
	}
	if len(rows) != 3 {
		t.Errorf("got %d row lines, want 3", len(rows))
	}
	if summary == nil {
		t.Error("missing summary line")
	}
}

func TestNDJSONViaAccept(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestSchemaQueryType(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{"statement":"CREATE INDEX person_name FOR (p:Person) ON (p.name)","includeCounters":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec.Body.Bytes())
	if out["queryType"] != "s" {
		t.Errorf("queryType = %v, want s", out["queryType"])
	}
}

func TestInvalidJSON(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := post(t, h, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestQueryAdmissionShed confirms the HTTP surface sheds a query as a retryable transient
// when the shared in-flight gate is full (doc 18 §8.8). The one slot is claimed directly,
// so the request finds the gate full and gets a 503 with a transient code.
func TestQueryAdmissionShed(t *testing.T) {
	gate := gr.NewAdmission(1, 10*time.Millisecond)
	release, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer release()
	h := Handler(newTestDB(t), Options{Admission: gate})
	rec := post(t, h, `{"statement":"RETURN 1 AS n"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := decode(t, rec.Body.Bytes())
	errsAny, ok := body["errors"].([]any)
	if !ok || len(errsAny) == 0 {
		t.Fatalf("no errors in response: %v", body)
	}
	first := errsAny[0].(map[string]any)
	if first["code"] != "Neo.TransientError.General.TransientError" {
		t.Errorf("code = %v, want transient", first["code"])
	}
}

// TestQueryAdmissionAdmits confirms a configured gate with a free slot lets a query
// through, so the gate is not an unconditional block.
func TestQueryAdmissionAdmits(t *testing.T) {
	h := Handler(newTestDB(t), Options{Admission: gr.NewAdmission(4, 0)})
	rec := post(t, h, `{"statement":"RETURN 1 AS n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestQueryMaxTime confirms the server-wide wall-clock cap times a query out. A
// sub-nanosecond cap means the deadline has already passed when the engine checks its
// context at entry, so the query reports a timed out transaction with a 504.
func TestQueryMaxTime(t *testing.T) {
	h := Handler(newTestDB(t), Options{QueryMaxTime: time.Nanosecond})
	rec := post(t, h, `{"statement":"RETURN 1 AS n"}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec.Body.Bytes())
	errsAny, ok := body["errors"].([]any)
	if !ok || len(errsAny) == 0 {
		t.Fatalf("no errors in response: %v", body)
	}
	first := errsAny[0].(map[string]any)
	if first["code"] != "Neo.ClientError.Transaction.TransactionTimedOut" {
		t.Errorf("code = %v, want TransactionTimedOut", first["code"])
	}
}

// TestQueryMaxTimeAdmits confirms a generous cap does not interfere with a normal query.
func TestQueryMaxTimeAdmits(t *testing.T) {
	h := Handler(newTestDB(t), Options{QueryMaxTime: time.Minute})
	rec := post(t, h, `{"statement":"RETURN 1 AS n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
