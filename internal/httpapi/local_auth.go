package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/ratelimit"
	"github.com/s3ntin3l8/branchdam/internal/auth/session"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
)

// localAuthHandlers bundles the wiring the local-auth endpoints need.
// Constructed once at startup in registerLocalAuthRoutes; carries the
// user service, login rate limiter, and session middleware (which
// knows the cookie name + timeouts).
type localAuthHandlers struct {
	users        *users.Service
	loginLimiter *ratelimit.Limiter
	sessionMw    *session.Middleware
	log          *slog.Logger
	authMode     auth.AuthMode
	trustedIPs   []string
}

// registerLocalAuthRoutes mounts /api/v1/setup/status,
// /api/v1/setup/admin, /api/v1/login, and DELETE /api/v1/session
// directly on the mux (Huma's response model is JSON-only; these
// endpoints must write Set-Cookie headers, which Huma's response
// pipeline doesn't expose cleanly).
func (s *Server) registerLocalAuthRoutes(mux *http.ServeMux) {
	if s.localAuth == nil {
		// Local auth disabled. The setup endpoints 503; the session
		// DELETE endpoint is a no-op. We mount them anyway so a
		// SPA accidentally hitting them in forward-only mode gets a
		// clean response, not a 404.
		mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatusNoLocal)
		mux.HandleFunc("POST /api/v1/setup/admin", s.handleSetupAdminNoLocal)
		mux.HandleFunc("POST /api/v1/login", s.handleLoginNoLocal)
		mux.HandleFunc("DELETE /api/v1/session", s.handleLogoutNoLocal)
		return
	}
	mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/setup/admin", s.handleSetupAdmin)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("DELETE /api/v1/session", s.handleLogout)
}

// --- /api/v1/setup/status ---

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.localAuth.users.CountUsers(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "count users: "+err.Error())
		return
	}
	mode := string(s.localAuth.authMode)
	writeJSON(w, http.StatusOK, map[string]any{
		"readyForSetup": n == 0,
		"mode":          mode,
	})
}

// --- /api/v1/setup/admin ---

func (s *Server) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	if s.localAuth.authMode == auth.AuthModeForward {
		writeJSONError(w, http.StatusConflict, "setup form is only available when auth.mode is 'local' or 'both'")
		return
	}
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	username := strings.TrimSpace(body.Username)
	email := strings.TrimSpace(body.Email)
	if username == "" || len(body.Password) < 8 {
		writeJSONError(w, http.StatusUnprocessableEntity, "username required and password must be at least 8 characters")
		return
	}

	n, err := s.localAuth.users.CountUsers(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "count users: "+err.Error())
		return
	}
	if n > 0 {
		writeJSONError(w, http.StatusNotFound, "setup already complete")
		return
	}

	now := time.Now()
	user, err := s.localAuth.users.CreateLocalUser(r.Context(), username, email, body.Password, true, now.Unix(), "setup")
	if err != nil {
		// Most likely UNIQUE-constraint failure on username; surface as 422.
		writeJSONError(w, http.StatusUnprocessableEntity, "create user: "+err.Error())
		return
	}

	_, cookieValue, err := s.localAuth.users.MintCookieValue()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint cookie: "+err.Error())
		return
	}
	cookieID, _, err := splitCookieValue(cookieValue)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "parse cookie: "+err.Error())
		return
	}
	_, err = s.localAuth.users.CreateSession(r.Context(), user.ID, cookieID, clientIP(r), r.UserAgent(), now.Add(s.localAuth.sessionMw.AbsoluteTimeout()), now.Add(s.localAuth.sessionMw.IdleTimeout()))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: user.ID, Valid: true}, username, "local", "ok", clientIP(r), r.UserAgent(), "{}")

	maxAge := int(s.localAuth.sessionMw.AbsoluteTimeout().Seconds())
	s.localAuth.sessionMw.SetSessionCookie(w, r, cookieValue, maxAge, s.localAuth.trustedIPs)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/v1/login ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.localAuth.authMode == auth.AuthModeForward {
		writeJSONError(w, http.StatusConflict, "login form is only available when auth.mode is 'local' or 'both'")
		return
	}

	ip := clientIP(r)
	if d := s.localAuth.loginLimiter.Check(ip); !d.Allowed {
		w.Header().Set("Retry-After", formatRetryAfter(d.RetryAfter))
		writeJSONError(w, http.StatusTooManyRequests, "rate limited; retry after "+d.RetryAfter.String())
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	username := strings.TrimSpace(body.Username)

	user, err := s.localAuth.users.GetUserByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			s.localAuth.loginLimiter.RecordFailure(ip)
			s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{}, username, "local", "no-such-user", ip, r.UserAgent(), "{}")
			// Constant-time-ish: still verify a dummy hash so the
			// response timing is similar to a real failure.
			_ = s.localAuth.users.VerifyPassword(body.Password, "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
			writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "lookup user: "+err.Error())
		return
	}
	if !user.PasswordHash.Valid {
		s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{}, username, "local", "no-such-user", ip, r.UserAgent(), "{}")
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err := s.localAuth.users.VerifyPassword(body.Password, user.PasswordHash.String); err != nil {
		s.localAuth.loginLimiter.RecordFailure(ip)
		s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: user.ID, Valid: true}, username, "local", "bad-password", ip, r.UserAgent(), "{}")
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if user.DisabledAt.Valid {
		s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: user.ID, Valid: true}, username, "local", "user-disabled", ip, r.UserAgent(), "{}")
		writeJSONError(w, http.StatusForbidden, "account disabled")
		return
	}

	now := time.Now()
	_, cookieValue, err := s.localAuth.users.MintCookieValue()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint cookie: "+err.Error())
		return
	}
	cookieID, _, err := splitCookieValue(cookieValue)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "parse cookie: "+err.Error())
		return
	}
	_, err = s.localAuth.users.CreateSession(r.Context(), user.ID, cookieID, ip, r.UserAgent(), now.Add(s.localAuth.sessionMw.AbsoluteTimeout()), now.Add(s.localAuth.sessionMw.IdleTimeout()))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	s.localAuth.loginLimiter.RecordSuccess(ip)
	s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: user.ID, Valid: true}, username, "local", "ok", ip, r.UserAgent(), "{}")

	maxAge := int(s.localAuth.sessionMw.AbsoluteTimeout().Seconds())
	s.localAuth.sessionMw.SetSessionCookie(w, r, cookieValue, maxAge, s.localAuth.trustedIPs)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/v1/session (DELETE = logout) ---

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Revoke the current session by cookie, if present. Then clear the cookie.
	if cookie, err := r.Cookie(s.localAuth.sessionMw.CookieName()); err == nil {
		if cookieID, err := s.localAuth.users.VerifyCookieValue(cookie.Value); err == nil && cookieID != "" {
			sess, err := s.localAuth.users.GetSessionByCookieID(r.Context(), cookieID)
			if err == nil {
				_ = s.localAuth.users.RevokeSession(r.Context(), sess.ID)
			}
		}
	}
	s.localAuth.sessionMw.ClearSessionCookie(w, r, s.localAuth.trustedIPs)
	w.WriteHeader(http.StatusNoContent)
}

