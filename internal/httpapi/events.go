package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type ssePayloadCache struct {
	mu         sync.Mutex
	generation uint64
	payload    []byte
	hasValue   bool
}

// getSSEPayload returns the cached serialized recent scan jobs JSON payload,
// re-querying the database only when the SSE broadcast generation counter has
// changed or when the cache is uninitialized. This prevents N connected clients
// from issuing N redundant DB queries on the same broadcast tick (#365).
func (s *Server) getSSEPayload(ctx context.Context) ([]byte, error) {
	var currentGen uint64
	if s.hub != nil {
		currentGen = s.hub.Generation()
	}

	s.sseCache.mu.Lock()
	defer s.sseCache.mu.Unlock()

	if s.sseCache.hasValue && s.sseCache.generation == currentGen {
		return s.sseCache.payload, nil
	}

	if s.db == nil || s.db.Reader == nil {
		return nil, errors.New("database not available")
	}

	rows, err := s.db.Reader.ListRecentScanJobs(ctx, 5)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}

	s.sseCache.payload = b
	s.sseCache.generation = currentGen
	s.sseCache.hasValue = true
	return b, nil
}

// handleEvents streams a progress nudge over SSE: once on connect, then on
// every change (a scan batch committed, an edge resolved), with periodic
// heartbeats so proxies don't drop the idle connection. The payload is
// re-fetched from the database on each nudge, cached across connected clients
// on the same broadcast tick (#365).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.sseSlot.acquire() {
		http.Error(w, "too many streaming clients", http.StatusServiceUnavailable)
		return
	}
	defer s.sseSlot.release()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// http.Server.WriteTimeout (cmd/branchdam/main.go) applies a
	// per-connection deadline meant for ordinary request/response cycles --
	// applied unmodified to this handler, it tears the stream down at
	// WriteTimeoutSecs (15s by default), well before this handler's own 20s
	// heartbeat ever fires once. The client's EventSource reconnects, so
	// this was invisible as a hard failure, just a churn of reconnects and
	// a blind window each cycle where a nudge could be missed. Clearing the
	// write deadline opts this one long-lived handler out of it; s.shutdown
	// (below) and ctx.Done() are what actually bound its lifetime instead.
	// This is the load-bearing half of the fix.
	//
	// The read deadline is also cleared, but that's defense-in-depth, not
	// load-bearing for a GET handler with no request body: net/http starts
	// a background read on the connection before the handler even runs
	// (since the request has no body to keep reading) and that background
	// read already clears the read deadline itself, independent of
	// ReadTimeoutSecs -- verified against the stdlib's own
	// startBackgroundRead. Clearing it here again is harmless and documents
	// the intent rather than relying on that stdlib internal.
	//
	// Errors are deliberately ignored: SetWriteDeadline/SetReadDeadline
	// return an error only when the underlying connection doesn't support
	// deadlines at all (never true for a real net.Conn-backed
	// http.ResponseWriter, only possible in some test doubles), in which
	// case there is nothing to clear and nothing actionable to do about it.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	notify, unsub := s.hub.Subscribe()
	defer unsub()

	// errClientGone is a sentinel returned by send() when the underlying
	// http.ResponseWriter rejected a write. Genuine client-gone events
	// (browser tab closed, network partition) are the only thing that
	// warrants breaking the loop; the old code conflated this with
	// transient ListRecentScanJobs/marshal failures, terminating the
	// stream (and forcing EventSource to reconnect) on a momentary DB
	// hiccup that the next tick would have recovered from on its own.
	var errClientGone = errors.New("sse: client gone")

	send := func() error {
		b, err := s.getSSEPayload(r.Context())
		if err != nil {
			// Transient -- log and let the next tick retry.
			s.log.Warn("sse: get payload", "err", err)
			return nil
		}
		if _, err := w.Write([]byte("event: progress\ndata: ")); err != nil {
			return fmt.Errorf("%w: %v", errClientGone, err)
		}
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("%w: %v", errClientGone, err)
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return fmt.Errorf("%w: %v", errClientGone, err)
		}
		flusher.Flush()
		return nil
	}

	if err := send(); err != nil && errors.Is(err, errClientGone) {
		return
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			// http.Server.Shutdown does not cancel in-flight request
			// contexts -- it only waits for connections to go idle -- and
			// this handler's loop never returns on its own as long as the
			// client stays connected. Without this case, a single open
			// dashboard tab would keep ctx.Done() from ever firing, so
			// Shutdown would block for the entire shutdown timeout on every
			// routine restart (cmd/branchdam/main.go).
			return
		case <-notify:
			if err := send(); err != nil && errors.Is(err, errClientGone) {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
