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

// TestHandleEventsReturnsPromptlyOnShutdown backs #124: http.Server.Shutdown
// does not cancel in-flight request contexts, it only waits for connections
// to go idle. A long-lived SSE stream (the SPA's dashboard tab) never goes
// idle on its own, so without a select case on s.shutdown, a single
// connected client would keep handleEvents blocked until the shutdown
// timeout itself expired -- turning every routine restart with a dashboard
// open into a stalled one.
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

// TestHandleEventsIgnoresServerWriteTimeout backs a fix for
// http.Server.WriteTimeout/ReadTimeout (cmd/branchdam/main.go, 15s default)
// applying unmodified to this long-lived stream and tearing it down well
// before its own 20s heartbeat ever fires once -- the client's EventSource
// reconnects, so this was invisible as a hard failure, just reconnect churn
// and a blind window each cycle where a nudge could be missed. Uses a short
// WriteTimeout/ReadTimeout to stand in for that behavior without waiting a
// full 15s: without handleEvents clearing the deadline via
// http.ResponseController, the connection is closed by net/http itself
// well before the nudge below has any chance to arrive.
func TestHandleEventsIgnoresServerWriteTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-timeout.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	hub := sse.New()
	srv := New(Deps{
		Config: &config.Config{}, Log: slog.New(slog.DiscardHandler), DB: database,
		Prober: probe.New(), Pool: workers.New[string](1, 4),
		Engine: graph.NewEngine(database, nil), Hub: hub,
		Version: "test",
	})

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.WriteTimeout = 200 * time.Millisecond
	ts.Config.ReadTimeout = 200 * time.Millisecond
	ts.Start()
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

	reader := bufio.NewReader(resp.Body)
	for range 3 { // the initial "event: progress\ndata: ...\n\n" sent on connect
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read initial SSE event: %v", err)
		}
	}

	// Wait well past the server's WriteTimeout, then nudge -- if the
	// deadline wasn't cleared, the connection is already closed by now and
	// neither this nudge nor the read below will ever arrive.
	time.Sleep(500 * time.Millisecond)
	hub.Broadcast()

	readDone := make(chan error, 1)
	go func() {
		_, err := reader.ReadString('\n')
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read after WriteTimeout elapsed: %v (connection was likely closed by the server's WriteTimeout)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive the post-timeout nudge within 2s")
	}
}
