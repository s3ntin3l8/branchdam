package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

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

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
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
	// Shell must revalidate on every load -- hashed asset filenames
	// change every build, so a cached old shell is exactly the
	// "looks like the deploy didn't take" failure mode SPA cache
	// hardening is meant to prevent.
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
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

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})

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

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})

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

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})

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

// TestSPAAssetCacheHeaders pins the immutable-cache policy for hashed
// assets under /assets/. Vite content-hashes every emitted JS/CSS chunk,
// so a year-long max-age is safe: the filename itself changes per build.
// Non-asset paths (favicon, manifest) keep the FileServer default so a
// stale favicon doesn't last a year.
func TestSPAAssetCacheHeaders(t *testing.T) {
	spa := fstest.MapFS{
		"index.html":              {Data: []byte(indexHTMLFixture)},
		"assets/index-AbCdEf.js":  {Data: []byte("// hashed asset")},
		"assets/index-AbCdEf.css": {Data: []byte("/* hashed asset */")},
		"favicon.ico":             {Data: []byte{0x00, 0x01, 0x02}},
	}
	srv := testServerWithSPA(t, spa)

	tests := []struct {
		path string
		want string
	}{
		{"/assets/index-AbCdEf.js", "public, max-age=31536000, immutable"},
		{"/assets/index-AbCdEf.css", "public, max-age=31536000, immutable"},
		// Non-asset embedded files: FileServer default (no Cache-Control
		// from us). Asset names always live under assets/ in a Vite
		// build, so this won't pin a year-long cache to anything
		// content-addressable.
		{"/favicon.ico", ""},
	}
	for _, tc := range tests {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://branchdam.example"+tc.path, nil)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSPABuildIDReadFromEmbeddedFS asserts that the identifier vite's
// branchdamBuildStamp plugin writes to web/dist/BUILD_ID is read at
// startup and exposed through /api/v1/config. Tests that supply no SPA
// (the default fullTestServer case) must see an empty string -- the
// read failures must never panic / block boot.
func TestSPABuildIDReadFromEmbeddedFS(t *testing.T) {
	spa := fstest.MapFS{
		"index.html": {Data: []byte(indexHTMLFixture)},
		// A trailing newline is what the vite plugin actually writes;
		// verify the trim, not the byte-for-byte round-trip.
		"BUILD_ID": {Data: []byte("a1b2c3d-dirty\n")},
	}
	srv := testServerWithSPA(t, spa)

	rr := httptest.NewRequest(http.MethodGet, "http://branchdam.example/api/v1/config", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, rr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		SPABuildID string `json:"spaBuildId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SPABuildID != "a1b2c3d-dirty" {
		t.Errorf("spaBuildId = %q, want %q", got.SPABuildID, "a1b2c3d-dirty")
	}

	// No BUILD_ID file -> empty string, never an error.
	noStamp := fstest.MapFS{
		"index.html": {Data: []byte(indexHTMLFixture)},
	}
	srv = testServerWithSPA(t, noStamp)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://branchdam.example/api/v1/config", nil))
	var missing struct {
		SPABuildID string `json:"spaBuildId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &missing); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if missing.SPABuildID != "" {
		t.Errorf("spaBuildId = %q, want empty when BUILD_ID missing", missing.SPABuildID)
	}

	// Oversize BUILD_ID with a UTF-8 rune straddling the 128-byte cap:
	// the previous id[:128] truncated mid-rune and emitted invalid
	// UTF-8 to JSON clients. ToValidUTF8 drops the broken suffix.
	//
	// Fixture math: 126 ASCII bytes + a 3-byte '€' = 129 total bytes;
	// byte 128 is the lone 0x82 trailing byte of the € rune, so
	// id[:128] ends mid-rune and decodes as invalid UTF-8 (Hermes
	// round-2 reproducer). Earlier fixtures using 125 ASCII bytes
	// landed byte 128 exactly on a rune boundary and silently passed
	// against the pre-fix code -- the test was meaningless there.
	oversize := strings.Repeat("a", 126) + "€€€€€€€"
	raw := oversize + "\n"
	trimmed := strings.TrimSpace(raw)

	// Sanity-check the fixture before exercising the cap: the pre-fix
	// form (id[:128]) MUST be invalid UTF-8, otherwise we are not
	// actually exercising the regression. This catches a future tweak
	// that quietly re-aligns the boundary.
	if utf8.ValidString(trimmed[:128]) {
		t.Fatalf("fixture no longer reproduces the bug: id[:128] = %q is valid UTF-8; bump the ASCII prefix to land byte 128 mid-rune", trimmed[:128])
	}
	if !utf8.ValidString(oversize) {
		t.Fatalf("fixture itself is invalid UTF-8 before the cap: %q", oversize)
	}

	spa = fstest.MapFS{
		"index.html": {Data: []byte(indexHTMLFixture)},
		"BUILD_ID":   {Data: []byte(raw)},
	}
	srv = testServerWithSPA(t, spa)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://branchdam.example/api/v1/config", nil))
	var oversizeOut struct {
		SPABuildID string `json:"spaBuildId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &oversizeOut); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !utf8.ValidString(oversizeOut.SPABuildID) {
		t.Errorf("spaBuildId = %q, want valid UTF-8 after cap", oversizeOut.SPABuildID)
	}
	if len(oversizeOut.SPABuildID) > 128 {
		t.Errorf("spaBuildId length = %d, want <=128", len(oversizeOut.SPABuildID))
	}
}
