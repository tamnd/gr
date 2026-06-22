package httpd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// captureServerEvents builds an event log writing JSON into a buffer plus a reader that
// parses the buffer into one map per entry, so a server test can assert what was emitted.
func captureServerEvents() (*gr.EventLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: gr.LevelTrace}))
	el := gr.NewEventLog(logger)
	read := func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				panic(err)
			}
			out = append(out, m)
		}
		return out
	}
	return el, read
}

// findEvent returns the first entry with the given event name, or nil.
func findEvent(entries []map[string]any, name string) map[string]any {
	for _, e := range entries {
		if e["event"] == name {
			return e
		}
	}
	return nil
}

// TestServerEmitsAuthFailure confirms a rejected credential emits an auth_failure event
// naming the claimed user, the client, and a non-disclosing reason, and that a successful
// request does not.
func TestServerEmitsAuthFailure(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "editor"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	el, read := captureServerEvents()
	h := Handler(db, Options{Auth: p, EventLog: el})

	// A wrong password is rejected and recorded.
	bad := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:wrong"))
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	r.Header.Set("Authorization", bad)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	e := findEvent(read(), gr.EventAuthFailure)
	if e == nil {
		t.Fatalf("no auth_failure event emitted")
	}
	if e["user"] != "alice" {
		t.Errorf("user = %v, want alice", e["user"])
	}
	if e["reason"] != "invalid credentials" {
		t.Errorf("reason = %v, want invalid credentials", e["reason"])
	}
	if e["client"] == nil || e["client"] == "" {
		t.Errorf("client missing")
	}

	// A successful request emits no auth_failure.
	good := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	r = httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	r.Header.Set("Authorization", good)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := len(read()); got != 1 {
		t.Errorf("entries = %d, want 1 (the success added none)", got)
	}
}

// TestServerEmitsAuthFailureNoHeader confirms a missing credential is recorded as an
// anonymous auth failure with the no-credentials reason.
func TestServerEmitsAuthFailureNoHeader(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "editor"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	el, read := captureServerEvents()
	h := Handler(db, Options{Auth: p, EventLog: el})

	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	e := findEvent(read(), gr.EventAuthFailure)
	if e == nil {
		t.Fatalf("no auth_failure event emitted")
	}
	if e["user"] != "anonymous" {
		t.Errorf("user = %v, want anonymous", e["user"])
	}
	if e["reason"] != "no credentials presented" {
		t.Errorf("reason = %v, want no credentials presented", e["reason"])
	}
}

// TestServerEmitsOverload confirms a query shed by the admission gate emits an overload
// event with the shed action.
func TestServerEmitsOverload(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	el, read := captureServerEvents()
	// A gate of one slot with no queue wait sheds the moment a slot is held. We hold the
	// only slot ourselves so the request's acquire sheds immediately.
	gate := gr.NewAdmission(1, 0)
	release, err := gate.Acquire(t.Context())
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer release()
	h := Handler(db, Options{Admission: gate, EventLog: el})

	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	e := findEvent(read(), gr.EventOverload)
	if e == nil {
		t.Fatalf("no overload event emitted (status %d, body %s)", rec.Code, rec.Body.String())
	}
	if e["action"] != "shed" {
		t.Errorf("action = %v, want shed", e["action"])
	}
}

// TestServerEventsNilDisabled confirms a server with no event log neither panics nor
// records on an auth failure.
func TestServerEventsNilDisabled(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "editor"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	h := Handler(db, Options{Auth: p})

	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r) // must not panic
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
