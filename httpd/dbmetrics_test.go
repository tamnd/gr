package httpd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// TestMetricsEndpointRendersDatabaseRegistry confirms GET /metrics renders both the server
// plane (gr_server_) and the database registry (gr_) after a query has run.
func TestMetricsEndpointRendersDatabaseRegistry(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Run and drain a read so the query metrics record.
	res, err := db.Run(context.Background(), "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	_ = res.Close()

	h := Handler(db, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE gr_server_auth_total counter", // server plane
		"# TYPE gr_queries_total counter",     // database registry
		`gr_queries_total{kind="read",status="ok"} 1`,
		"# TYPE gr_query_duration_seconds histogram",
		`gr_query_duration_seconds_count{kind="read"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n%s", want, body)
		}
	}
}

// TestAuthFeedsSharedRegistry confirms an HTTP authentication outcome lands on the shared
// gr_auth_attempts_total, the unified auth metric the Bolt surface also feeds (doc 20 §3.3): a good
// credential counts as ok and a bad one as denied.
func TestAuthFeedsSharedRegistry(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "editor"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	h := Handler(db, Options{Auth: p})

	post := func(authz string) {
		r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
			strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", authz)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	enc := func(u, pw string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+pw))
	}
	post(enc("alice", "secret"))
	post(enc("alice", "wrong"))

	snap := db.Metrics()
	if c := snap.Counter("gr_auth_attempts_total", gr.Labels{"result": "ok"}); c != 1 {
		t.Errorf("auth ok = %d, want 1", c)
	}
	if c := snap.Counter("gr_auth_attempts_total", gr.Labels{"result": "denied"}); c != 1 {
		t.Errorf("auth denied = %d, want 1", c)
	}
}

// TestVarsEndpoint confirms GET /debug/vars renders the database registry as valid expvar JSON.
func TestVarsEndpoint(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := db.Run(context.Background(), "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	_ = res.Close()

	h := Handler(db, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/vars", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var tree map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatalf("vars body is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	grRoot, ok := tree["gr"].(map[string]any)
	if !ok {
		t.Fatalf("missing gr root: %v", tree)
	}
	q, ok := grRoot["queries_total"].(map[string]any)
	if !ok {
		t.Fatalf("missing queries_total: %v", grRoot)
	}
	read := q["read"].(map[string]any)
	if read["ok"].(float64) != 1 {
		t.Errorf("queries_total.read.ok = %v, want 1", read["ok"])
	}
}

// TestVarsEndpointRejectsPost confirms the expvar endpoint is GET-only.
func TestVarsEndpointRejectsPost(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := Handler(db, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/vars", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /debug/vars status = %d, want 404", rec.Code)
	}
}
