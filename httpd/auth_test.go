package httpd

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// authHandler builds a handler whose provider knows the given user/password pairs.
func authHandler(t *testing.T, users map[string]string) http.Handler {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	for u, pw := range users {
		if err := p.AddUser(u, pw, "editor"); err != nil {
			t.Fatalf("add user: %v", err)
		}
	}
	return Handler(db, Options{Auth: p})
}

// basic builds an Authorization: Basic header value.
func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// authPost sends a query with an optional Authorization header.
func authPost(t *testing.T, h http.Handler, authz string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2",
		strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	r.Header.Set("Content-Type", "application/json")
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAuthRequiredNoHeader(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	rec := authPost(t, h, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Basic") {
		t.Errorf("WWW-Authenticate = %q", wa)
	}
}

func TestAuthBasicSuccess(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	rec := authPost(t, h, basic("alice", "secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAuthWrongPassword(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	rec := authPost(t, h, basic("alice", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthUnknownUser(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	rec := authPost(t, h, basic("mallory", "secret"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthBearerRejectedByBasicProvider(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	rec := authPost(t, h, "Bearer sometoken")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthMalformedHeader(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	for _, hdr := range []string{"Basic", "Basic !!!notbase64", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")), "Weird scheme"} {
		rec := authPost(t, h, hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", hdr, rec.Code)
		}
	}
}

func TestAuthDisabledAnonymous(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := Handler(db, Options{}) // no Auth provider
	rec := authPost(t, h, "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (auth disabled)", rec.Code)
	}
}

func TestHealthAndDiscoveryStayOpen(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "secret"})
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s without auth = %d, want 200", path, rec.Code)
		}
	}
}

func TestTxOwnershipScoping(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "apw", "bob": "bpw"})

	// Alice opens a transaction.
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/tx", strings.NewReader(`{"statements":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("alice", "apw"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("begin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	id := decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)

	// Bob, authenticated but not the owner, cannot touch it: 404, not even confirming
	// the id exists.
	r = httptest.NewRequest(http.MethodPost, "/db/neo4j/tx/"+id, strings.NewReader(`{"statements":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", basic("bob", "bpw"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("bob touching alice's tx = %d, want 404", rec.Code)
	}

	// Alice can still use it.
	r = httptest.NewRequest(http.MethodDelete, "/db/neo4j/tx/"+id, nil)
	r.Header.Set("Authorization", basic("alice", "apw"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("alice rollback = %d, want 200", rec.Code)
	}
}

func TestStaticProviderDirect(t *testing.T) {
	p := NewStaticProvider()
	if err := p.AddUser("carol", "pw", "reader", "editor"); err != nil {
		t.Fatalf("add: %v", err)
	}
	princ, err := p.Authenticate(t.Context(), "basic", "carol", []byte("pw"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if princ.Name != "carol" || len(princ.Roles) != 2 {
		t.Errorf("principal = %+v", princ)
	}
	if _, err := p.Authenticate(t.Context(), "basic", "carol", []byte("nope")); err == nil {
		t.Error("wrong password authenticated")
	}
	if _, err := p.Authenticate(t.Context(), "bearer", "", []byte("tok")); err == nil {
		t.Error("bearer accepted by basic-only provider")
	}
}
