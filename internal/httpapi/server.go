// Package httpapi is branchDAM's HTTP surface: middleware chain, the
// Huma-generated REST API, the SSE progress stream, and the embedded SPA
// fallback.
package httpapi

import (
	"bytes"
	"html"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/thumbs"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

// configProvider is the seam that lets the HTTP layer read either a fixed,
// process-lifetime config.Config (every existing test and the Deps.Config
// path) or a *settings.Store's live-resolved one, through the same
// s.cfg() call. *settings.Store already satisfies this directly.
type configProvider interface {
	Effective() *config.Config
}

// staticConfigProvider adapts a bare *config.Config (Deps.Config, with no
// Deps.Settings supplied) to configProvider -- this is what keeps every
// existing Server-constructing test compiling unchanged: Deps.Config never
// changed shape, only how it's wrapped internally.
type staticConfigProvider struct{ cfg *config.Config }

func (p staticConfigProvider) Effective() *config.Config { return p.cfg }

// Deps bundles everything the HTTP layer needs. Built once at startup in
// cmd/branchdam and handed to New.
type Deps struct {
	Config  *config.Config
	Log     *slog.Logger
	DB      *db.DB
	Guard   *storage.Guard
	Prober  *probe.Prober
	Pool    *workers.Pool[string]
	Engine  *graph.Engine
	Hub     *sse.Hub
	SPA     fs.FS
	Version string

	// Settings, if set, is the live-resolved config source: s.cfg() reads
	// through it (Store.Effective()) instead of Config directly, and the
	// GET/PUT /api/v1/settings routes use it for everything Effective()
	// alone can't answer (Apply, PendingRestart, IsOverridden). Config is
	// still required either way -- Settings doesn't replace it, it's what
	// cmd/branchdam builds Settings *from*. Nil (every existing test, and
	// any server built without a *settings.Store) falls back to a fixed
	// wrapper around Config, so s.cfg() never needs a nil check at the
	// call site.
	Settings *settings.Store

	// ThumbCache serves GET /api/v1/assets/{id}/thumbnail. May be nil (a
	// misconfigured/uncreatable cache dir at startup, or thumbnails not
	// wired at all in a test) -- the route 404s rather than panicking.
	ThumbCache *thumbs.Cache

	// Tracker, if set, is passed through to every pipeline.ScanDeps this
	// server builds, so cmd/branchdam can join scans started via
	// POST /api/v1/scan before closing the database on shutdown.
	Tracker *pipeline.ScanTracker

	// Shutdown, if set, is passed through to every pipeline.ScanDeps this
	// server builds -- see pipeline.ScanDeps.Shutdown's doc comment. Not the
	// request context: a scan must survive the HTTP request that started it,
	// same reasoning as Tracker.
	Shutdown <-chan struct{}

	// RequestRestart, if set, is POST /api/v1/restart's mechanism for
	// triggering a graceful process restart -- cmd/branchdam wires this to
	// the same stop() SIGTERM/SIGINT already cancel (signal.NotifyContext),
	// so the shutdown sequence that follows is identical either way, plus a
	// flag telling main to re-exec afterward instead of exiting. Nil (every
	// existing test, and any Server built without it) makes the route
	// return 503 rather than silently doing nothing -- see restart.go.
	RequestRestart func()
}

// Server bundles the dependencies handlers need.
type Server struct {
	cfgProvider    configProvider
	settingsStore  *settings.Store // nil unless Deps.Settings was supplied
	log            *slog.Logger
	db             *db.DB
	guard          *storage.Guard
	prober         *probe.Prober
	pool           *workers.Pool[string]
	engine         *graph.Engine
	hub            *sse.Hub
	sseSlot        *limiter
	sseCache       ssePayloadCache
	spa            fs.FS
	version        string
	tracker        *pipeline.ScanTracker
	shutdown       <-chan struct{}
	thumbs         *thumbs.Cache
	requestRestart func()
}

// cfg returns the current effective config -- config.yaml/.env as loaded,
// with any settings override resolved on top when Deps.Settings was
// supplied. Every handler reads config through this, never through a
// stored *config.Config, so a settings change is visible on the very next
// request without a restart (for whichever fields Store.Apply already
// updated Effective() with).
func (s *Server) cfg() *config.Config {
	if s.cfgProvider == nil {
		return nil
	}
	return s.cfgProvider.Effective()
}

// New builds the HTTP server handler set.
func New(d Deps) *Server {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	version := d.Version
	if version == "" {
		version = "dev"
	}
	var cfgProvider configProvider = staticConfigProvider{cfg: d.Config}
	if d.Settings != nil {
		cfgProvider = d.Settings
	}
	// Surface the trust-all-proxy default at startup so a security-minded
	// operator notices they're running with the backward-compat branch
	// of isTrustedProxy. The check below mirrors isTrustedProxy's nil =>
	// true behavior: nil or empty list means "trust all". An explicit
	// non-empty list (even just "*") means the operator has opted in.
	if cfg := cfgProvider.Effective(); cfg != nil {
		if len(cfg.HTTP.TrustedProxies) == 0 {
			log.Warn("http: trustedProxies is empty -- X-Forwarded-* headers are trusted from any source (backward-compat default). Set http.trustedProxies to your reverse proxy's IP/CIDR to harden.")
		}
	}
	return &Server{
		cfgProvider:    cfgProvider,
		settingsStore:  d.Settings,
		log:            log,
		db:             d.DB,
		guard:          d.Guard,
		prober:         d.Prober,
		pool:           d.Pool,
		engine:         d.Engine,
		hub:            d.Hub,
		sseSlot:        newLimiter(maxSSEClients),
		spa:            d.SPA,
		version:        version,
		tracker:        d.Tracker,
		shutdown:       d.Shutdown,
		thumbs:         d.ThumbCache,
		requestRestart: d.RequestRestart,
	}
}

// contentSecurityPolicy: the frontend build (PR 10) self-hosts its fonts
// via Tailwind rather than pulling from a Google Fonts CDN, so there is no
// third-party style-src/font-src to allow. img-src permits data: and
// blob: for inline thumbnail previews rendered client-side; connect-src
// 'self' covers the SSE stream, which is same-origin.
const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' data: blob:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"

// Handler returns the root mux wrapped in the middleware chain: recover ->
// securityHeaders -> log -> auth.Route -> openAPIMiddleware -> mux. auth.Route is innermost
// among global wrappers because it decides WHICH identity extraction runs before any handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	exposeOpenAPI := false
	var allowedGroups []string
	if cfg := s.cfg(); cfg != nil {
		exposeOpenAPI = cfg.HTTP.ExposeOpenAPI
		allowedGroups = cfg.Authz.Groups
	}

	humaConfig := huma.DefaultConfig("branchDAM", s.version)
	if !exposeOpenAPI {
		humaConfig.OpenAPIPath = ""
		humaConfig.DocsPath = ""
	}

	api := humago.New(mux, humaConfig)
	s.registerRoutes(api)

	mux.HandleFunc("GET /healthz", s.handleHealth)
	// SSE is registered directly on the mux, not through Huma -- Huma's
	// response model fights a streaming text/event-stream handler.
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	// Thumbnails are also registered directly on the mux, for the same
	// reason -- Huma's response model expects a JSON body, not a raw
	// image/jpeg byte stream.
	mux.HandleFunc("GET /api/v1/assets/{id}/thumbnail", s.handleThumbnail)
	// Agent streaming upload accepts raw octet stream with custom headers
	mux.HandleFunc("POST /api/v1/agent/upload", s.handleAgentUpload)
	// Web browser multipart upload
	mux.HandleFunc("POST /api/v1/upload", s.handleWebUpload)
	mux.Handle("GET /", s.spaHandler())

	var agentCfg auth.AgentConfig
	if cfg := s.cfg(); cfg != nil {
		agentCfg = auth.AgentConfig{
			APIKey:             cfg.Agent.APIKey,
			SignedRequests:     cfg.Agent.SignedRequests,
			ReplayWindow:       cfg.Agent.ReplayWindow(),
			SignedMaxBodyBytes: cfg.Agent.SignedMaxBodyBytes,
			SkipSignaturePaths: cfg.Agent.SkipSignaturePaths,
		}
	}

	authzHandler := openAPIMiddleware(exposeOpenAPI, allowedGroups, s.log, mux)
	routed := auth.RouteWithConfig(agentCfg, s.log, authzHandler)

	return recoverMiddleware(s.log, securityHeaders(logMiddleware(s.log, routed)))
}

