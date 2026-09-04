package auth

import (
	"testing"
	"time"
)

// TestReplayCacheAcceptsNewNonce pins the basic happy-path contract: a
// fresh nonce is accepted and a re-presentation of the same nonce within
// its validity window is rejected.
func TestReplayCacheAcceptsNewNonce(t *testing.T) {
	cache := NewReplayCache()
	now := time.Unix(1_700_000_000, 0)
	expiresAt := now.Add(5 * time.Minute)

	if !cache.CheckAndRecord("nonce-1", expiresAt, now) {
		t.Error("first CheckAndRecord returned false, want true")
	}
	if cache.CheckAndRecord("nonce-1", expiresAt, now) {
		t.Error("second CheckAndRecord with same nonce returned true, want false (replayed)")
	}
}

// TestReplayCacheRejectsExpiredNonceAsDuplicateAfterExpiry ensures the
// nonce-replay check uses the entry's expiresAt, not just map presence --
// an expired nonce must be replaceable with a fresh record at the same
// key. The old behavior (delete-on-sweep first, then presence check) had
// the same end result but only when the periodic sweep happened to fire;
// this test guards the corner case where a request arrives just after the
// entry expired but before the next sweep pass.
func TestReplayCacheRejectsExpiredNonceAsDuplicateAfterExpiry(t *testing.T) {
	cache := NewReplayCache()
	t0 := time.Unix(1_700_000_000, 0)
	window := 5 * time.Minute

	if !cache.CheckAndRecord("nonce-1", t0.Add(window), t0) {
		t.Fatal("initial CheckAndRecord returned false")
	}

	// One nanosecond past the entry's expiresAt, but still within the
	// sweep interval (so the prune pass does NOT run again). The
	// expired entry must be replaced, not rejected as a replay.
	t1 := t0.Add(window + time.Nanosecond)
	if !cache.CheckAndRecord("nonce-1", t1.Add(window), t1) {
		t.Error("CheckAndRecord after expiry returned false, want true (expired nonces must be replaceable)")
	}
}

// TestReplayCacheSweepRateLimited verifies the auth path no longer pays
// O(N) per request: filling the cache with expired entries and then
// issuing many requests within one sweep interval must not call the
// prune pass on every call.
func TestReplayCacheSweepRateLimited(t *testing.T) {
	cache := NewReplayCache()
	t0 := time.Unix(1_700_000_000, 0)
	window := 5 * time.Minute
	expiredAt := t0.Add(-time.Second) // already expired at t0

	// Fill with N already-expired entries; the first CheckAndRecord at t0
	// sweeps them all (lastSweep transitions from zero -> t0), so we use
	// that call to populate, then count the work on subsequent calls.
	for i := 0; i < 1000; i++ {
		cache.entries[nonceKey(i)] = expiredAt
	}
	cache.entries["fresh-nonce"] = t0.Add(window)
	if !cache.CheckAndRecord("first-call", t0.Add(window), t0) {
		t.Fatal("first CheckAndRecord returned false")
	}
	if got := len(cache.entries); got != 2 {
		t.Fatalf("after first sweep: len(entries) = %d, want 2 (1000 expired + 1 fresh + the new entry)", got)
	}

	// 1000 more requests at t0 + 10ms (well inside replaySweepInterval).
	// None of them should trigger another sweep -- lastSweep == t0.
	for i := 0; i < 1000; i++ {
		cache.CheckAndRecord(nonceKey(1_000_000+i), t0.Add(window), t0.Add(10*time.Millisecond))
	}

	// A second sweep should only run after replaySweepInterval has elapsed.
	if !cache.lastSweep.Equal(t0) {
		t.Errorf("lastSweep = %v, want %v (no sweep should have run within the rate-limit interval)", cache.lastSweep, t0)
	}
}

func nonceKey(i int) string {
	const alphabet = "0123456789abcdef"
	buf := make([]byte, 32)
	for j := 0; j < 32; j++ {
		buf[j] = alphabet[(i>>(j%16))&0xf]
	}
	return string(buf)
}
