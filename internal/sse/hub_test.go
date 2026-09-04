package sse

import "testing"

func TestHubSubscribeBroadcastUnsubscribe(t *testing.T) {
	h := New()
	if h.Count() != 0 {
		t.Fatalf("new hub Count = %d, want 0", h.Count())
	}

	notify, unsub := h.Subscribe()
	if h.Count() != 1 {
		t.Fatalf("after Subscribe Count = %d, want 1", h.Count())
	}

	h.Broadcast()
	select {
	case <-notify:
	default:
		t.Fatal("expected a nudge after Broadcast")
	}

	unsub()
	if h.Count() != 0 {
		t.Fatalf("after unsub Count = %d, want 0", h.Count())
	}
}

// Broadcast must never block even when a subscriber hasn't drained its channel
// (the nudge coalesces).
func TestHubBroadcastCoalesces(t *testing.T) {
	h := New()
	_, unsub := h.Subscribe()
	defer unsub()
	for i := 0; i < 5; i++ {
		h.Broadcast() // would deadlock/panic if it blocked on the full buffer
	}
}

// Unsubscribing twice must not panic (double close of the channel).
func TestHubDoubleUnsubscribe(t *testing.T) {
	h := New()
	_, unsub := h.Subscribe()
	unsub()
	unsub()
}

func TestHubGeneration(t *testing.T) {
	h := New()
	if gen := h.Generation(); gen != 0 {
		t.Fatalf("initial Generation = %d, want 0", gen)
	}
	h.Broadcast()
	if gen := h.Generation(); gen != 1 {
		t.Fatalf("after 1 Broadcast Generation = %d, want 1", gen)
	}
	h.Broadcast()
	if gen := h.Generation(); gen != 2 {
		t.Fatalf("after 2 Broadcasts Generation = %d, want 2", gen)
	}
}
