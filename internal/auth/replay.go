package auth

import (
	"sync"
	"time"
)

// ReplayCache tracks nonces seen within the active replay window in memory.
// It is safe for concurrent use by multiple goroutines.
type ReplayCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // nonce -> expiresAt
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

	// Sweep expired nonces to keep memory bounded
	for k, exp := range c.entries {
		if !now.Before(exp) {
			delete(c.entries, k)
		}
	}

	if _, exists := c.entries[nonce]; exists {
		return false
	}
	c.entries[nonce] = expiresAt
	return true
}
