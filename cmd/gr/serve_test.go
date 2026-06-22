package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServeBuildsHandler confirms gr serve opens the database, builds a handler, and
// hands it to the listener at the requested address. The injected listen stub captures
// the handler and exercises it in-process instead of binding a port.
func TestServeBuildsHandler(t *testing.T) {
	var gotAddr string
	// The real listener blocks until shutdown, so the database is open for the whole
	// run. The stub mirrors that by exercising the handler before it returns, while
	// runServe's deferred Close has not yet fired.
	listen := func(addr string, h http.Handler) error {
		gotAddr = addr
		req := httptest.NewRequest(http.MethodPost, "/db/graph/query/v2",
			strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("query status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return nil
	}
	var out, errw bytes.Buffer
	code := runServe([]string{"--addr", ":9999", "--name", "graph"}, &out, &errw, listen)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if gotAddr != ":9999" {
		t.Errorf("addr = %q, want :9999", gotAddr)
	}
}

// TestServeDefaultAddr confirms the default listen address is the Neo4j HTTP port.
func TestServeDefaultAddr(t *testing.T) {
	var gotAddr string
	listen := func(addr string, h http.Handler) error {
		gotAddr = addr
		return nil
	}
	var out, errw bytes.Buffer
	if code := runServe(nil, &out, &errw, listen); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if gotAddr != defaultServeAddr {
		t.Errorf("addr = %q, want %q", gotAddr, defaultServeAddr)
	}
}

// TestServeWithAuth confirms --user wires a credential provider into the handler, so a
// request without credentials is rejected and one with them succeeds.
func TestServeWithAuth(t *testing.T) {
	var h http.Handler
	listen := func(addr string, handler http.Handler) error {
		h = handler
		req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
			strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential query = %d, want 401", rec.Code)
		}

		req = httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
			strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("credentialed query = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
		return nil
	}
	var out, errw bytes.Buffer
	if code := runServe([]string{"--user", "alice:secret"}, &out, &errw, listen); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if h == nil {
		t.Fatal("handler not built")
	}
}

// TestServeBadUser rejects a malformed --user as a usage error.
func TestServeBadUser(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	var out, errw bytes.Buffer
	if code := runServe([]string{"--user", "nopassword"}, &out, &errw, listen); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeListenError surfaces a listener failure as the I/O exit code.
func TestServeListenError(t *testing.T) {
	listen := func(addr string, h http.Handler) error {
		return http.ErrServerClosed
	}
	var out, errw bytes.Buffer
	if code := runServe(nil, &out, &errw, listen); code != exitIO {
		t.Errorf("code = %d, want exitIO", code)
	}
}
