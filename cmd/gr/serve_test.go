package main

import (
	"bytes"
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
