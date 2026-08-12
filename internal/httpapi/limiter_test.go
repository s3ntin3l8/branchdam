package httpapi

import "testing"

// The SSE limiter must hand out at most N slots and reclaim them on
// release, so a flood of long-lived connections can't exhaust goroutines.
func TestLimiter(t *testing.T) {
	l := newLimiter(2)
	if !l.acquire() {
		t.Fatal("first acquire should succeed")
	}
	if !l.acquire() {
		t.Fatal("second acquire should succeed")
	}
	if l.acquire() {
		t.Fatal("third acquire should fail at capacity")
	}
	l.release()
	if !l.acquire() {
		t.Fatal("acquire after release should succeed")
	}
}
