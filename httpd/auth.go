package httpd

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// AuthProvider verifies a credential and yields an authenticated principal (doc 18
// §10.4). It is the seam a deployment replaces to plug an external identity system in;
// the built-in StaticProvider is the default. The same seam backs both the HTTP and
// the Bolt transport, so a deployment configures auth once; Bolt is a later arc, so
// only HTTP consumes it today.
type AuthProvider interface {
	// Authenticate verifies the credential for the given scheme and returns the
	// authenticated principal with its roles, or an error.
	Authenticate(ctx context.Context, scheme, principal string, credential []byte) (*Principal, error)
	// Schemes reports which auth schemes this provider verifies, so the server
	// rejects an unsupported scheme cleanly.
	Schemes() []string
}

// Principal is an authenticated identity and its roles (doc 18 §10.4, §10.6). The
// roles drive the authorization model, which is enforced in a later slice; this slice
// authenticates and carries the principal so the transaction store can scope ownership
// to it (doc 18 §9.9).
type Principal struct {
	Name  string
	Roles []string
}

// ErrUnauthorized is the credential-check failure, mapped to 401 (doc 18 §12). The
// provider returns it for a bad username, a bad password, or an unsupported scheme, so
// the response never reveals which, the standard non-disclosing auth failure.
var ErrUnauthorized = errors.New("gr: unauthorized")

// anonymous is the principal a request carries when authentication is disabled (no
// provider configured), so the rest of the server treats the auth-on and auth-off
// cases uniformly: the principal is just empty-named with no roles.
var anonymous = &Principal{Name: ""}

// pbkdf2Iter is the PBKDF2 iteration count for the built-in store. The spec prefers
// Argon2id or scrypt (doc 18 §10.3), but both live outside the standard library and gr
// is zero-dependency, so the built-in store uses stdlib PBKDF2-HMAC-SHA256 with a high
// iteration count; the provider seam is the place to plug a stronger external KDF.
const pbkdf2Iter = 210_000

// storedUser is one credential in the built-in store: a per-user random salt, the
// derived hash, and the granted roles.
type storedUser struct {
	salt  []byte
	hash  []byte
	roles []string
}

// StaticProvider is the built-in in-memory credential store (doc 18 §10.3). It keeps
// salted PBKDF2 hashes, never plaintext, and verifies with a constant-time compare. It
// is the default provider for a served database; the persistent credential store in
// the database's reserved system area (doc 08, with ALTER USER / CREATE USER) is a
// later slice, so this provider is configured in code at startup for now.
type StaticProvider struct {
	mu    sync.RWMutex
	users map[string]storedUser
	// dummy is a throwaway hash compared against when the named user does not exist,
	// so a missing user takes the same time as a wrong password and the lookup does
	// not leak which usernames exist through timing.
	dummy []byte
}

// NewStaticProvider returns an empty credential store.
func NewStaticProvider() *StaticProvider {
	dummy, _ := derive("", make([]byte, 16))
	return &StaticProvider{users: make(map[string]storedUser), dummy: dummy}
}

// AddUser adds or replaces a user with a salted hash of the password and the given
// roles. A fresh random salt is drawn per user.
func (p *StaticProvider) AddUser(name, password string, roles ...string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := derive(password, salt)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.users[name] = storedUser{salt: salt, hash: hash, roles: roles}
	p.mu.Unlock()
	return nil
}

// Authenticate verifies a basic credential against the store (doc 18 §10.3). It only
// handles the basic scheme; a bearer or any other scheme is rejected, so a deployment
// that needs tokens installs a provider that supports them. The comparison is
// constant-time, and a missing user is compared against a dummy hash so it costs the
// same as a wrong password.
func (p *StaticProvider) Authenticate(ctx context.Context, scheme, principal string, credential []byte) (*Principal, error) {
	if scheme != "basic" {
		return nil, ErrUnauthorized
	}
	p.mu.RLock()
	u, ok := p.users[principal]
	p.mu.RUnlock()
	if !ok {
		// Compare against the dummy hash so a missing user is indistinguishable
		// from a wrong password by timing, then fail.
		_, _ = derive(string(credential), make([]byte, 16))
		subtle.ConstantTimeCompare(p.dummy, p.dummy)
		return nil, ErrUnauthorized
	}
	got, err := derive(string(credential), u.salt)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(got, u.hash) != 1 {
		return nil, ErrUnauthorized
	}
	return &Principal{Name: principal, Roles: append([]string(nil), u.roles...)}, nil
}

// Schemes reports that the built-in store verifies only the basic scheme.
func (p *StaticProvider) Schemes() []string { return []string{"basic"} }

// derive computes the PBKDF2-HMAC-SHA256 hash of a password with a salt.
func derive(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, 32)
}

// authFail describes a rejected request: the apiError to return and the scheme to put
// in the WWW-Authenticate header. The status is always 401 for an auth failure.
type authFail struct {
	err    apiError
	scheme string
}

// authenticate verifies a request and returns its principal (doc 18 §9.8). With no
// provider configured authentication is off and every request is anonymous. Otherwise
// the Authorization header is parsed and verified; a missing or bad credential is a
// 401 with a WWW-Authenticate header.
func (s *server) authenticate(r *http.Request) (*Principal, *authFail) {
	if s.auth == nil {
		return anonymous, nil
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, &authFail{
			err:    apiError{Code: "Neo.ClientError.Security.Unauthorized", Message: "authentication required"},
			scheme: "Basic realm=\"gr\"",
		}
	}
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		return nil, unauthorized()
	}
	switch {
	case strings.EqualFold(scheme, "Basic"):
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil, unauthorized()
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return nil, unauthorized()
		}
		princ, err := s.auth.Authenticate(r.Context(), "basic", user, []byte(pass))
		if err != nil {
			return nil, unauthorized()
		}
		return princ, nil
	case strings.EqualFold(scheme, "Bearer"):
		princ, err := s.auth.Authenticate(r.Context(), "bearer", "", []byte(rest))
		if err != nil {
			return nil, unauthorized()
		}
		return princ, nil
	default:
		return nil, unauthorized()
	}
}

// unauthorized is the generic 401 for a bad credential, non-disclosing about why.
func unauthorized() *authFail {
	return &authFail{
		err:    apiError{Code: "Neo.ClientError.Security.Unauthorized", Message: "invalid authentication credentials"},
		scheme: "Basic realm=\"gr\"",
	}
}

// writeAuthError writes a 401 with the WWW-Authenticate header and the JSON error body.
func (s *server) writeAuthError(w http.ResponseWriter, f *authFail) {
	w.Header().Set("WWW-Authenticate", f.scheme)
	s.writeError(w, http.StatusUnauthorized, f.err)
}

// principalKey is the context key the authenticated principal is stored under.
type principalKey struct{}

// withPrincipal returns a context carrying the authenticated principal.
func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalFrom returns the principal in a context, or the anonymous principal when
// none is present.
func principalFrom(ctx context.Context) *Principal {
	if p, ok := ctx.Value(principalKey{}).(*Principal); ok && p != nil {
		return p
	}
	return anonymous
}
