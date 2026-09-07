package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/ratelimit"
	"github.com/s3ntin3l8/branchdam/internal/auth/session"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// errSetupComplete is the sentinel the setup handler returns when
// the count-check inside its InTx closure finds users already exist.
// Translated to 404 at the handler boundary.
var errSetupComplete = errors.New("setup already complete")

// localAuthHandlers bundles the wiring the local-auth endpoints need.
// Constructed once at startup in registerLocalAuthRoutes; carries the
// user service, login rate limiter, and session middleware (which
// knows the cookie name + timeouts).
type localAuthHandlers struct {
	users        *users.Service
	loginLimiter *ratelimit.Limiter
	resetLimiter *ratelimit.Limiter
	sessionMw    *session.Middleware
	reset        *users.PasswordResetService
	log          *slog.Logger
	authMode     auth.AuthMode
	jit          auth.JITProvisioner
}

// registerLocalAuthRoutes mounts /api/v1/setup/status,
// /api/v1/setup/admin, /api/v1/login, and DELETE /api/v1/session
// directly on the mux (Huma's response model is JSON-only; these
// endpoints must write Set-Cookie headers, which Huma's response
// pipeline doesn't expose cleanly). The password-reset endpoints
// (PR #409) mount in the same file because they share the rate-
// limiter and the IP-extraction logic; see password_reset.go.
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
		mux.HandleFunc("POST /api/v1/password-reset/request", s.handlePasswordResetRequestNoLocal)
		mux.HandleFunc("POST /api/v1/password-reset/confirm", s.handlePasswordResetConfirmNoLocal)
		mux.HandleFunc("POST /api/v1/admin/users/{id}/reset-password", s.handleAdminResetPasswordNoLocal)
		return
	}
	mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/setup/admin", s.handleSetupAdmin)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("DELETE /api/v1/session", s.handleLogout)
	mux.HandleFunc("POST /api/v1/password-reset/request", s.handlePasswordResetRequest)
	mux.HandleFunc("POST /api/v1/password-reset/confirm", s.handlePasswordResetConfirm)
	mux.HandleFunc("POST /api/v1/admin/users/{id}/reset-password", s.handleAdminResetPassword)
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

	// TOCTOU-safe setup (Hermes #407 review): wrap the count-check +
	// create in a single write transaction. The writer pool has
	// SetMaxOpenConns(1) (AGENTS.md invariant #2), which alone
	// serializes the two queries, but doing the whole sequence inside
	// one tx makes the atomicity obvious in the code and survives any
	// future move to a multi-writer pool. The setup token is implicit:
	// the count being zero IS the token.
	var userID int64
	now := time.Now()
	err := s.db.InTx(r.Context(), func(q *sqlcgen.Queries) error {
		count, err := q.CountUsers(r.Context())
		if err != nil {
			return fmt.Errorf("count users: %w", err)
		}
		if count > 0 {
			return errSetupComplete
		}
		hash, err := s.localAuth.users.HashPassword(body.Password)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		var emailNS sql.NullString
		if email != "" {
			emailNS = sql.NullString{String: email, Valid: true}
		}
		created, err := q.CreateLocalUser(r.Context(), sqlcgen.CreateLocalUserParams{
			Username:     username,
			Email:        emailNS,
			PasswordHash: sql.NullString{String: hash, Valid: true},
			IsAdmin:      1,
			CreatedAt:    now.Unix(),
			CreatedBy:    "setup",
		})
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		userID = created.ID
		return nil
	})
	if errors.Is(err, errSetupComplete) {
		writeJSONError(w, http.StatusNotFound, "setup already complete")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
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
	_, err = s.localAuth.users.CreateSession(r.Context(), userID, cookieID, clientIP(s, r), r.UserAgent(), now.Add(s.localAuth.sessionMw.AbsoluteTimeout()), now.Add(s.localAuth.sessionMw.IdleTimeout()))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: userID, Valid: true}, username, "local", "ok", clientIP(s, r), r.UserAgent(), "{}")

	maxAge := int(s.localAuth.sessionMw.AbsoluteTimeout().Seconds())
	s.localAuth.sessionMw.SetSessionCookie(w, r, cookieValue, maxAge, s.cfg().HTTP.TrustedProxies)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- /api/v1/login ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.localAuth.authMode == auth.AuthModeForward {
		writeJSONError(w, http.StatusConflict, "login form is only available when auth.mode is 'local' or 'both'")
		return
	}

	ip := clientIP(s, r)
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
	s.localAuth.sessionMw.SetSessionCookie(w, r, cookieValue, maxAge, s.cfg().HTTP.TrustedProxies)

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
	s.localAuth.sessionMw.ClearSessionCookie(w, r, s.cfg().HTTP.TrustedProxies)
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
func (s *Server) handlePasswordResetRequestNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handlePasswordResetConfirmNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handleAdminResetPasswordNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}

// --- helpers ---

// clientIP returns the originating client IP. Honors X-Forwarded-For
// ONLY when (a) a non-empty http.trustedProxies list is configured AND
// (b) the direct connection came from one of those proxies. When no
// proxies are configured, falls back to r.RemoteAddr verbatim -- the
// X-Forwarded-For header is meaningless without a proxy in front, and
// trusting it unconditionally would let a direct client spoof the
// source IP and bypass the /login rate limiter.
//
// Wired through the Server so the live-resolved config is consulted.
func clientIP(s *Server, r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	trusted := currentTrustedProxies(s)
	// Hermes Critical (#407): when no proxies are configured, the
	// X-Forwarded-For header MUST be ignored. isTrustedProxy's nil =>
	// true behavior is a backward-compat default for header-rewriting
	// features (requestOrigin, Secure-cookie auto-detection) that
	// degrade gracefully on misconfiguration; the rate limiter cannot
	// afford that default -- an attacker who sets the header directly
	// to a rotating IP would otherwise bypass the brute-force control.
	if len(trusted) == 0 {
		return remoteIP.String()
	}
	if !isTrustedProxy(remoteIP.String(), trusted) {
		return remoteIP.String()
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// First entry is the original client; trailing entries are
		// additional proxies that received the request, ignored.
		if comma := strings.IndexByte(fwd, ','); comma >= 0 {
			fwd = fwd[:comma]
		}
		return strings.TrimSpace(fwd)
	}
	return remoteIP.String()
}

// currentTrustedProxies reads s.cfg().HTTP.TrustedProxies through a
// thin indirection so tests can stub it without exporting a mutable
// package variable. Nil/empty is intentional: it means "no proxy in
// front" and triggers the safe default in clientIP (no header trust).
func currentTrustedProxies(s *Server) []string {
	if s == nil {
		return nil
	}
	cfg := s.cfg()
	if cfg == nil {
		return nil
	}
	return cfg.HTTP.TrustedProxies
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
	return strconv.Itoa(secs)
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
