// Package ratelimit owns the per-IP sliding-window rate limiter used by
// /api/v1/login and /api/v1/setup/admin. State is in-memory; a process
// restart resets the counters (acceptable for v1, documented in PLAN.md
// §11). The limiter is deliberately per-IP rather than per-account to
// avoid leaking which usernames exist via lockout timing.
//
// Two windows with two thresholds:
//   - "fast" window: N failures within FastWindow triggers a short
//     cool-off (CoolOffFast).
//   - "slow" window: a higher N within SlowWindow triggers a longer
//     cool-off (CoolOffSlow).
//
// A successful login clears the IP's failure history immediately.
package ratelimit

import (
	"sync"
	"time"
)

// Config bundles the rate-limit thresholds.
type Config struct {
	MaxFailuresFast int
	FastWindow      time.Duration
	CoolOffFast     time.Duration

	MaxFailuresSlow int
	SlowWindow      time.Duration
	CoolOffSlow     time.Duration

	Now func() time.Time
}

// Limiter is a per-IP sliding-window failure counter.
type Limiter struct {
	mu              sync.Mutex
	ips             map[string]*ipState
	maxFailuresFast int
	fastWindow      time.Duration
	coolOffFast     time.Duration
	maxFailuresSlow int
	slowWindow      time.Duration
	coolOffSlow     time.Duration
	now             func() time.Time
	lastSweep       time.Time
}

const sweepInterval = 30 * time.Second

type ipState struct {
	failures     []time.Time
	coolOffUntil time.Time
	coolOffKind  string
}

// New returns a Limiter with the package-recommended defaults.
func New() *Limiter {
	return NewFromConfig(Config{})
}

// NewFromConfig returns a Limiter with the supplied thresholds.
func NewFromConfig(cfg Config) *Limiter {
	l := &Limiter{
		ips: make(map[string]*ipState),
		now: cfg.Now,
	}
	if l.now == nil {
		l.now = time.Now
	}
	l.maxFailuresFast = cfg.MaxFailuresFast
	if l.maxFailuresFast <= 0 {
		l.maxFailuresFast = 5
	}
	l.fastWindow = cfg.FastWindow
	if l.fastWindow <= 0 {
		l.fastWindow = 5 * time.Minute
	}
	l.coolOffFast = cfg.CoolOffFast
	if l.coolOffFast <= 0 {
		l.coolOffFast = 60 * time.Second
	}
	l.maxFailuresSlow = cfg.MaxFailuresSlow
	if l.maxFailuresSlow <= 0 {
		l.maxFailuresSlow = 10
	}
	l.slowWindow = cfg.SlowWindow
	if l.slowWindow <= 0 || l.slowWindow < l.fastWindow {
		l.slowWindow = l.fastWindow
	}
	l.coolOffSlow = cfg.CoolOffSlow
	if l.coolOffSlow <= 0 {
		l.coolOffSlow = 5 * time.Minute
	}
	return l
}

// Decision is the result of a Check call.
type Decision struct {
	Allowed     bool
	RetryAfter  time.Duration
	CoolOffKind string
}

// Check returns the limiter's decision for ip WITHOUT recording a failure.
func (l *Limiter) Check(ip string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.maybeSweep()

	state, ok := l.ips[ip]
	if !ok {
		return Decision{Allowed: true}
	}
	now := l.now()
	if now.Before(state.coolOffUntil) {
		return Decision{
			Allowed:     false,
			RetryAfter:  state.coolOffUntil.Sub(now),
			CoolOffKind: coolOffKindOf(state),
		}
	}
	return Decision{Allowed: true}
}

// RecordFailure marks ip as having failed once.
func (l *Limiter) RecordFailure(ip string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.maybeSweep()

	now := l.now()
	state, ok := l.ips[ip]
	if !ok {
		state = &ipState{}
		l.ips[ip] = state
	}

	state.failures = append(state.failures, now)
	state.failures = evictOlderThan(state.failures, now, l.slowWindow)

	if len(state.failures) >= l.maxFailuresSlow {
		state.coolOffUntil = now.Add(l.coolOffSlow)
		state.coolOffKind = "slow"
		return Decision{
			Allowed:     false,
			RetryAfter:  l.coolOffSlow,
			CoolOffKind: "slow",
		}
	}
	if countWithin(state.failures, now, l.fastWindow) >= l.maxFailuresFast {
		state.coolOffUntil = now.Add(l.coolOffFast)
		state.coolOffKind = "fast"
		return Decision{
			Allowed:     false,
			RetryAfter:  l.coolOffFast,
			CoolOffKind: "fast",
		}
	}
	return Decision{Allowed: true}
}

// RecordSuccess clears ip's failure history.
func (l *Limiter) RecordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

func (l *Limiter) maybeSweep() {
	now := l.now()
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for ip, state := range l.ips {
		if now.Before(state.coolOffUntil) {
			continue
		}
		state.failures = evictOlderThan(state.failures, now, l.slowWindow)
		if len(state.failures) == 0 {
			delete(l.ips, ip)
		}
	}
}

func evictOlderThan(failures []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	out := failures[:0:0]
	for _, t := range failures {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func countWithin(failures []time.Time, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, t := range failures {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

func coolOffKindOf(state *ipState) string {
	if state.coolOffUntil.IsZero() {
		return ""
	}
	return state.coolOffKind
}