func openAPIMiddleware(exposeOpenAPI bool, allowedGroups []string, log *slog.Logger, next http.Handler) http.Handler {
	requireAdmin := auth.RequireAdmin(allowedGroups, log)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isOpenAPI := path == "/openapi.json" || path == "/openapi.yaml" || path == "/openapi" || path == "/docs" || strings.HasPrefix(path, "/docs/") || strings.HasPrefix(path, "/openapi/")
		if isOpenAPI {
			if !exposeOpenAPI {
				http.NotFound(w, r)
				return
			}
			p, ok := auth.From(r.Context())
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"$schema":"https://huma.rocks/schema/error.json","title":"Forbidden","status":403,"detail":"authentication required"}` + "\n"))
				return
			}
			// #164: same reasoning as auth.RequireAdmin -- a browser
			// principal with no identity headers at all must not pass this
			// stricter (GET-included) admin check as if it were a real
			// logged-in user.
			if p.Kind != auth.KindMachine && !p.Authenticated {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"$schema":"https://huma.rocks/schema/error.json","title":"Forbidden","status":403,"detail":"authentication required"}` + "\n"))
				return
			}
			if p.Kind != auth.KindMachine && len(allowedGroups) > 0 {
				isAdmin := false
				for _, g := range p.Groups {
					if slices.Contains(allowedGroups, g) {
						isAdmin = true
						break
					}
				}
				if !isAdmin {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"$schema":"https://huma.rocks/schema/error.json","title":"Forbidden","status":403,"detail":"admin authorization required"}` + "\n"))
					return
				}
			}
		}
		requireAdmin(next).ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// sanitizeForLog replaces CR/LF in a user-controlled value (here, always
// r.URL.Path) with their visible escape sequences before it's written to a
// log record. CodeQL's go/log-injection flags both call sites below:
// net/http's ServeMux/Huma reject a raw CR/LF in the request line itself,
// but nothing stops a client from percent-encoding one (%0d%0a), which
// net/url decodes back into r.URL.Path -- so this is a real
// client-controlled forged-log-entry vector, not just a lint nit.
// Replacing rather than deleting keeps the line break visible (as literal
// `\r`/`\n`) instead of silently concatenating the forged suffix onto the
// path with no separator -- matching the go/log-injection alert's own
// recommended fix (CWE-117): a forged log line is defused by removing the
// ACTUAL line breaks that would let it masquerade as a separate entry,
// without changing request handling or losing the rest of the value.
func sanitizeForLog(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func recoverMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic in handler", "path", sanitizeForLog(r.URL.Path), "err", v)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Debug("request", "method", r.Method, "path", sanitizeForLog(r.URL.Path), "dur", time.Since(start))
		}
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func init() {
	// Go's builtin extension->MIME table has no entry for .webmanifest, so
	// http.FileServer would otherwise sniff web/dist/site.webmanifest as
	// text/plain. Browsers are lenient about this, but Lighthouse's PWA
	// installability check looks at it.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// spaHandler serves embedded assets, falling back to index.html for client
// routes (so deep links work). web.Dist() provides the embedded FS
// (web/embed.go); a nil s.spa (tests that don't build an SPA) yields a 404,
// not the SPA shell. index.html itself is never handed to fileServer
// unmodified -- see serveIndexHTML.
func (s *Server) spaHandler() http.Handler {
	if s.spa == nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(s.spa))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.spa, p); err != nil {
			p = "index.html"
		}
		if p == "index.html" {
			s.serveIndexHTML(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// serveIndexHTML serves the SPA shell with __BRANCHDAM_ORIGIN__ substituted
// for the request's own scheme+host, computed per request rather than baked
// in at build time: branchDAM is self-hosted behind an operator-chosen
// reverse proxy (AGENTS.md: Traefik v3 + Authentik ForwardAuth) with no
// fixed public domain, so the Open Graph/Twitter card image and url tags
// can only be made absolute -- as the OG/Twitter spec requires -- from the
// incoming request's Host/X-Forwarded-* headers.
//
// requestOrigin's output is attacker-influenceable (Host/X-Forwarded-* are
// request headers, not validated hostnames) and lands inside HTML attribute
// values below, so it's HTML-escaped before substitution -- without this, a
// crafted header (e.g. `X-Forwarded-Host: x"><script>...`) would be a
// reflected-XSS vector.
func (s *Server) serveIndexHTML(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.spa, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var trustedProxies []string
	if s.cfg() != nil {
		trustedProxies = s.cfg().HTTP.TrustedProxies
	}
	origin := html.EscapeString(requestOrigin(r, trustedProxies))
	page := bytes.ReplaceAll(data, []byte("__BRANCHDAM_ORIGIN__"), []byte(origin))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))
	_, _ = w.Write(page)
}

// requestOrigin reconstructs the scheme+host the client used to reach this
// request, honoring the X-Forwarded-* headers Traefik sets when proxying,
// and falling back to the direct connection's own Host/TLS state (e.g. for
// `make dev-api`, which has no proxy in front).
func requestOrigin(r *http.Request, trustedProxies []string) string {
	scheme := "http"
	host := r.Host
	if isTrustedProxy(r.RemoteAddr, trustedProxies) {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else if r.TLS != nil {
			scheme = "https"
		}
		if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
			host = fwdHost
		}
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}

// isTrustedProxy checks whether remoteAddr belongs to a trusted proxy range.
// When trusted is nil, all proxies are trusted for backward compatibility:
// the original behavior honored X-Forwarded-* headers unconditionally.
// An explicitly empty list denies all forwarded headers.
func isTrustedProxy(remoteAddr string, trusted []string) bool {
	if trusted == nil {
		return true
	}

	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	remoteIP, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	remoteIP = remoteIP.Unmap()

	for _, entry := range trusted {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				continue
			}
			if prefix.Contains(remoteIP) {
				return true
			}
		} else {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				continue
			}
			if addr == remoteIP {
				return true
			}
		}
	}
	return false
}
