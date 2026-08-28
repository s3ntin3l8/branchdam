package httpapi

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

const indexHTMLFixture = `<!doctype html><html><head>` +
	`<meta property="og:image" content="__BRANCHDAM_ORIGIN__/og-image.png" />` +
	`</head><body>shell</body></html>`

// testServerWithSPA mirrors testServer (server_test.go) but embeds a real
// (in-memory) SPA filesystem -- testServer deliberately leaves Deps.SPA nil
// for TestNotFoundRoutesReturn404, so this is a separate helper rather than
// a parameter added to that one.
func testServerWithSPA(t *testing.T, spa fs.FS) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "httpapi.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	log := slog.New(slog.DiscardHandler)
	pool := workers.New[string](1, 4)

	return New(Deps{
		Config: &config.Config{}, Log: log, DB: database,
		Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, log), Hub: sse.New(),
		Version: "test", SPA: spa,
	})
}

func TestServeIndexHTMLSubstitutesOriginFromDirectRequest(t *testing.T) {
	spa := fstest.MapFS{"index.html": {Data: []byte(indexHTMLFixture)}}
	srv := testServerWithSPA(t, spa)

	req := httptest.NewRequest(http.MethodGet, "http://branchdam.example/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	body, _ := io.ReadAll(rr.Body)
	want := `content="http://branchdam.example/og-image.png"`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("body = %q, want it to contain %q", body, want)
	}
}

func TestServeIndexHTMLHonorsForwardedHeaders(t *testing.T) {
	spa := fstest.MapFS{"index.html": {Data: []byte(indexHTMLFixture)}}
	srv := testServerWithSPA(t, spa)

	req := httptest.NewRequest(http.MethodGet, "http://internal-backend:8080/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "dam.example.com")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	want := `content="https://dam.example.com/og-image.png"`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("body = %q, want it to contain %q (Traefik-forwarded origin, not the internal Host)", body, want)
	}
}

func TestServeIndexHTMLOnDeepLinkFallback(t *testing.T) {
	spa := fstest.MapFS{"index.html": {Data: []byte(indexHTMLFixture)}}
	srv := testServerWithSPA(t, spa)

	// /assets/123 isn't a real file in the SPA fs -- spaHandler must fall
	// back to the (still-templated) index.html so client-side routing works.
	req := httptest.NewRequest(http.MethodGet, "http://branchdam.example/assets/123", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	want := `content="http://branchdam.example/og-image.png"`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("body = %q, want the templated shell", body)
	}
}
