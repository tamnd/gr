package httpd

import (
	"sync"
	"time"
)

// lockout is the failed-attempt lockout state shared by the credential providers (doc
// 18 §10.3): a per-principal consecutive-failure count that locks the principal for a
// window once it crosses a threshold. It is process state, not persisted, so a restart
// clears every lockout. Both the in-memory StaticProvider and the database-backed
// DBProvider embed it, so the brute-force protection is identical whichever store backs
// the credentials.
type lockout struct {
	// mu guards the attempt map, separate from a provider's credential lock so a flood
	// of failed attempts does not contend with credential reads.
	mu       sync.Mutex
	attempts map[string]*attempt
	maxFail  int           // failures before lockout; 0 disables lockout
	dur      time.Duration // how long a locked principal stays locked
	// now is the clock, overridable in tests so lockout expiry is driven without sleeping.
	now func() time.Time
}

// attempt is a principal's lockout state (doc 18 §10.3): the consecutive-failure count
// and, once locked, the time the lock lifts.
type attempt struct {
	failed int
	until  time.Time
}

// newLockout returns lockout state with the default policy (doc 18 §10.3, doc 24): a
// principal is locked after five failures for a minute.
func newLockout() lockout {
	return lockout{
		attempts: make(map[string]*attempt),
		maxFail:  defaultMaxFailedAttempts,
		dur:      defaultLockoutDuration,
		now:      time.Now,
	}
}

// setPolicy sets the lockout policy: a principal is locked after maxFailed consecutive
// failures for dur, and a maxFailed of 0 disables lockout.
func (l *lockout) setPolicy(maxFailed int, dur time.Duration) {
	l.mu.Lock()
	l.maxFail = maxFailed
	l.dur = dur
	l.mu.Unlock()
}

// locked reports whether a principal is currently within its lockout window (doc 18
// §10.3). With lockout disabled (maxFail == 0) it is always false.
func (l *lockout) locked(principal string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxFail <= 0 {
		return false
	}
	a, ok := l.attempts[principal]
	if !ok || a.until.IsZero() {
		return false
	}
	return l.now().Before(a.until)
}

// recordFailure counts a failed attempt and locks the principal once it reaches the
// threshold (doc 18 §10.3). When a previous lock has already expired the counter
// restarts, so a principal gets a fresh window of attempts after a lockout lifts rather
// than being re-locked by a single late failure.
func (l *lockout) recordFailure(principal string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxFail <= 0 {
		return
	}
	a := l.attempts[principal]
	if a == nil {
		a = &attempt{}
		l.attempts[principal] = a
	}
	if !a.until.IsZero() && !l.now().Before(a.until) {
		a.failed = 0
		a.until = time.Time{}
	}
	a.failed++
	if a.failed >= l.maxFail {
		a.until = l.now().Add(l.dur)
	}
}

// recordSuccess clears a principal's lockout state after a successful authentication.
func (l *lockout) recordSuccess(principal string) {
	l.mu.Lock()
	delete(l.attempts, principal)
	l.mu.Unlock()
}
