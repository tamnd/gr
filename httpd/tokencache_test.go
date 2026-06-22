package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// countingProvider counts how many times Authenticate runs, so a test can prove the token
// cache short-circuits the provider on a repeated token.
type countingProvider struct {
	calls int
	princ *Principal
}

func (c *countingProvider) Authenticate(ctx context.Context, scheme, principal string, credential []byte) (*Principal, error) {
	c.calls++
	if c.princ == nil {
		return nil, ErrUnauthorized
	}
	return c.princ, nil
}

func (c *countingProvider) Schemes() []string { return []string{"bearer"} }

// serveCounting builds a Server over the counting provider with an injected clock and the
// given cache TTL.
func serveCounting(t *testing.T, cp *countingProvider, ttl time.Duration, clock *time.Time) *Server {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sv := New(db, Options{Auth: cp, TokenCacheTTL: ttl, now: func() time.Time { return *clock }})
	t.Cleanup(func() { sv.Close(); _ = db.Close() })
	return sv
}

func bearerGet(sv *Server, token string) int {
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"RETURN 1"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, r)
	return rec.Code
}

func TestTokenCacheHit(t *testing.T) {
	clock := baseClock
	cp := &countingProvider{princ: &Principal{
		Name:  "alice",
		Roles: []string{"admin"},
		Token: &Claims{Subject: "alice", ExpiresAt: clock.Add(time.Hour)},
	}}
	sv := serveCounting(t, cp, time.Minute, &clock)

	if code := bearerGet(sv, "tok-123"); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	if code := bearerGet(sv, "tok-123"); code != http.StatusOK {
		t.Fatalf("second request = %d, want 200", code)
	}
	if cp.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (second request should hit the cache)", cp.calls)
	}
	// A different token is a separate validation.
	if code := bearerGet(sv, "tok-456"); code != http.StatusOK {
		t.Fatalf("third request = %d, want 200", code)
	}
	if cp.calls != 2 {
		t.Errorf("provider calls = %d, want 2 after a new token", cp.calls)
	}
}

func TestTokenCacheExpiresByTTL(t *testing.T) {
	clock := baseClock
	cp := &countingProvider{princ: &Principal{
		Name:  "alice",
		Roles: []string{"admin"},
		Token: &Claims{Subject: "alice", ExpiresAt: clock.Add(time.Hour)}, // token outlives the TTL
	}}
	sv := serveCounting(t, cp, time.Minute, &clock)

	bearerGet(sv, "tok-123")
	if cp.calls != 1 {
		t.Fatalf("calls = %d, want 1", cp.calls)
	}
	// Advance past the cache TTL but not the token expiry: the cache entry is stale and
	// the provider revalidates.
	clock = clock.Add(2 * time.Minute)
	bearerGet(sv, "tok-123")
	if cp.calls != 2 {
		t.Errorf("calls = %d, want 2 after the cache TTL lapsed", cp.calls)
	}
}

func TestTokenCacheBoundedByTokenExpiry(t *testing.T) {
	clock := baseClock
	cp := &countingProvider{princ: &Principal{
		Name:  "alice",
		Roles: []string{"admin"},
		Token: &Claims{Subject: "alice", ExpiresAt: clock.Add(30 * time.Second)}, // shorter than the TTL
	}}
	sv := serveCounting(t, cp, time.Hour, &clock) // long TTL

	bearerGet(sv, "tok-123")
	if cp.calls != 1 {
		t.Fatalf("calls = %d, want 1", cp.calls)
	}
	// Past the token's own expiry though well within the long TTL: the cache must not
	// serve a principal past the life of the token it came from, so the provider runs again.
	clock = clock.Add(time.Minute)
	bearerGet(sv, "tok-123")
	if cp.calls != 2 {
		t.Errorf("calls = %d, want 2 after the token expiry, the cache must not outlive the token", cp.calls)
	}
}

func TestTokenCacheNotUsedForBasic(t *testing.T) {
	// A basic credential is re-checked every request (doc 18 §10.4), so the cache, which
	// only the bearer path consults, must not short-circuit it.
	clock := baseClock
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "admin"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	sv := New(db, Options{Auth: p, now: func() time.Time { return clock }})
	t.Cleanup(func() { sv.Close(); _ = db.Close() })

	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"RETURN 1"}`))
		r.Header.Set("Authorization", basic("alice", "secret"))
		rec := httptest.NewRecorder()
		sv.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("basic request %d = %d, want 200", i, rec.Code)
		}
	}
}
