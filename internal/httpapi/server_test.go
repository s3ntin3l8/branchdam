package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

func testServer(t *testing.T) *Server {
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

	// SPA is deliberately left nil in this fixture -- TestNotFoundRoutesReturn404
	// depends on spaHandler()'s nil-spa fallback being a plain 404, not the
	// SPA shell web.Dist() provides.
	return New(Deps{
		Config: &config.Config{}, Log: log, DB: database,
		Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, log), Hub: sse.New(),
		Version: "test",
	})
}

func TestHealthHandler(t *testing.T) {
	srv := testServer(t)
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
	srv := testServer(t)
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

// TestSanitizeForLog is the unit-level guard for the go/log-injection
// CodeQL alerts on recoverMiddleware/logMiddleware: r.URL.Path is
// client-controlled (a percent-encoded %0d%0a in the request line decodes
// to a literal CR/LF by the time net/url has parsed it into r.URL.Path),
// so a value containing CR/LF must have both stripped before it's safe to
// hand to a plain-text log sink.
func TestSanitizeForLog(t *testing.T) {
	in := "/api/v1/assets\r\n2026-01-01 00:00:00 FORGED level=ERROR msg=\"fake entry\""
	got := sanitizeForLog(in)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("sanitizeForLog(%q) = %q, still contains CR/LF", in, got)
	}
	want := "/api/v1/assets2026-01-01 00:00:00 FORGED level=ERROR msg=\"fake entry\""
	if got != want {
		t.Errorf("sanitizeForLog(%q) = %q, want %q", in, got, want)
	}
}

// TestRecoveryMiddlewareSanitizesPathInLog proves the CR/LF strip actually
// applies on the real logging path, not just in sanitizeForLog isolation:
// a request whose URL.Path carries an injected CR/LF must not let that
// CR/LF reach the log sink verbatim -- log/slog's TextHandler would
// otherwise emit it as a literal newline, letting the attacker-controlled
// suffix masquerade as a separate, forged log entry.
func TestRecoveryMiddlewareSanitizesPathInLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/api/v1/assets\r\nlevel=ERROR msg=\"forged by client\""
	rr := httptest.NewRecorder()

	recoverMiddleware(log, panicHandler).ServeHTTP(rr, req)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log output = %q, want exactly one line -- the injected CR/LF must not split it into %d", buf.String(), len(lines))
	}
}

// TestLogMiddlewareSanitizesPathInLog is logMiddleware's counterpart to
// TestRecoveryMiddlewareSanitizesPathInLog.
func TestLogMiddlewareSanitizesPathInLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	noop := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/api/v1/assets\r\nlevel=ERROR msg=\"forged by client\""
	rr := httptest.NewRecorder()

	logMiddleware(log, noop).ServeHTTP(rr, req)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log output = %q, want exactly one line -- the injected CR/LF must not split it into %d", buf.String(), len(lines))
	}
}

func TestNotFoundRoutesReturn404(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// spaHandler() falls back to a plain 404 when no SPA has been embedded
	// -- this fixture deliberately leaves Deps.SPA nil (see testServer).
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no SPA embedded in this test fixture)", rr.Code)
	}
}
