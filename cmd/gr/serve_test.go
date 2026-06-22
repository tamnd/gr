package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestServeUserRoles confirms --user grants the roles named in its third field, so a
// reader is refused a write while an editor is allowed one, and a roleless --user
// defaults to admin (full access).
func TestServeUserRoles(t *testing.T) {
	listen := func(addr string, handler http.Handler) error {
		send := func(user, statement string) int {
			body := `{"statement":"` + statement + `"}`
			req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(body))
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":pw")))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec.Code
		}
		if code := send("reader", "CREATE (:T)"); code != http.StatusForbidden {
			t.Errorf("reader write = %d, want 403", code)
		}
		if code := send("reader", "RETURN 1"); code != http.StatusOK {
			t.Errorf("reader read = %d, want 200", code)
		}
		if code := send("writer", "CREATE (:T)"); code != http.StatusOK {
			t.Errorf("editor write = %d, want 200", code)
		}
		if code := send("boss", "CREATE INDEX FOR (p:T) ON (p.x)"); code != http.StatusOK {
			t.Errorf("default-admin schema = %d, want 200", code)
		}
		return nil
	}
	var out, errw bytes.Buffer
	args := []string{"--user", "reader:pw:reader", "--user", "writer:pw:editor", "--user", "boss:pw"}
	if code := runServe(args, &out, &errw, listen); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
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

// mintServeHS256 mints an HS256 JWT for the serve tests, so the bearer wiring is exercised
// without importing the httpd test helpers.
func mintServeHS256(t *testing.T, secret []byte, claims map[string]any) string {
	t.Helper()
	b64 := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	in := b64.EncodeToString(header) + "." + b64.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(in))
	return in + "." + b64.EncodeToString(mac.Sum(nil))
}

// TestServeJWT confirms --jwt-hmac-secret wires the bearer-token provider: a valid token
// authenticates and its roles authorize, and a request with no token is refused.
func TestServeJWT(t *testing.T) {
	secret := "serve-signing-secret"
	tok := mintServeHS256(t, []byte(secret), map[string]any{
		"sub":   "alice",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
		"roles": []string{"editor"},
	})
	listen := func(addr string, handler http.Handler) error {
		send := func(authz, statement string) int {
			req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"`+statement+`"}`))
			if authz != "" {
				req.Header.Set("Authorization", authz)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec.Code
		}
		if code := send("", "RETURN 1"); code != http.StatusUnauthorized {
			t.Errorf("no-token query = %d, want 401", code)
		}
		if code := send("Bearer "+tok, "CREATE (:T)"); code != http.StatusOK {
			t.Errorf("editor-token write = %d, want 200", code)
		}
		return nil
	}
	var out, errw bytes.Buffer
	if code := runServe([]string{"--jwt-hmac-secret", secret}, &out, &errw, listen); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeAuthConflict rejects mixing --user and --jwt-* as a usage error, since the
// server runs one provider at a time.
func TestServeAuthConflict(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	var out, errw bytes.Buffer
	args := []string{"--user", "alice:secret", "--jwt-hmac-secret", "x"}
	if code := runServe(args, &out, &errw, listen); code != exitUsage {
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
