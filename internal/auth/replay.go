package auth

import (
	"sync"
	"time"
)

// replaySweepInterval bounds how often CheckAndRecord walks the entry map
// to delete expired nonces. Without this, every accepted request did an
// O(N) sweep while holding the cache mutex; at even modest request rates
// (hundreds/s with the default 5-minute window) the sweep becomes the
// dominant cost in the auth path. Once per second is a comfortable
// tradeoff: a worst-case in-flight set of nonces is bounded by
// rate * window regardless, and the sweep itself is cheap against a
// map[string]time.Time.
const replaySweepInterval = time.Second

// ReplayCache tracks nonces seen within the active replay window in memory.
// It is safe for concurrent use by multiple goroutines. The cache is
// per-process; a server restart resets the nonce store, which is a
// documented limitation of the in-memory design (config.example.yaml
// notes it alongside agent.replayWindowSecs).
type ReplayCache struct {
	mu        sync.Mutex
	entries   map[string]time.Time // nonce -> expiresAt
	lastSweep time.Time            // last time the entry map was pruned
}

// NewReplayCache creates an initialized in-memory ReplayCache.
func NewReplayCache() *ReplayCache {
	return &ReplayCache{
		entries: make(map[string]time.Time),
	}
}

// CheckAndRecord checks whether nonce has already been observed within its validity window.
// If it has not been seen, it records nonce with expiresAt and returns true (accepted).
// If it was already recorded and not yet expired, it returns false (rejected as duplicate/replay).
func (c *ReplayCache) CheckAndRecord(nonce string, expiresAt time.Time, now time.Time) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Rate-limit the prune pass: a full sweep on every accepted request is
	// O(N) and serializes all agent traffic behind the cache mutex.
	// Between sweeps, expired entries linger harmlessly -- they're skipped
	// on the duplicate check via the expiresAt comparison below.
	if c.lastSweep.IsZero() || now.Sub(c.lastSweep) >= replaySweepInterval {
		for k, exp := range c.entries {
			if !now.Before(exp) {
				delete(c.entries, k)
			}
		}
		c.lastSweep = now
	}

	if existing, exists := c.entries[nonce]; exists && now.Before(existing) {
		return false
	}
	c.entries[nonce] = expiresAt
	return true
}
