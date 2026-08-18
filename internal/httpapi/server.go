// Package httpapi is branchDAM's HTTP surface: middleware chain, the
// Huma-generated REST API, the SSE progress stream, and the embedded SPA
// fallback.
package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
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
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

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

	// Tracker, if set, is passed through to every pipeline.ScanDeps this
	// server builds, so cmd/branchdam can join scans started via
	// POST /api/v1/scan before closing the database on shutdown.
	Tracker *pipeline.ScanTracker
}

// Server bundles the dependencies handlers need.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	db      *db.DB
	guard   *storage.Guard
	prober  *probe.Prober
	pool    *workers.Pool[string]
	engine  *graph.Engine
	hub     *sse.Hub
	sseSlot *limiter
	spa     fs.FS
	version string
	tracker *pipeline.ScanTracker
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
	return &Server{
		cfg:     d.Config,
		log:     log,
		db:      d.DB,
		guard:   d.Guard,
		prober:  d.Prober,
		pool:    d.Pool,
		engine:  d.Engine,
		hub:     d.Hub,
		sseSlot: newLimiter(maxSSEClients),
		spa:     d.SPA,
		version: version,
		tracker: d.Tracker,
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
	if s.cfg != nil {
		exposeOpenAPI = s.cfg.HTTP.ExposeOpenAPI
		allowedGroups = s.cfg.Authz.Groups
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
	mux.Handle("GET /", s.spaHandler())

	var apiKey string
	if s.cfg != nil {
		apiKey = s.cfg.Agent.APIKey
	}

	authzHandler := openAPIMiddleware(exposeOpenAPI, allowedGroups, s.log, mux)
	routed := auth.Route(apiKey, s.log, authzHandler)

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

func recoverMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic in handler", "path", r.URL.Path, "err", v)
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
			log.Debug("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// spaHandler serves embedded assets, falling back to index.html for client
// routes (so deep links work). web.Dist() provides the embedded FS
// (web/embed.go); a nil s.spa (tests that don't build an SPA) yields a 404,
// not the SPA shell.
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
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
