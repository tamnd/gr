package httpd

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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

// Principal is an authenticated identity and its roles (doc 18 §10.4, §10.6). The roles
// drive the authorization model (doc 18 §10.6) and the transaction store scopes ownership
// to the name (doc 18 §9.9). Token carries the validated claims for a bearer/JWT principal
// and is nil for a basic credential, which has no token. ImpersonatedBy names the
// authenticating principal when this one is an impersonation target (doc 18 §10.5): the
// principal runs with the impersonated user's Name and Roles, but the audit trail records
// who is acting as whom, so ImpersonatedBy holds the actor and Name the impersonated user.
// It is empty for an ordinary, non-impersonated principal.
type Principal struct {
	Name           string
	Roles          []string
	Token          *Claims
	ImpersonatedBy string
}

// RoleResolver is the optional seam a provider implements to support impersonation (doc
// 18 §10.5). Resolve returns the principal for a user by name, with its roles, without a
// credential check, so an admin may run a query as that user. A provider that does not
// implement it cannot be a target of impersonation, so impersonation is refused when the
// configured provider is not a RoleResolver. Resolve returns ErrNoSuchPrincipal when the
// named user does not exist.
type RoleResolver interface {
	Resolve(ctx context.Context, name string) (*Principal, error)
}

// ErrNoSuchPrincipal is returned by a RoleResolver when the named user does not exist
// (doc 18 §10.5). The impersonation check maps it to a forbidden response, so a probe
// cannot tell a missing impersonation target from one the actor may not assume.
var ErrNoSuchPrincipal = errors.New("gr: no such principal")

// Claims is the validated content of a bearer/JWT token (doc 18 §10.4). It is what a
// JWTProvider returns on the Principal so a downstream caller (audit, the token cache)
// can read the subject, the issuer, and the expiry without re-parsing the token. Raw
// holds every claim as decoded, so a deployment reads a custom claim the typed fields do
// not name.
type Claims struct {
	Subject   string
	Issuer    string
	Audience  []string
	ExpiresAt time.Time
	NotBefore time.Time
	IssuedAt  time.Time
	Roles     []string
	Raw       map[string]any
}

// ErrUnauthorized is the credential-check failure, mapped to 401 (doc 18 §12). The
// provider returns it for a bad username, a bad password, or an unsupported scheme, so
// the response never reveals which, the standard non-disclosing auth failure.
var ErrUnauthorized = errors.New("gr: unauthorized")

// ErrLockedOut is returned during a principal's lockout window (doc 18 §10.3). It wraps
// ErrUnauthorized, so a caller that only cares whether authentication failed treats it
// the same and the client sees the same non-disclosing 401, while a caller that records
// metrics can tell a lockout apart from an ordinary failure to raise the brute-force
// signal (the auth_total{outcome="lockout"} counter).
var ErrLockedOut = fmt.Errorf("gr: account locked: %w", ErrUnauthorized)

// ErrTokenExpired is returned when a bearer token's expiry has passed (doc 18 §10.4, §12).
// It wraps ErrUnauthorized so a generic caller treats it as a failure, but the HTTP layer
// maps it to the distinct Neo.ClientError.Security.TokenExpired code so a client can tell
// an expired token (refresh and retry) apart from a bad one (re-authenticate).
var ErrTokenExpired = fmt.Errorf("gr: token expired: %w", ErrUnauthorized)

// Default lockout policy (doc 24): five failures locks a principal for a minute. Zero
// max disables lockout. These are the spec's defaults; a deployment tunes them through
// the configuration model (doc 24), or in code with SetLockout.
const (
	defaultMaxFailedAttempts = 5
	defaultLockoutDuration   = 60 * time.Second
)

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
	lockout
	mu    sync.RWMutex
	users map[string]storedUser
	// dummy is a throwaway hash compared against when the named user does not exist,
	// so a missing user takes the same time as a wrong password and the lookup does
	// not leak which usernames exist through timing.
	dummy []byte
}

// NewStaticProvider returns an empty credential store with the default lockout policy
// (doc 18 §10.3, doc 24): a principal is locked after five failures for a minute.
func NewStaticProvider() *StaticProvider {
	dummy, _ := derive("", make([]byte, 16))
	return &StaticProvider{
		lockout: newLockout(),
		users:   make(map[string]storedUser),
		dummy:   dummy,
	}
}

