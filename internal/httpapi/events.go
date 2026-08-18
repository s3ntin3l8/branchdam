package httpapi

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleEvents streams a progress nudge over SSE: once on connect, then on
// every change (a scan batch committed, an edge resolved), with periodic
// heartbeats so proxies don't drop the idle connection. The payload is
// re-fetched from the database on each nudge, not carried on the hub's
// channel -- see internal/sse's package doc for why that's what keeps this
// cheap under a large scan.
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

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	notify, unsub := s.hub.Subscribe()
	defer unsub()

	send := func() {
		rows, err := s.db.Reader.ListRecentScanJobs(r.Context(), 5)
		if err != nil {
			return
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return
		}
		_, _ = w.Write([]byte("event: progress\ndata: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	send() // initial state

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
			// Shutdown would block for the entire shutdownBudget on every
			// routine restart (cmd/branchdam/main.go's runShutdownSequence),
			// misreporting a normal restart as a degraded one.
			return
		case <-notify:
			send()
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}
