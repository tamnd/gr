package gr

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// ErrConflict marks a write transaction that lost a write-write or serialization
// race and must be retried against a fresh snapshot (doc 16 §6.4, doc 06 §6.2). It
// is the cause a retryable commit failure wraps, matched with errors.Is.
//
// On the default single-writer path it never occurs: write transactions serialize
// behind the write lock, so a single writer has no one to conflict with (doc 06
// §5.1). It becomes load-bearing only when a deployment opts into concurrent
// writers (doc 16 §17.3), at which point Retry turns a transient first-committer-
// wins abort into eventual success for independent writes.
var ErrConflict = errors.New("gr: transaction conflict, retry")

// DefaultMaxRetries is the retry bound Update uses when Options.MaxRetries is zero.
// It is a small bound: a contended write that cannot succeed within a handful of
// attempts is better surfaced than retried forever (doc 16 §6.4, §25).
const DefaultMaxRetries = 5

const (
	// baseBackoff is the first inter-attempt delay; later delays grow from it.
	baseBackoff = time.Millisecond
	// maxBackoff caps the exponential growth so the delay stays bounded.
	maxBackoff = 100 * time.Millisecond
)

// IsRetryable reports whether err is a transient failure that retrying against a
// fresh transaction can resolve (doc 16 §15.5). A conflict is retryable; a
// constraint violation, a syntax error, or a cancellation is not, because re-running
// the same statement cannot make any of them succeed.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrConflict)
}

// Retry runs fn, and on a retryable error (IsRetryable) re-runs it up to a total of
// attempts invocations, with a bounded exponential backoff plus jitter between
// tries so a contended write does not hot-loop (doc 16 §6.4). It returns nil on the
// first success, the last error if every attempt fails, and a non-retryable error
// immediately without retrying. A cancelled ctx stops the loop and returns the
// context error.
//
// fn must be re-runnable: Retry may invoke it more than once, so it must compute the
// same writes from the same inputs each time and hold no side effect outside the
// transaction that a re-run would double (doc 16 §6.2).
func Retry(ctx context.Context, attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err = fn(); err == nil {
			return nil
		}
		if !IsRetryable(err) {
			return err
		}
		if attempt < attempts-1 {
			if werr := sleepBackoff(ctx, attempt); werr != nil {
				return werr
			}
		}
	}
	return err
}

// sleepBackoff waits the backoff delay for the given attempt, returning early with
// the context error if ctx is cancelled while waiting.
func sleepBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(backoffDelay(attempt))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffDelay is a bounded exponential delay with jitter: the base delay doubled
// per attempt and capped at maxBackoff, then randomized into the lower half of that
// window so concurrent retriers spread out rather than waking together.
func backoffDelay(attempt int) time.Duration {
	d := maxBackoff
	if attempt < 16 {
		if shifted := baseBackoff << uint(attempt); shifted < maxBackoff {
			d = shifted
		}
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}