// SetLockout sets the failed-attempt lockout policy (doc 18 §10.3): a principal is locked
// after maxFailed consecutive failures for dur, and a maxFailed of 0 disables lockout. It
// returns the provider so a caller can chain it after NewStaticProvider.
func (p *StaticProvider) SetLockout(maxFailed int, dur time.Duration) *StaticProvider {
	p.setPolicy(maxFailed, dur)
	return p
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
	// A locked principal is refused without consulting the hash (doc 18 §10.3), even
	// with a correct password, so a flood of guesses cannot succeed during the lockout.
	// A dummy derive is still run so a locked attempt takes the same wall-clock as a
	// normal failure and the lockout does not leak through timing.
	if p.locked(principal) {
		_, _ = derive(string(credential), make([]byte, 16))
		return nil, ErrLockedOut
	}
	p.mu.RLock()
	u, ok := p.users[principal]
	p.mu.RUnlock()
	if !ok {
		// Compare against the dummy hash so a missing user is indistinguishable
		// from a wrong password by timing, then fail.
		_, _ = derive(string(credential), make([]byte, 16))
		subtle.ConstantTimeCompare(p.dummy, p.dummy)
		p.recordFailure(principal)
		return nil, ErrUnauthorized
	}
	got, err := derive(string(credential), u.salt)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(got, u.hash) != 1 {
		p.recordFailure(principal)
		return nil, ErrUnauthorized
	}
	// A success resets the principal's failed-attempt counter (doc 18 §10.3).
	p.recordSuccess(principal)
	return &Principal{Name: principal, Roles: append([]string(nil), u.roles...)}, nil
}

// Schemes reports that the built-in store verifies only the basic scheme.
func (p *StaticProvider) Schemes() []string { return []string{"basic"} }

// Resolve returns the principal for a user by name, without a credential check, so an
// admin may impersonate it (doc 18 §10.5). It returns ErrNoSuchPrincipal for a name the
// store does not hold.
func (p *StaticProvider) Resolve(ctx context.Context, name string) (*Principal, error) {
	p.mu.RLock()
	u, ok := p.users[name]
	p.mu.RUnlock()
	if !ok {
		return nil, ErrNoSuchPrincipal
	}
	return &Principal{Name: name, Roles: append([]string(nil), u.roles...)}, nil
}

// derive computes the PBKDF2-HMAC-SHA256 hash of a password with a salt.
func derive(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, 32)
}

// authFail describes a rejected request: the apiError to return and the scheme to put
// in the WWW-Authenticate header. The status is always 401 for an auth failure. lockout
// records that the failure was a lockout rather than a bad credential, so the metrics can
// raise the brute-force signal; the client sees the same non-disclosing 401 either way.
type authFail struct {
	err     apiError
	scheme  string
	lockout bool
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
		return nil, unauthorized(nil)
	}
	switch {
	case strings.EqualFold(scheme, "Basic"):
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil, unauthorized(nil)
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return nil, unauthorized(nil)
		}
		princ, err := s.auth.Authenticate(r.Context(), "basic", user, []byte(pass))
		if err != nil {
			return nil, unauthorized(err)
		}
		return princ, nil
	case strings.EqualFold(scheme, "Bearer"):
		// A validated token is cached for its lifetime (bounded by the cache TTL) so a
		// high-rate client with a JWT does not re-verify the signature on every request
		// (doc 18 §10.4). A cache miss validates through the provider and caches the result.
		if s.bearerCache != nil {
			if princ, ok := s.bearerCache.get(rest); ok {
				return princ, nil
			}
		}
		princ, err := s.auth.Authenticate(r.Context(), "bearer", "", []byte(rest))
		if err != nil {
			return nil, unauthorized(err)
		}
		if s.bearerCache != nil {
			s.bearerCache.put(rest, princ)
		}
		return princ, nil
	default:
		return nil, unauthorized(nil)
	}
}

// unauthorized is the generic 401 for a bad credential, non-disclosing about why. An
// expired token is the one exception: it maps to the distinct TokenExpired code with a
// Bearer challenge so a client knows to refresh rather than re-authenticate (doc 18 §12).
// It flags a lockout (from the underlying error) so the metrics can record it; the body
// is otherwise the same regardless.
func unauthorized(err error) *authFail {
	if errors.Is(err, ErrTokenExpired) {
		return &authFail{
			err:    apiError{Code: "Neo.ClientError.Security.TokenExpired", Message: "authentication token expired"},
			scheme: "Bearer realm=\"gr\", error=\"invalid_token\", error_description=\"token expired\"",
		}
	}
	return &authFail{
		err:     apiError{Code: "Neo.ClientError.Security.Unauthorized", Message: "invalid authentication credentials"},
		scheme:  "Basic realm=\"gr\"",
		lockout: errors.Is(err, ErrLockedOut),
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
