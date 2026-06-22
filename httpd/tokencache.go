package httpd

import (
	"crypto/sha256"
	"sync"
	"time"
)

// defaultTokenCacheTTL bounds how long a validated bearer token is cached before it is
// revalidated (doc 18 §10.4, doc 24 auth.token_cache_ttl). Longer cuts the signature-
// verification cost for a high-rate client, shorter tightens how fast a revoked token
// stops working. Five minutes is a middle default; a deployment tunes it through Options.
const defaultTokenCacheTTL = 5 * time.Minute

// tokenCache memoizes a successful bearer validation so a high-rate client with a JWT does
// not re-verify the signature on every request (doc 18 §10.4). An entry lives until the
// sooner of the cache TTL and the token's own expiry, so a cached principal never outlives
// the token it was minted from. The cache is keyed by a hash of the token, not the token
// itself, so a memory dump does not hand out reusable bearer tokens (the secret discipline,
// doc 24 §21.4).
type tokenCache struct {
	mu  sync.Mutex
	m   map[string]tokenEntry
	ttl time.Duration
	now func() time.Time
}

// tokenEntry is one cached validation: the principal and when the entry expires.
type tokenEntry struct {
	princ   *Principal
	expires time.Time
}

// newTokenCache returns an empty token cache with the given TTL and clock.
func newTokenCache(ttl time.Duration, now func() time.Time) *tokenCache {
	return &tokenCache{m: make(map[string]tokenEntry), ttl: ttl, now: now}
}

// get returns the cached principal for a token when one is present and unexpired. An
// expired entry is dropped on access, so a stale validation never serves a request.
func (c *tokenCache) get(token string) (*Principal, bool) {
	key := hashToken(token)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.m, key)
		return nil, false
	}
	return e.princ, true
}

// put caches a validated principal for a token. The entry expires at the sooner of now+TTL
// and the token's own expiry, so a short-lived token is not cached past its life even when
// the TTL is long.
func (c *tokenCache) put(token string, p *Principal) {
	expires := c.now().Add(c.ttl)
	if p.Token != nil && !p.Token.ExpiresAt.IsZero() && p.Token.ExpiresAt.Before(expires) {
		expires = p.Token.ExpiresAt
	}
	key := hashToken(token)
	c.mu.Lock()
	c.m[key] = tokenEntry{princ: p, expires: expires}
	c.mu.Unlock()
}

// hashToken hashes a token to its cache key, so the raw token is not held in the map.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return string(sum[:])
}
