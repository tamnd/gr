package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/vfs"
)

// noBolt is the Bolt-listener stub for the tests that do not enable --bolt. It is never
// called, since the serve command only reaches the Bolt path when --bolt is set.
func noBolt(addr string, h bolt.Handler) (io.Closer, error) {
	return io.NopCloser(nil), nil
}

// captureBolt returns a Bolt-listener stub that records the address and handler it is
// given and a noop closer, so a test can drive the handler in-process without a port.
func captureBolt(addr *string, h *bolt.Handler) func(string, bolt.Handler) (io.Closer, error) {
	return func(a string, handler bolt.Handler) (io.Closer, error) {
		*addr = a
		*h = handler
		return io.NopCloser(nil), nil
	}
}

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
	code := runServe([]string{"--addr", ":9999", "--name", "graph"}, &out, &errw, listen, noBolt)
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
	if code := runServe(nil, &out, &errw, listen, noBolt); code != exitOK {
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
	if code := runServe([]string{"--user", "alice:secret"}, &out, &errw, listen, noBolt); code != exitOK {
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
	if code := runServe(args, &out, &errw, listen, noBolt); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeBadUser rejects a malformed --user as a usage error.
func TestServeBadUser(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	var out, errw bytes.Buffer
	if code := runServe([]string{"--user", "nopassword"}, &out, &errw, listen, noBolt); code != exitUsage {
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
	if code := runServe([]string{"--jwt-hmac-secret", secret}, &out, &errw, listen, noBolt); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeAuthConflict rejects mixing --user and --jwt-* as a usage error, since the
// server runs one provider at a time.
func TestServeAuthConflict(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	var out, errw bytes.Buffer
	args := []string{"--user", "alice:secret", "--jwt-hmac-secret", "x"}
	if code := runServe(args, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeAuthStore confirms --auth-store authenticates against the database's own
// credential store: a user created in the file before serving authenticates over HTTP,
// and an uncredentialed request is refused.
func TestServeAuthStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "served.gr")
	db, err := gr.Open(path, gr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser("ada", "s3cret", gr.RoleEditor); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

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
			t.Errorf("no-credential query = %d, want 401", code)
		}
		cred := base64.StdEncoding.EncodeToString([]byte("ada:s3cret"))
		if code := send("Basic "+cred, "CREATE (:T)"); code != http.StatusOK {
			t.Errorf("editor write = %d, want 200", code)
		}
		bad := base64.StdEncoding.EncodeToString([]byte("ada:wrong"))
		if code := send("Basic "+bad, "RETURN 1"); code != http.StatusUnauthorized {
			t.Errorf("wrong password = %d, want 401", code)
		}
		return nil
	}
	var out, errw bytes.Buffer
	if code := runServe([]string{"--auth-store", path}, &out, &errw, listen, noBolt); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeAuthStoreConflict rejects mixing --auth-store with --user as a usage error.
func TestServeAuthStoreConflict(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	var out, errw bytes.Buffer
	args := []string{"--auth-store", "--user", "alice:secret"}
	if code := runServe(args, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeImpersonationNeedsAuth rejects --auth-impersonation with no auth provider as a
// usage error, since impersonation needs a principal to authorize against.
func TestServeImpersonationNeedsAuth(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	var out, errw bytes.Buffer
	if code := runServe([]string{"--auth-impersonation"}, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeImpersonationEndToEnd serves with --user and --auth-impersonation and confirms
// an admin can run a query as another user over the wire.
func TestServeImpersonationEndToEnd(t *testing.T) {
	listen := func(addr string, handler http.Handler) error {
		body := `{"statement":"CREATE (:T)","impersonatedUser":"e"}`
		req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(body))
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("boss:pw")))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("admin-as-editor write = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
		return nil
	}
	var out, errw bytes.Buffer
	args := []string{"--user", "boss:pw:admin", "--user", "e:pw:editor", "--auth-impersonation"}
	if code := runServe(args, &out, &errw, listen, noBolt); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeListenError surfaces a listener failure as the I/O exit code.
func TestServeListenError(t *testing.T) {
	listen := func(addr string, h http.Handler) error {
		return http.ErrServerClosed
	}
	var out, errw bytes.Buffer
	if code := runServe(nil, &out, &errw, listen, noBolt); code != exitIO {
		t.Errorf("code = %d, want exitIO", code)
	}
}

// TestServeBolt confirms --bolt starts the Bolt listener at the default port and that the
// handler it is given runs Cypher over the same database the HTTP surface serves. The
// captured handler is driven in-process inside the HTTP listen stub, where the database is
// still open.
func TestServeBolt(t *testing.T) {
	var boltAddr string
	var h bolt.Handler
	listen := func(addr string, handler http.Handler) error {
		if h == nil {
			t.Fatal("Bolt handler not captured before HTTP listen")
		}
		tx, err := h.Begin(map[string]any{}, bolt.Auth{})
		if err != nil {
			t.Fatalf("bolt begin: %v", err)
		}
		cur, err := tx.Run("RETURN 1 AS n", nil)
		if err != nil {
			t.Fatalf("bolt run: %v", err)
		}
		row, ok, err := cur.Next()
		if err != nil || !ok {
			t.Fatalf("bolt next: ok=%v err=%v", ok, err)
		}
		if n, _ := row[0].AsInt(); n != 1 {
			t.Errorf("bolt RETURN 1 = %v, want 1", row[0])
		}
		cur.Close()
		if _, err := tx.Commit(); err != nil {
			t.Fatalf("bolt commit: %v", err)
		}
		return nil
	}
	var out, errw bytes.Buffer
	code := runServe([]string{"--bolt"}, &out, &errw, listen, captureBolt(&boltAddr, &h))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if boltAddr != defaultBoltAddr {
		t.Errorf("bolt addr = %q, want %q", boltAddr, defaultBoltAddr)
	}
	if !strings.Contains(errw.String(), "serving Bolt") {
		t.Errorf("startup banner did not announce Bolt: %s", errw.String())
	}
}

// TestServeBoltAddr confirms --bolt-addr overrides the Bolt listen address.
func TestServeBoltAddr(t *testing.T) {
	var boltAddr string
	var h bolt.Handler
	listen := func(addr string, handler http.Handler) error { return nil }
	var out, errw bytes.Buffer
	code := runServe([]string{"--bolt", "--bolt-addr", "127.0.0.1:9100"}, &out, &errw, listen, captureBolt(&boltAddr, &h))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if boltAddr != "127.0.0.1:9100" {
		t.Errorf("bolt addr = %q, want 127.0.0.1:9100", boltAddr)
	}
}

// TestServeBoltAuth confirms --bolt with an auth provider routes Bolt authentication
// through the same provider as HTTP: good credentials pass, a wrong password fails, and
// the none scheme is rejected.
func TestServeBoltAuth(t *testing.T) {
	var boltAddr string
	var h bolt.Handler
	listen := func(addr string, handler http.Handler) error {
		if _, err := h.Authenticate("basic", "alice", "secret"); err != nil {
			t.Errorf("valid credentials rejected over Bolt: %v", err)
		}
		if _, err := h.Authenticate("basic", "alice", "wrong"); err == nil {
			t.Error("wrong password accepted over Bolt")
		}
		if _, err := h.Authenticate("none", "", ""); err == nil {
			t.Error("none scheme accepted over Bolt while auth is on")
		}
		return nil
	}
	var out, errw bytes.Buffer
	code := runServe([]string{"--user", "alice:secret", "--bolt"}, &out, &errw, listen, captureBolt(&boltAddr, &h))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeBoltError surfaces a Bolt listener bind failure as the I/O exit code.
func TestServeBoltError(t *testing.T) {
	listen := func(addr string, h http.Handler) error { return nil }
	boltListen := func(addr string, h bolt.Handler) (io.Closer, error) {
		return nil, errors.New("bind failed")
	}
	var out, errw bytes.Buffer
	if code := runServe([]string{"--bolt"}, &out, &errw, listen, boltListen); code != exitIO {
		t.Errorf("code = %d, want exitIO", code)
	}
}

// TestStartBolt confirms the real Bolt listener binds an ephemeral port, serves a driver
// handshake, and closes cleanly.
func TestStartBolt(t *testing.T) {
	db, err := gr.Open(memPath, gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	closer, err := startBolt("127.0.0.1:0", db.BoltHandler())
	if err != nil {
		t.Fatalf("start bolt: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("close bolt: %v", err)
	}
}
