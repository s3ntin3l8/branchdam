package httpapi

import "sync"

// maxSSEClients bounds concurrent Server-Sent-Events connections. Each one
// holds a goroutine and a ticker; without a cap a flood of connections
// could exhaust server resources.
const maxSSEClients = 256

// limiter is a fixed-capacity, non-blocking slot pool.
type limiter struct{ ch chan struct{} }

func newLimiter(n int) *limiter { return &limiter{ch: make(chan struct{}, n)} }

// acquire takes a slot, returning false immediately if none are free.
func (l *limiter) acquire() bool {
	select {
	case l.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a slot to the pool.
func (l *limiter) release() {
	select {
	case <-l.ch:
	default:
	}
}

// maxSSEClientsPerPrincipal keeps one account (or one anonymous source IP)
// from taking every global SSE slot: with only the global cap, a single
// logged-in user opening 256 tabs/connections starves every other user.
const maxSSEClientsPerPrincipal = 8

// keyedLimiter is a non-blocking per-key slot counter.
type keyedLimiter struct {
	max    int
	mu     sync.Mutex
	counts map[string]int
}

func newKeyedLimiter(max int) *keyedLimiter {
	return &keyedLimiter{max: max, counts: map[string]int{}}
}

func (l *keyedLimiter) acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= l.max {
		return false
	}
	l.counts[key]++
	return true
}

func (l *keyedLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] <= 1 {
		delete(l.counts, key)
		return
	}
	l.counts[key]--
}
