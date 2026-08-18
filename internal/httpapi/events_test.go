package httpapi

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

// TestHandleEventsReturnsPromptlyOnShutdown backs a finding from PR #119's
// review: http.Server.Shutdown does not cancel in-flight request contexts,
// it only waits for connections to go idle. A long-lived SSE stream (the
// SPA's dashboard tab) never goes idle on its own, so without a select case
// on s.shutdown, a single connected client would keep handleEvents blocked
// until the shutdown budget itself expired -- turning every routine restart
// with a dashboard open into a false "degraded shutdown."
func TestHandleEventsReturnsPromptlyOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	shutdown := make(chan struct{})
	srv := New(Deps{
		Config: &config.Config{}, Log: slog.New(slog.DiscardHandler), DB: database,
		Prober: probe.New(), Pool: workers.New[string](1, 4),
		Engine: graph.NewEngine(database, nil), Hub: sse.New(),
		Version: "test", Shutdown: shutdown,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read past the initial "event: progress\ndata: ...\n\n" payload sent on
	// connect (three lines, ending at the blank-line separator) so we know
	// the handler's loop is genuinely live -- and so nothing from that
	// initial send is still sitting in the buffered reader, which would let
	// the post-shutdown read below succeed on stale, already-received bytes
	// instead of actually observing shutdown behavior.
	reader := bufio.NewReader(resp.Body)
	for range 3 {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read initial SSE event: %v", err)
		}
	}

	close(shutdown)

	readDone := make(chan error, 1)
	go func() {
		_, err := reader.ReadString('\n')
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err == nil {
			t.Error("stream read returned nil error after shutdown closed, want EOF/closed-connection (handler should have returned)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not close the stream within 2s of the shutdown channel closing")
	}
}
