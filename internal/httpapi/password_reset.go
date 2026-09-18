// Package httpapi is branchDAM's HTTP surface: middleware chain, the
// Huma-generated REST API, the SSE progress stream, and the embedded SPA
// fallback.
//
// Password-reset HTTP handlers (PR #409). Three endpoints:
//
//	POST /api/v1/password-reset/request   -- no auth, 200 always
//	POST /api/v1/password-reset/confirm   -- no auth, 200 / 404
//	POST /api/v1/admin/users/{id}/reset-password   -- admin-only, returns new password
//
// The /request handler emits a slog.WARN with only numeric fields
// (user_id, token_id, expires_at). The plaintext token and the
// request IP/User-Agent are deliberately NOT logged here. The
// plaintext token is delivered to admins via the admin-UI pending-
// resets panel (PR #408) and via the admin reset-password endpoint's
// response body, so logging it in slog would only widen the attack
// surface for log exfiltration. Request IP/UA flow into the audit
// row written by RequestPasswordReset; they are user-controlled and
// must never cross into a slog line (CodeQL go/log-injection).

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	emailpkg "github.com/s3ntin3l8/branchdam/internal/email"
)

// handlePasswordResetRequest: POST /api/v1/password-reset/request
// body: {"email": "..."}
//
// Always returns 200 OK with an empty body. On a user-found, a token
// is minted and an admin-only slog marker is written (numeric fields
// only -- user_id, token_id, expires_at). The plaintext token does
// NOT cross into slog; the response body does NOT include the token.
// Operators/admins retrieve the token via the admin-UI pending-resets
// panel (PR #408). On a no-such-user, no token is minted, no log
// line is written -- the response is byte-identical to the success
// path so the endpoint cannot be used to enumerate which addresses
// have accounts.
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
	// Operator-facing: emit a token-minted marker to slog so journalctl
	// picks it up. Only numeric fields (user_id, token_id, expires_at)
	// are logged: the plaintext token and request IP/User-Agent are
	// omitted deliberately. The plaintext token is surfaced to admins
	// via the admin-UI pending-resets panel (PR #408) and via the
	// admin reset-password endpoint's newPassword response body, so
	// logging it here would only widen the attack surface for log
	// exfiltration. Request IP/UA flow into the audit row written by
	// RequestPasswordReset; they are user-controlled and CodeQL's
	// go/log-injection rule flags them in slog output regardless of
	// the lgtm suppression.
	// Go's typed slog fields (Int64, Int) make it clear to CodeQL
	// that these are numeric, not user-controlled strings.
	s.localAuth.log.Warn("password-reset: token minted (operator: retrieve via admin pending-resets panel)",
		slog.Int64("user_id", issue.Token.UserID),
		slog.Int64("token_id", issue.Token.ID),
		slog.Int64("expires_at", issue.ExpiresAt.Unix()),
	)

	// Email delivery: when the notifier is configured, send the reset
	// link to the user's email address. The notifier is nil when
	// auth.email.provider is unset (default "log"), which silently
	// skips delivery. Enumeration defense: email is only sent when a
	// token was minted (user found), and the 200 response is identical.
	//
	// Delivery is performed in a background goroutine so the 200
	// response is returned without blocking on the SMTP round-trip.
	// Without this, an attacker could distinguish "user exists" (full
	// SMTP round-trip before 200) from "user does not exist" (200
	// immediately), defeating the byte-identical response defense.
	// Errors are logged from the goroutine; the caller never sees
	// them. We copy every value the goroutine needs into locals
	// (request context is canceled when the handler returns, so we
	// cannot share r, user, issue with the goroutine).
	if s.localAuth.email != nil {
		user, lookupErr := s.localAuth.users.GetUserByID(r.Context(), issue.Token.UserID)
		if lookupErr == nil && user.Email.Valid && user.Email.String != "" {
			baseURL := passwordResetBaseURL(s, r)
			resetLink := fmt.Sprintf("%s/password-reset?token=%s", baseURL, issue.PlaintextToken)
			subject := "Reset your branchDAM password"
			username := user.Username
			recipient := user.Email.String
			expiresLabel := issue.ExpiresAt.Format("15:04 UTC, Mon Jan 2")
			userID := issue.Token.UserID
			htmlBody := emailpkg.PasswordResetHTML(emailpkg.ResetEmailData{
				Username:  username,
				ResetLink: resetLink,
				ExpiresAt: expiresLabel,
			})
			textBody := emailpkg.PasswordResetText(emailpkg.ResetEmailData{
				Username:  username,
				ResetLink: resetLink,
				ExpiresAt: expiresLabel,
			})
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), emailpkg.SendTimeout)
				defer cancel()
				if sendErr := s.localAuth.email.Send(ctx, recipient, subject, htmlBody, textBody); sendErr != nil {
					s.localAuth.log.Warn("password-reset: email delivery failed",
						slog.Int64("user_id", userID),
						slog.String("error", sendErr.Error()),
					)
				}
			}()
		}
	}

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
// (admin-only, gated by requireSettingsAdmin)
//
// Rotates the target user's password hash and returns the new
// plaintext exactly once. The response is the ONLY place the
// plaintext exists; the database stores only the argon2id hash.
// Caller is expected to copy the password out of the response
// before navigating away -- the SPA "copy now or we won't show it
// again" pattern (PR #408 builds the admin UI panel that surfaces
// this).
func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	if err := s.requireSettingsAdmin(r.Context()); err != nil {
		var statusErr huma.StatusError
		if errors.As(err, &statusErr) {
			writeJSONError(w, statusErr.GetStatus(), statusErr.Error())
		} else {
			writeJSONError(w, http.StatusForbidden, err.Error())
		}
		return
	}
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
