package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

func testServer() *Server {
	return New(&config.Config{}, slog.New(slog.DiscardHandler))
}

func TestHealthHandler(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rr.Header().Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy header not set")
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	recoverMiddleware(log, panicHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (panic should be recovered, not propagated)", rr.Code)
	}
}

func TestNotFoundRoutesReturn404(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// No SPA fallback is registered until PR 9/10; an unknown route is a
	// plain 404 for now.
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no SPA fallback registered yet)", rr.Code)
	}
}
