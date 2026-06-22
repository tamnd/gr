package gr

import (
	"testing"
	"time"
)

func TestRateLimiterDisabled(t *testing.T) {
	if r := NewRateLimiter(0, 10); r != nil {
		t.Errorf("NewRateLimiter(0, 10) = %v, want nil (disabled)", r)
	}
	if r := NewRateLimiter(10, 0); r != nil {
		t.Errorf("NewRateLimiter(10, 0) = %v, want nil (disabled)", r)
	}
	var r *RateLimiter // nil limiter
	if ok, wait := r.Allow("k"); !ok || wait != 0 {
		t.Errorf("nil limiter Allow = (%v, %v), want (true, 0)", ok, wait)
	}
	if r.Throttled() != 0 {
		t.Errorf("nil limiter Throttled = %d, want 0", r.Throttled())
	}
}

func TestRateLimiterBurstThenThrottle(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	r := NewRateLimiter(1, 3) // 1 query/sec, burst of 3
	r.now = func() time.Time { return clock }

	// The first three queries spend the burst.
	for i := 0; i < 3; i++ {
		if ok, _ := r.Allow("user:alice"); !ok {
			t.Fatalf("query %d throttled, want allowed within burst", i)
		}
	}
	// The fourth is throttled, and the wait is about one second (the refill of one token).
	ok, wait := r.Allow("user:alice")
	if ok {
		t.Fatal("fourth query allowed, want throttled past the burst")
	}
	if wait <= 0 || wait > time.Second {
		t.Errorf("throttle wait = %v, want (0, 1s]", wait)
	}
	if r.Throttled() != 1 {
		t.Errorf("Throttled = %d, want 1", r.Throttled())
	}
}

func TestRateLimiterRefill(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	r := NewRateLimiter(2, 1) // 2 queries/sec, burst of 1
	r.now = func() time.Time { return clock }

	if ok, _ := r.Allow("k"); !ok {
		t.Fatal("first query throttled, want allowed")
	}
	if ok, _ := r.Allow("k"); ok {
		t.Fatal("second immediate query allowed, want throttled (burst 1)")
	}
	// Half a second refills one token at 2/sec.
	clock = clock.Add(500 * time.Millisecond)
	if ok, _ := r.Allow("k"); !ok {
		t.Fatal("query after refill throttled, want allowed")
	}
}

func TestRateLimiterPerKey(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	r := NewRateLimiter(1, 1) // burst of 1 per key
	r.now = func() time.Time { return clock }

	// Each key has its own bucket, so one busy key does not throttle another.
	if ok, _ := r.Allow("user:alice"); !ok {
		t.Fatal("alice first query throttled")
	}
	if ok, _ := r.Allow("user:bob"); !ok {
		t.Fatal("bob first query throttled by alice's spend")
	}
	// alice's second query is throttled, bob's bucket is untouched.
	if ok, _ := r.Allow("user:alice"); ok {
		t.Fatal("alice second query allowed, want throttled")
	}
}
