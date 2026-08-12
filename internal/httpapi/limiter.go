package httpapi

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
