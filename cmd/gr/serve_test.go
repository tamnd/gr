package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/vfs"
)

// writeTestCert generates a throwaway self-signed ECDSA certificate and key, writes them
// to temp files in PEM form, and returns their paths. It gives the TLS tests real material
// to load without checking a fixture into the tree.
func writeTestCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gr-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// noBolt is the Bolt-listener stub for the tests that do not enable --bolt. It is never
// called, since the serve command only reaches the Bolt path when --bolt is set.
func noBolt(ln *bolt.Listener) (io.Closer, error) {
	return io.NopCloser(nil), nil
}

// captureBolt returns a Bolt-listener stub that records the configured listener and a
// noop closer, so a test can inspect its address, handler, and TLS posture and drive the
// handler in-process without a port.
func captureBolt(ln **bolt.Listener) func(*bolt.Listener) (io.Closer, error) {
	return func(l *bolt.Listener) (io.Closer, error) {
		*ln = l
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
	listen := func(addr string, h http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, h http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
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
	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
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

	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	args := []string{"--auth-store", "--user", "alice:secret"}
	if code := runServe(args, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeImpersonationNeedsAuth rejects --auth-impersonation with no auth provider as a
// usage error, since impersonation needs a principal to authorize against.
func TestServeImpersonationNeedsAuth(t *testing.T) {
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	if code := runServe([]string{"--auth-impersonation"}, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeImpersonationEndToEnd serves with --user and --auth-impersonation and confirms
// an admin can run a query as another user over the wire.
func TestServeImpersonationEndToEnd(t *testing.T) {
	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
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
	listen := func(addr string, h http.Handler, _ *tls.Config) error {
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
	var ln *bolt.Listener
	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
		if ln == nil || ln.Server == nil || ln.Server.Handler == nil {
			t.Fatal("Bolt handler not captured before HTTP listen")
		}
		h := ln.Server.Handler
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
	code := runServe([]string{"--bolt"}, &out, &errw, listen, captureBolt(&ln))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if ln.Addr != defaultBoltAddr {
		t.Errorf("bolt addr = %q, want %q", ln.Addr, defaultBoltAddr)
	}
	if ln.TLSMode != bolt.TLSDisabled {
		t.Errorf("default TLS mode = %v, want disabled", ln.TLSMode)
	}
	if !strings.Contains(errw.String(), "serving Bolt") {
		t.Errorf("startup banner did not announce Bolt: %s", errw.String())
	}
}

// TestServeBoltAddr confirms --bolt-addr overrides the Bolt listen address.
func TestServeBoltAddr(t *testing.T) {
	var ln *bolt.Listener
	listen := func(addr string, handler http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	code := runServe([]string{"--bolt", "--bolt-addr", "127.0.0.1:9100"}, &out, &errw, listen, captureBolt(&ln))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if ln.Addr != "127.0.0.1:9100" {
		t.Errorf("bolt addr = %q, want 127.0.0.1:9100", ln.Addr)
	}
}

// TestServeBoltAuth confirms --bolt with an auth provider routes Bolt authentication
// through the same provider as HTTP: good credentials pass, a wrong password fails, and
// the none scheme is rejected.
func TestServeBoltAuth(t *testing.T) {
	var ln *bolt.Listener
	listen := func(addr string, handler http.Handler, _ *tls.Config) error {
		h := ln.Server.Handler
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
	code := runServe([]string{"--user", "alice:secret", "--bolt"}, &out, &errw, listen, captureBolt(&ln))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
}

// TestServeBoltTLS confirms --bolt-tls with a certificate and key configures the listener
// for the requested posture and loads the certificate with the hardening defaults.
func TestServeBoltTLS(t *testing.T) {
	certPath, keyPath := writeTestCert(t)
	var ln *bolt.Listener
	listen := func(addr string, handler http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	args := []string{"--bolt", "--bolt-tls", "required", "--tls-cert", certPath, "--tls-key", keyPath}
	code := runServe(args, &out, &errw, listen, captureBolt(&ln))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if ln.TLSMode != bolt.TLSRequired {
		t.Errorf("TLS mode = %v, want required", ln.TLSMode)
	}
	if ln.TLSConfig == nil {
		t.Fatal("TLS config not loaded")
	}
	if ln.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("min version = %x, want TLS 1.2", ln.TLSConfig.MinVersion)
	}
	if len(ln.TLSConfig.Certificates) != 1 {
		t.Errorf("certificates = %d, want 1", len(ln.TLSConfig.Certificates))
	}
	if !strings.Contains(errw.String(), "TLS required") {
		t.Errorf("banner did not announce TLS posture: %s", errw.String())
	}
}

// TestServeBoltTLSOptional confirms the optional posture also loads a certificate, since
// it serves TLS to clients that offer a ClientHello.
func TestServeBoltTLSOptional(t *testing.T) {
	certPath, keyPath := writeTestCert(t)
	var ln *bolt.Listener
	listen := func(addr string, handler http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	args := []string{"--bolt", "--bolt-tls", "optional", "--tls-cert", certPath, "--tls-key", keyPath}
	code := runServe(args, &out, &errw, listen, captureBolt(&ln))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if ln.TLSMode != bolt.TLSOptional {
		t.Errorf("TLS mode = %v, want optional", ln.TLSMode)
	}
	if ln.TLSConfig == nil {
		t.Fatal("optional posture did not load a certificate")
	}
}

// TestServeBoltTLSMissingCert rejects an encrypted posture with no certificate material.
func TestServeBoltTLSMissingCert(t *testing.T) {
	listen := func(addr string, handler http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	code := runServe([]string{"--bolt", "--bolt-tls", "required"}, &out, &errw, listen, captureBolt(new(*bolt.Listener)))
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
	if !strings.Contains(errw.String(), "tls-cert") {
		t.Errorf("error did not name the missing flag: %s", errw.String())
	}
}

// TestServeBoltTLSInvalidMode rejects an unknown posture.
func TestServeBoltTLSInvalidMode(t *testing.T) {
	listen := func(addr string, handler http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	code := runServe([]string{"--bolt", "--bolt-tls", "maybe"}, &out, &errw, listen, captureBolt(new(*bolt.Listener)))
	if code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeBoltError surfaces a Bolt listener bind failure as the I/O exit code.
func TestServeBoltError(t *testing.T) {
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
	boltServe := func(ln *bolt.Listener) (io.Closer, error) {
		return nil, errors.New("bind failed")
	}
	var out, errw bytes.Buffer
	if code := runServe([]string{"--bolt"}, &out, &errw, listen, boltServe); code != exitIO {
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
	ln := &bolt.Listener{Server: &bolt.Server{Handler: db.BoltHandler()}, Addr: "127.0.0.1:0"}
	closer, err := startBolt(ln)
	if err != nil {
		t.Fatalf("start bolt: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("close bolt: %v", err)
	}
}

// TestServeMaxInFlight confirms the --max-in-flight flag is accepted and the server
// still serves; the gate's behavior is covered at the unit and adapter level.
func TestServeMaxInFlight(t *testing.T) {
	var ln *bolt.Listener
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	code := runServe([]string{"--bolt", "--max-in-flight", "4"}, &out, &errw, listen, captureBolt(&ln))
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if ln == nil || ln.Server == nil || ln.Server.Handler == nil {
		t.Fatal("Bolt handler not configured with the gate")
	}
}

// TestServeHTTPTLS confirms --http-tls required hands the HTTP listener a TLS config
// loaded from the shared certificate material with the hardening defaults.
func TestServeHTTPTLS(t *testing.T) {
	certPath, keyPath := writeTestCert(t)
	var gotTLS *tls.Config
	listen := func(addr string, h http.Handler, c *tls.Config) error {
		gotTLS = c
		return nil
	}
	var out, errw bytes.Buffer
	args := []string{"--http-tls", "required", "--tls-cert", certPath, "--tls-key", keyPath}
	code := runServe(args, &out, &errw, listen, noBolt)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if gotTLS == nil {
		t.Fatal("HTTP listener got no TLS config")
	}
	if gotTLS.MinVersion != tls.VersionTLS12 {
		t.Errorf("min version = %x, want TLS 1.2", gotTLS.MinVersion)
	}
	if len(gotTLS.Certificates) != 1 {
		t.Errorf("certificates = %d, want 1", len(gotTLS.Certificates))
	}
	if !strings.Contains(errw.String(), "TLS required") {
		t.Errorf("banner did not announce HTTP TLS posture: %s", errw.String())
	}
}

// TestServeHTTPPlaintext confirms the default posture hands the listener a nil TLS config.
func TestServeHTTPPlaintext(t *testing.T) {
	var sawTLS bool
	listen := func(addr string, h http.Handler, c *tls.Config) error {
		sawTLS = c != nil
		return nil
	}
	var out, errw bytes.Buffer
	if code := runServe(nil, &out, &errw, listen, noBolt); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errw.String())
	}
	if sawTLS {
		t.Error("plaintext HTTP listener got a TLS config")
	}
}

// TestServeHTTPTLSMissingCert rejects --http-tls required with no certificate material.
func TestServeHTTPTLSMissingCert(t *testing.T) {
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	if code := runServe([]string{"--http-tls", "required"}, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}

// TestServeHTTPTLSInvalidMode rejects an unknown HTTP posture.
func TestServeHTTPTLSInvalidMode(t *testing.T) {
	listen := func(addr string, h http.Handler, _ *tls.Config) error { return nil }
	var out, errw bytes.Buffer
	if code := runServe([]string{"--http-tls", "maybe"}, &out, &errw, listen, noBolt); code != exitUsage {
		t.Errorf("code = %d, want exitUsage", code)
	}
}
