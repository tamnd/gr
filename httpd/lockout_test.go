package httpd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// lockoutProvider builds a StaticProvider with one user, a controllable clock, and the
// given lockout policy, so a test drives lockout expiry without sleeping.
func lockoutProvider(t *testing.T, clock *time.Time, maxFailed int, dur time.Duration) *StaticProvider {
	t.Helper()
	p := NewStaticProvider().SetLockout(maxFailed, dur)
	p.now = func() time.Time { return *clock }
	if err := p.AddUser("alice", "secret", "admin"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	return p
}

func TestLockoutAfterThreshold(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	p := lockoutProvider(t, &clock, 3, time.Minute)

	// Three wrong attempts are ordinary failures and lock the principal on the third.
	for i := 0; i < 3; i++ {
		if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("wrong")); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("attempt %d err = %v, want ErrUnauthorized", i, err)
		}
	}
	// Now even the correct password is refused, and the lockout is distinguishable from
	// an ordinary failure (it wraps ErrUnauthorized so a generic caller still sees a fail).
	_, err := p.Authenticate(t.Context(), "basic", "alice", []byte("secret"))
	if !errors.Is(err, ErrLockedOut) {
		t.Errorf("locked correct-password err = %v, want ErrLockedOut", err)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("ErrLockedOut should wrap ErrUnauthorized, got %v", err)
	}
}

func TestLockoutExpires(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	p := lockoutProvider(t, &clock, 3, time.Minute)
	for i := 0; i < 3; i++ {
		_, _ = p.Authenticate(t.Context(), "basic", "alice", []byte("wrong"))
	}
	// Still locked right now.
	if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("secret")); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("err before expiry = %v, want ErrLockedOut", err)
	}
	// Advance past the lockout: the correct password works again.
	clock = clock.Add(time.Minute + time.Second)
	princ, err := p.Authenticate(t.Context(), "basic", "alice", []byte("secret"))
	if err != nil {
		t.Fatalf("err after expiry = %v, want success", err)
	}
	if princ.Name != "alice" {
		t.Errorf("principal = %+v", princ)
	}
}

func TestLockoutResetOnSuccess(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	p := lockoutProvider(t, &clock, 3, time.Minute)

	// Two failures, then a success resets the counter.
	for i := 0; i < 2; i++ {
		_, _ = p.Authenticate(t.Context(), "basic", "alice", []byte("wrong"))
	}
	if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("secret")); err != nil {
		t.Fatalf("success before threshold err = %v", err)
	}
	// Two more failures must not lock, since the counter restarted at the success.
	for i := 0; i < 2; i++ {
		if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("wrong")); !errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrLockedOut) {
			t.Fatalf("post-reset failure %d err = %v, want plain ErrUnauthorized", i, err)
		}
	}
	if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("secret")); err != nil {
		t.Errorf("correct password after reset err = %v, want success", err)
	}
}

func TestLockoutDisabled(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	p := lockoutProvider(t, &clock, 0, time.Minute) // 0 disables lockout

	for i := 0; i < 10; i++ {
		if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("wrong")); errors.Is(err, ErrLockedOut) {
			t.Fatalf("attempt %d locked despite lockout disabled", i)
		}
	}
	if _, err := p.Authenticate(t.Context(), "basic", "alice", []byte("secret")); err != nil {
		t.Errorf("correct password with lockout disabled err = %v", err)
	}
}

// TestLockoutMetric drives the lockout end to end over HTTP and confirms the
// auth_total{outcome="lockout"} counter advances once the principal is locked.
func TestLockoutMetric(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := time.Unix(1_700_000_000, 0)
	p := lockoutProvider(t, &clock, 2, time.Minute)
	sv := New(db, Options{Auth: p})

	send := func(authz string) int {
		r := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"RETURN 1"}`))
		r.Header.Set("Authorization", authz)
		rec := httptest.NewRecorder()
		sv.ServeHTTP(rec, r)
		return rec.Code
	}
	// Two wrong attempts lock alice; a third (even correct) is refused.
	send(basic("alice", "wrong"))
	send(basic("alice", "wrong"))
	if code := send(basic("alice", "secret")); code != http.StatusUnauthorized {
		t.Errorf("locked correct-password request = %d, want 401", code)
	}

	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "gr_server_auth_total{outcome=\"lockout\"} 1") {
		t.Errorf("metrics missing lockout count\nbody:\n%s", body)
	}
	if !strings.Contains(body, "gr_server_auth_total{outcome=\"failure\"} 2") {
		t.Errorf("metrics missing the two ordinary failures\nbody:\n%s", body)
	}
}
