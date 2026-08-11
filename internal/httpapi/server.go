// Package httpapi is branchDAM's HTTP surface: middleware chain, health,
// and (from PR 9 onward) the Huma-generated API, SSE progress endpoint, and
// the embedded SPA fallback.
//
// PR 0 wires up only the middleware chain and /healthz -- everything else
// (auth.Route splitting browser vs. agent traffic, the route table, SSE) is
// layered on top of this same Handler() in PR 8/9 without changing its shape.
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

// Server bundles the dependencies handlers need. Grows in later PRs (db
// pools, storage.Guard, sse.Hub, auth chains) without changing this shape.
type Server struct {
	cfg *config.Config
	log *slog.Logger
}

// New builds the HTTP server handler set.
func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

// contentSecurityPolicy is intentionally tighter than a typical SPA CSP: the
// frontend build (PR 10) self-hosts its fonts via Tailwind rather than
// pulling from a Google Fonts CDN, so there is no third-party style-src/
// font-src to allow. img-src permits data: and blob: for inline thumbnail
// previews rendered client-side; connect-src 'self' covers the SSE stream,
// which is same-origin.
const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' data: blob:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"

// Handler returns the root mux wrapped in the middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return recoverMiddleware(s.log, securityHeaders(logMiddleware(s.log, mux)))
}

// securityHeaders adds defense-in-depth response headers to every response.
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

// recoverMiddleware turns a handler panic into a logged 500 instead of
// letting it tear down the connection silently.
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
