// Package sse implements a minimal Server-Sent Events hub: clients
// subscribe and get a nudge whenever something changes. The actual payload
// (scan/progress state) is fetched by the handler from the database, not
// carried on the channel -- this hub only ever says "something changed, go
// re-fetch," which is what lets 10k files hashed in one scan produce at
// most one nudge per client per read cycle instead of one per file.
//
// Copied from traefik-viewer/internal/sse/hub.go, same design, same shape.
package sse

import "sync"

// Hub tracks connected clients and fans out change notifications.
type Hub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

// New creates an empty Hub.
func New() *Hub {
	return &Hub{clients: map[chan struct{}]struct{}{}}
}

// Subscribe registers a client and returns its notification channel + an
// unsubscribe func. The channel is buffered and coalescing.
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Broadcast nudges every connected client (non-blocking; coalesces).
func (h *Hub) Broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default: // a nudge is already pending
		}
	}
}

// Count returns the number of connected clients (for diagnostics/tests).
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
