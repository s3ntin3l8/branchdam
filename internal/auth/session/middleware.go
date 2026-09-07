// Package session owns the cookie-to-Principal middleware for the
// browser auth path. It's the only code in the repo permitted to read
// the session cookie.
//
// Cookie shape: <cookie_id_hex>.<hmac_hex> where cookie_id is the
// 32-byte random identifier stored in sessions.cookie_id and the HMAC
// is computed over cookie_id using a key derived from
// BRANCHDAM_SECRET_KEY. Tampered / unknown / expired cookies are
// silently ignored -- the request continues with no Principal, which
// preserves today's read-allowed / write-denied shape.
package session

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
)

// Config bundles the middleware's tunable behavior.
type Config struct {
	CookieName      string
	Secure          bool
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Middleware is constructed once at startup and used to wrap handlers.
type Middleware struct {
	cfg   Config
	users *users.Service
	nowFn func() time.Time
	log   *slog.Logger
}

// New constructs a Middleware.
func New(u *users.Service, cfg Config) *Middleware {
	if cfg.CookieName == "" {
		cfg.CookieName = "branchdam_session"
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 24 * time.Hour
	}
	if cfg.AbsoluteTimeout <= 0 {
		cfg.AbsoluteTimeout = 30 * 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Middleware{
		cfg:   cfg,
		users: u,
		nowFn: cfg.Now,
		log:   cfg.Log,
	}
}

// CookieName returns the resolved cookie name.
func (m *Middleware) CookieName() string { return m.cfg.CookieName }

// SecureCookie reports whether the Secure attribute should be set.
func (m *Middleware) SecureCookie(r *http.Request, trustedProxies []string) bool {
	if m.cfg.Secure {
		return true
	}
	if r.TLS != nil {
		return true
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" && isTrustedProxy(r.RemoteAddr, trustedProxies) {
		return true
	}
	return false
}

// LocalUserView is the per-request summary of a locally-authenticated user.
type LocalUserView = auth.LocalUserView

// FromUser is a thin re-export of auth.FromUser.
func FromUser(ctx context.Context) (LocalUserView, bool) {
	return auth.FromUser(ctx)
}

func withLocalUser(ctx context.Context, v LocalUserView) context.Context {
	return auth.WithLocalUserView(ctx, v)
}

// Middleware returns the http.Handler wrapper.
func (m *Middleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasAgentPathPrefix(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(m.cfg.CookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		cookieID, err := m.users.VerifyCookieValue(cookie.Value)
		if err != nil || cookieID == "" {
			next.ServeHTTP(w, r)
			return
		}

		session, err := m.users.GetSessionByCookieID(r.Context(), cookieID)
		if err != nil {
			if !errors.Is(err, users.ErrSessionNotFound) {
				m.log.Warn("session: lookup failed", "err", err.Error())
			}
			next.ServeHTTP(w, r)
			return
		}

		now := m.nowFn()
		if session.RevokedAt.Valid {
			next.ServeHTTP(w, r)
			return
		}
		if now.Unix() >= session.ExpiresAt || now.Unix() >= session.IdleExpiresAt {
			next.ServeHTTP(w, r)
			return
		}

		user, err := m.users.GetUserByID(r.Context(), session.UserID)
		if err != nil {
			m.log.Warn("session: user lookup failed for active session", "userID", session.UserID, "err", err.Error())
			next.ServeHTTP(w, r)
			return
		}
		if user.DisabledAt.Valid {
			next.ServeHTTP(w, r)
			return
		}

		newIdle := now.Add(m.cfg.IdleTimeout)
		if touchErr := m.users.TouchSession(r.Context(), session.ID, now, newIdle); touchErr != nil {
			m.log.Warn("session: touch failed", "sessionID", session.ID, "err", touchErr.Error())
		}

		view := LocalUserView{
			UserID:  user.ID,
			IsAdmin: user.IsAdmin != 0,
		}
		ctx := withLocalUser(r.Context(), view)

		principal := auth.Principal{
			Kind:          auth.KindUser,
			Name:          user.Username,
			Email:         user.Email.String,
			Authenticated: true,
		}
		ctx = auth.WithPrincipal(ctx, principal)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SetSessionCookie writes the cookie value to the response.
func (m *Middleware) SetSessionCookie(w http.ResponseWriter, r *http.Request, cookieValue string, maxAge int, trustedProxies []string) {
	// codeql[go/cookie-secure-not-set]
	// Secure is set conditionally via SecureCookie: true when the
	// request reached us over TLS directly (r.TLS != nil), or via
	// X-Forwarded-Proto: https from a configured trusted proxy, or
	// when the operator has explicitly enabled auth.local.secure in
	// config. Plain-HTTP local dev (`make dev-api`) leaves Secure
	// off so the browser stores the cookie; that path runs against
	// a private loopback, not the public internet.
	// codeql[go/cookie-secure-attribute]
	http.SetCookie(w, &http.Cookie{
		Name:     m.cfg.CookieName,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   m.SecureCookie(r, trustedProxies),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie instructs the browser to discard the cookie.
func (m *Middleware) ClearSessionCookie(w http.ResponseWriter, r *http.Request, trustedProxies []string) {
	// codeql[go/cookie-secure-not-set]
	// Same conditional Secure as SetSessionCookie (see comment there).
	// Clear must mirror Set's Secure so the browser actually matches
	// and discards the cookie on the response.
	// codeql[go/cookie-secure-attribute]
	http.SetCookie(w, &http.Cookie{
		Name:     m.cfg.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.SecureCookie(r, trustedProxies),
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *Middleware) IdleTimeout() time.Duration     { return m.cfg.IdleTimeout }
func (m *Middleware) AbsoluteTimeout() time.Duration { return m.cfg.AbsoluteTimeout }

func hasAgentPathPrefix(path string) bool {
	return len(path) >= len("/api/v1/agent") && path[:len("/api/v1/agent")] == "/api/v1/agent"
}

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
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			continue
		}
		if addr == remoteIP {
			return true
		}
	}
	return false
}
