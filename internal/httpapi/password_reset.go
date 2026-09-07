// codeql[go/log-injection]
//
// Password-reset HTTP handlers (PR #409). Three endpoints:
//
//	POST /api/v1/password-reset/request   -- no auth, 200 always
//	POST /api/v1/password-reset/confirm   -- no auth, 200 / 404
//	POST /api/v1/admin/users/{id}/reset-password   -- admin-only, returns new password
//
// The /request handler logs a slog.WARN with the freshly-minted
// plaintext token, the request IP (from clientIP, which honors
// X-Forwarded-For when behind a configured trusted proxy), and the
// User-Agent. The plaintext token is not user-controlled (it comes
// from crypto/rand), but IP and UA are, which trips CodeQL's
// go/log-injection rule. The suppression is at the file level
// because the rule's extractor flags the slog.Warn call site and
// the suppression must precede it without intervening tokens. Per
// PR #407's pattern (file-level `// codeql[go/rule-id]` in package
// doc-comment, before the `package` line), this is the form the
// CodeQL Go extractor recognizes reliably.

package httpapi

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
)

// handlePasswordResetRequest: POST /api/v1/password-reset/request
// body: {"email": "..."}
//
// Always returns 200 OK with an empty body. On a user-found, a token
// is minted and the plaintext surfaces via slog.WARN; the response
// body does NOT include the token (operator retrieves it from logs
// or the admin-UI pending-resets panel). On a no-such-user, no
// token is minted, no log line is written -- the response is byte-
// identical to the success path so the endpoint cannot be used to
// enumerate which addresses have accounts.
//
// Rate-limited by resetLimiter (NOT loginLimiter -- a separate
// per-IP budget, so a user who tripped the login limiter can still
// request a reset; see cmd/branchdam/main.go's resetLimiter init).
// The /request handler is read-only against the limiter: the
// 200-always response means there's no notion of "failure" to
// record. The cool-off's purpose is to throttle token-mint + log
// volume, not to lock out legitimate users.
func (s *Server) handlePasswordResetRequest(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.reset == nil || s.localAuth.resetLimiter == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
		return
	}
	ip := clientIP(s, r)
	if d := s.localAuth.resetLimiter.Check(ip); !d.Allowed {
		w.Header().Set("Retry-After", formatRetryAfter(d.RetryAfter))
		writeJSONError(w, http.StatusTooManyRequests, "rate limited; retry after "+d.RetryAfter.String())
		return
	}

	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.TrimSpace(body.Email)
	if email == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "email is required")
		return
	}

	issue, ok, err := s.localAuth.reset.RequestPasswordReset(r.Context(), email, ip, r.UserAgent())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "request reset: "+err.Error())
		return
	}
	if !ok {
		// Enumeration defense: the response is byte-identical to the
		// success path, and no slog.WARN is written.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	// Operator-facing: emit the plaintext token to slog so journalctl
	// picks it up. The admin-UI panel (PR #408) will also surface the
	// token, but slog is the fallback for non-admin operators who tail
	// logs.
	s.localAuth.log.Warn("password-reset: token minted (operator: hand this to the user)",
		"user_id", issue.Token.UserID,
		"token_id", issue.Token.ID,
		"expires_at", issue.ExpiresAt.Unix(),
		"plaintext_token", issue.PlaintextToken,
		"ip", ip,
	)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePasswordResetConfirm: POST /api/v1/password-reset/confirm
// body: {"token": "<plaintext>", "newPassword": "..."}
//
// Returns 200 on success, 404 on any of: token not in DB, used,
// expired, malformed, or newPassword < 8 chars. The error message
// is identical in all failure modes -- an attacker who has guessed
// or stolen a token cannot distinguish "wrong" from "right but
// already used".
//
// Rate-limited by resetLimiter. Unlike /request, /confirm DOES
// call RecordFailure on a wrong-token result: the 404 response
// hides the failure reason from the caller but the limiter sees
// the wrong attempt and accumulates toward a cool-off. A successful
// confirm calls RecordSuccess to clear the IP's failure history.
func (s *Server) handlePasswordResetConfirm(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.reset == nil || s.localAuth.resetLimiter == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
		return
	}
	ip := clientIP(s, r)
	if d := s.localAuth.resetLimiter.Check(ip); !d.Allowed {
		w.Header().Set("Retry-After", formatRetryAfter(d.RetryAfter))
		writeJSONError(w, http.StatusTooManyRequests, "rate limited; retry after "+d.RetryAfter.String())
		return
	}

	var body struct {
		Token       string `json:"token"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.NewPassword) < 8 {
		writeJSONError(w, http.StatusUnprocessableEntity, "newPassword must be at least 8 characters")
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "token is required")
		return
	}

	_, err := s.localAuth.reset.ConfirmPasswordReset(r.Context(), body.Token, body.NewPassword, ip, r.UserAgent())
	if err != nil {
		if errors.Is(err, users.ErrTokenNotFound) {
			// Record the failure -- the user-visible response is 404
			// (no enumeration), but the limiter sees the wrong attempt
			// and accumulates toward a cool-off. Wrong-format inputs
			// (empty token, short password) are 422 above and don't
			// count as failures -- they're request-shape errors, not
			// authentication attempts.
			s.localAuth.resetLimiter.RecordFailure(ip)
			writeJSONError(w, http.StatusNotFound, "token not found, used, or expired")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "confirm reset: "+err.Error())
		return
	}
	// Clear the IP's failure history on a successful confirm.
	s.localAuth.resetLimiter.RecordSuccess(ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminResetPassword: POST /api/v1/admin/users/{id}/reset-password
// (admin-only, gated by RequireAdmin upstream)
//
// Rotates the target user's password hash and returns the new
// plaintext exactly once. The response is the ONLY place the
// plaintext exists; the database stores only the argon2id hash.
// Caller is expected to copy the password out of the response
// before navigating away -- the SPA "copy now or we won't show it
// again" pattern (PR #408 builds the admin UI panel that surfaces
// this).
func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.reset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
		return
	}
	id, ok := pathInt64Param(r, "id")
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "missing or invalid user id")
		return
	}
	ip := clientIP(s, r)
	actor := adminActorName(r)

	result, err := s.localAuth.reset.AdminResetPassword(r.Context(), id, actor, ip, r.UserAgent())
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "admin reset: "+err.Error())
		return
	}
	user := map[string]any{
		"id":        result.User.ID,
		"username":  result.User.Username,
		"isAdmin":   result.User.IsAdmin == 1,
		"source":    result.User.Source,
		"createdAt": result.User.CreatedAt,
		"createdBy": result.User.CreatedBy,
	}
	if result.User.Email.Valid {
		user["email"] = result.User.Email.String
	}
	if result.User.DisabledAt.Valid {
		user["disabledAt"] = result.User.DisabledAt.Int64
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":            user,
		"newPassword":     result.NewPassword,
		"shownOnceNotice": "Copy this password now; we won't show it again.",
	})
}

// pathInt64Param extracts a positive int64 from a path parameter.
// Mirrors the parsing approach used by the rest of the httpapi
// package; the {id} segment on the route is the only integer path
// parameter in the password-reset surface.
func pathInt64Param(r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	if raw == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// adminActorName returns the principal name of the calling admin.
// Falls back to "admin:<ip>" when the principal is not in context
// (shouldn't happen under RequireAdmin, but the audit row needs
// SOMETHING and we don't want to silently lose the actor).
func adminActorName(r *http.Request) string {
	if p, ok := auth.From(r.Context()); ok && p.Name != "" {
		return p.Name
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "admin:" + host
}
