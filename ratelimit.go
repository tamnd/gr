package gr

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ErrRateLimited is returned by RateLimiter.Allow's caller path when a key has spent its
// query budget (doc 18 §8.8). Like the in-flight shed it is a retryable transient: the
// HTTP surface maps it to 429 Too Many Requests with a Retry-After header and a transient
// status code, and the Bolt surface to a transient StatusError, so a driver backs off and
// retries rather than hammering a saturated server.
var ErrRateLimited = errors.New("gr: query rate limit exceeded")

// RateLimiter bounds the query rate per key, where a key is a principal (a per-token
// limit) or a connection source (a per-connection limit), so one client cannot monopolize
// the engine (doc 18 §8.8). It is a token bucket per key: each key starts with burst
// tokens that refill at rate tokens per second, a query spends one token, and a key with
// no token is throttled and told how long to wait for the next one. Both server surfaces
// share one limiter, so the bound holds across the whole process rather than per transport.
//
// A nil *RateLimiter is a disabled limiter: it allows every query, the embedded-friendly
// default when no limit is configured.
type RateLimiter struct {
	rate  float64 // tokens added per second
	burst float64 // bucket capacity, the largest momentary burst a key may make
	// now is the clock, overridable in tests so refill is driven without sleeping.
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	throttled atomic.Int64
}

// tokenBucket is one key's budget: the tokens left and when they were last refilled. A
// bucket at full capacity is indistinguishable from a fresh one, so a full bucket is
// evicted on access to keep the map from growing without bound as keys come and go.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a rate limiter that allows qps queries per second per key with a
// momentary burst of burst (doc 18 §8.8). A qps of zero or less, or a burst of zero or
// less, returns nil, a disabled limiter, so an unconfigured server is unlimited.
func NewRateLimiter(qps float64, burst int) *RateLimiter {
	if qps <= 0 || burst <= 0 {
		return nil
	}
	return &RateLimiter{
		rate:    qps,
		burst:   float64(burst),
		now:     time.Now,
		buckets: make(map[string]*tokenBucket),
	}
}

// Allow charges one query against key's budget (doc 18 §8.8). It returns true when a token
// was available and the query may proceed; when the key is out of tokens it returns false
// and the duration until the next token is available, the Retry-After hint the caller
// passes back to the client. A nil limiter allows every query.
func (r *RateLimiter) Allow(key string) (bool, time.Duration) {
	if r == nil {
		return true, 0
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		// A fresh key starts with a full bucket and spends one token at once.
		r.buckets[key] = &tokenBucket{tokens: r.burst - 1, last: now}
		return true, 0
	}

	// Refill for the time elapsed since the last charge, capped at the bucket capacity.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(r.burst, b.tokens+elapsed*r.rate)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		// A bucket back at full capacity carries no state a fresh one would not, so drop
		// it to bound the map; the next request for this key recreates it.
		if b.tokens >= r.burst {
			delete(r.buckets, key)
		}
		return true, 0
	}

	r.throttled.Add(1)
	// Time until one whole token has refilled.
	wait := time.Duration((1 - b.tokens) / r.rate * float64(time.Second))
	return false, wait
}

// Throttled reports how many queries the limiter has refused since it was created (doc 18
// §13.5), the signal an operator reads to decide whether a client is being too aggressive
// or the limit is too tight. A nil limiter reports zero.
func (r *RateLimiter) Throttled() int64 {
	if r == nil {
		return 0
	}
	return r.throttled.Load()
}
