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
	path := filepath.Join(t.TempDir(), "httpapi.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	log := slog.New(slog.DiscardHandler)
	pool := workers.New[string](1, 4)
	cfg := &config.Config{
		HTTP: config.HTTP{TrustedProxies: []string{"*"}},
	}
	srv := New(Deps{
		Config: cfg, Log: log, DB: database,
		Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, log), Hub: sse.New(),
		Version: "test", SPA: spa,
	})

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

func TestServeIndexHTMLEscapesHostileHeaders(t *testing.T) {
	spa := fstest.MapFS{"index.html": {Data: []byte(indexHTMLFixture)}}
	srv := testServerWithSPA(t, spa)

	req := httptest.NewRequest(http.MethodGet, "http://branchdam.example/", nil)
	req.Header.Set("X-Forwarded-Host", `evil.example"><script>alert(1)</script>`)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	// CodeQL flagged the pre-fix version of this handler as reflected XSS:
	// requestOrigin() builds its return value straight from request headers,
	// which are attacker-controlled, and that value lands inside an HTML
	// attribute. The unescaped payload must never appear verbatim.
	if bytes.Contains(body, []byte(`"><script>`)) {
		t.Fatalf("body = %q, contains an unescaped attribute breakout -- reflected XSS", body)
	}
	// With trust-all-by-default, the hostile header IS honored but
	// HTML-escaped by html.EscapeString, preventing XSS.
	if !bytes.Contains(body, []byte("&lt;script&gt;")) {
		t.Errorf("body = %q, want the header's <script> HTML-escaped, not stripped or passed through", body)
	}
}

func TestIsTrustedProxy(t *testing.T) {
	tests := []struct {
		name       string
		trusted    []string
		remoteAddr string
		want       bool
	}{
		{"unset trusts all (backward compatible)", nil, "10.0.0.1:1234", true},
		{"empty list denies all", []string{}, "10.0.0.1:1234", false},
		{"wildcard accepts all", []string{"*"}, "10.0.0.1:1234", true},
		{"exact IP match", []string{"10.0.0.1"}, "10.0.0.1:1234", true},
		{"exact IP no match", []string{"10.0.0.2"}, "10.0.0.1:1234", false},
		{"CIDR match", []string{"10.0.0.0/24"}, "10.0.0.5:1234", true},
		{"CIDR no match", []string{"10.0.1.0/24"}, "10.0.0.5:1234", false},
		{"IPv6 loopback", []string{"::1"}, "[::1]:1234", true},
		{"IPv4-mapped IPv6", []string{"10.0.0.1"}, "[::ffff:10.0.0.1]:1234", true},
		{"invalid IP ignored", []string{"not-an-ip"}, "10.0.0.1:1234", false},
		{"empty entry skipped", []string{"", "10.0.0.1"}, "10.0.0.1:1234", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isTrustedProxy(tc.remoteAddr, tc.trusted)
			if got != tc.want {
				t.Errorf("isTrustedProxy(%q, %v) = %v, want %v", tc.remoteAddr, tc.trusted, got, tc.want)
			}
		})
	}
}

func TestServeIndexHTMLHonorsForwardedHeadersByDefault(t *testing.T) {
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
		t.Errorf("body = %q, want forwarded headers honored by default (backward-compatible trust-all)", body)
	}
}

func TestServeIndexHTMLRejectsForwardedHeadersFromUntrustedProxy(t *testing.T) {
	spa := fstest.MapFS{"index.html": {Data: []byte(indexHTMLFixture)}}
	path := filepath.Join(t.TempDir(), "httpapi.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	log := slog.New(slog.DiscardHandler)
	pool := workers.New[string](1, 4)
	cfg := &config.Config{
		HTTP: config.HTTP{TrustedProxies: []string{}},
	}
	srv := New(Deps{
		Config: cfg, Log: log, DB: database,
		Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, log), Hub: sse.New(),
		Version: "test", SPA: spa,
	})

	req := httptest.NewRequest(http.MethodGet, "http://internal-backend:8080/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "dam.example.com")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	want := `content="http://internal-backend:8080/og-image.png"`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("body = %q, want forwarded headers ignored when trusted proxies is explicitly empty", body)
	}
}

func TestServeIndexHTMLHonorsForwardedHeadersFromTrustedProxy(t *testing.T) {
	spa := fstest.MapFS{"index.html": {Data: []byte(indexHTMLFixture)}}
	path := filepath.Join(t.TempDir(), "httpapi.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	log := slog.New(slog.DiscardHandler)
	pool := workers.New[string](1, 4)
	cfg := &config.Config{
		HTTP: config.HTTP{TrustedProxies: []string{"10.0.0.0/24"}},
	}
	srv := New(Deps{
		Config: cfg, Log: log, DB: database,
		Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, log), Hub: sse.New(),
		Version: "test", SPA: spa,
	})

	req := httptest.NewRequest(http.MethodGet, "http://10.0.0.5:8080/", nil)
	req.RemoteAddr = "10.0.0.99:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "dam.example.com")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	want := `content="https://dam.example.com/og-image.png"`
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("body = %q, want forwarded headers honored from trusted proxy", body)
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
