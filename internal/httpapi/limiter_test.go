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

func TestKeyedLimiterCapsPerKey(t *testing.T) {
	l := newKeyedLimiter(2)
	for i := 0; i < 2; i++ {
		if !l.acquire("a") {
			t.Fatalf("slot %d for a key should succeed", i+1)
		}
	}
	if l.acquire("a") {
		t.Fatal("third slot for the same key must be refused")
	}
	if !l.acquire("b") {
		t.Fatal("another key must not be starved by key a")
	}
	l.release("a")
	if !l.acquire("a") {
		t.Fatal("slot should be reusable after release")
	}
	l.release("a")
	l.release("a")
	l.release("b")
	if len(l.counts) != 0 {
		t.Fatalf("counts not cleaned up: %v", l.counts)
	}
}