// --- "not configured" stubs (local auth disabled at startup) ---

func (s *Server) handleSetupStatusNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"readyForSetup": false, "mode": "forward"})
}
func (s *Server) handleSetupAdminNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handleLoginNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handleLogoutNoLocal(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

// clientIP returns the originating client IP. Honors X-Forwarded-For
// only from trusted proxies (same logic as httpapi.isTrustedProxy);
// otherwise falls back to r.RemoteAddr. Keeps the rate limit accurate
// behind Traefik AND when branchDAM is hit directly in dev.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if trusted := trustedProxyList(); isTrusted(remoteIP, trusted) {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			// First entry is the original client.
			if comma := strings.IndexByte(fwd, ','); comma >= 0 {
				fwd = fwd[:comma]
			}
			return strings.TrimSpace(fwd)
		}
	}
	return remoteIP.String()
}

// isTrusted duplicates httpapi.isTrustedProxy for this file's local use;
// both evolve together.
func isTrusted(ip netip.Addr, trusted []string) bool {
	if trusted == nil {
		return true
	}
	ip = ip.Unmap()
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
			if prefix.Contains(ip) {
				return true
			}
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			continue
		}
		if addr == ip {
			return true
		}
	}
	return false
}

// trustedProxyList returns s.cfg().HTTP.TrustedProxies. Pulled through a
// method indirection so tests can stub it (the package-level
// httpConfigProvider is a function variable below).
var trustedProxyList = func() []string {
	// populated in New(); default nil = trust all (backward compat).
	return nil
}

func splitCookieValue(v string) (cookieID, hmacHex string, err error) {
	dot := strings.IndexByte(v, '.')
	if dot < 0 || dot == 0 || dot == len(v)-1 {
		return "", "", errors.New("malformed cookie value")
	}
	return v[:dot], v[dot+1:], nil
}

func formatRetryAfter(d time.Duration) string {
	secs := int(d.Seconds() + 0.5)
	if secs < 1 {
		secs = 1
	}
	return strconvItoa(secs)
}

// strconvItoa avoids pulling strconv into this file's import block
// for one use; storage_locations_test.go also defines an itoa for
// its own tests, so this name is package-unique.
func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// writeJSON is the localAuth file's small JSON helper. Avoids colliding
// with web_upload.go's writeJSONError (different shapes: writeJSON
// here returns a 200 body; writeJSONError elsewhere writes an error).
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"$schema": "https://huma.rocks/schema/error.json",
		"title":   http.StatusText(status),
		"status":  status,
		"detail":  detail,
	})
}

// Ensure the unused-import linter is happy for ratelimit (used in
// Decision construction above). Cheap insurance for older Go versions.
var _ ratelimit.Decision

// sync import guard
var _ = sync.Mutex{}
