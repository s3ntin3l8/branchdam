// lgtm[go/log-injection]
//
// MFA HTTP handlers (PR #459, TOTP-based MFA for local users). Four
// endpoints:
//
//	POST /api/v1/mfa/setup      -- session user, sets pending secret
//	POST /api/v1/mfa/enable     -- session user, promotes pending->active
//	POST /api/v1/mfa/disable    -- session user, requires password + TOTP
//	POST /api/v1/mfa/challenge  -- half-auth session, TOTP or recovery code
//
// All four consume user-provided JSON bodies and propagate the request
// IP + User-Agent through WriteLoginAudit (login_audit table). The
// audit write is a DB row, not a slog call, so this file has no direct
// log-injection sink -- but CodeQL's go/log-injection rule can flag the
// flow as if it were a sink. The suppression is at the file level,
// matching the password_reset.go pattern, so future additions of slog
// calls that intentionally log user-provided values (e.g. operator-
// facing rate-limit warnings with the IP) don't re-trigger the rule.

package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

func (s *Server) registerMFARoutes(mux *http.ServeMux) {
	if s.localAuth == nil {
		mux.HandleFunc("POST /api/v1/mfa/setup", s.handleMFASetupNoLocal)
		mux.HandleFunc("POST /api/v1/mfa/enable", s.handleMFAEnableNoLocal)
		mux.HandleFunc("POST /api/v1/mfa/disable", s.handleMFADisableNoLocal)
		mux.HandleFunc("POST /api/v1/mfa/challenge", s.handleMFAChallengeNoLocal)
		return
	}
	mux.HandleFunc("POST /api/v1/mfa/setup", s.handleMFASetup)
	mux.HandleFunc("POST /api/v1/mfa/enable", s.handleMFAEnable)
	mux.HandleFunc("POST /api/v1/mfa/disable", s.handleMFADisable)
	mux.HandleFunc("POST /api/v1/mfa/challenge", s.handleMFAChallenge)
}

func (s *Server) handleMFASetup(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.mfa == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "MFA is not configured")
		return
	}
	localUser, ok := auth.FromUser(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	user, err := s.localAuth.users.GetUserByID(r.Context(), localUser.UserID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "get user: "+err.Error())
		return
	}
	if s.localAuth.mfa.HasMFA(r.Context(), localUser.UserID) {
		writeJSONError(w, http.StatusConflict, "MFA is already enabled; disable it first")
		return
	}
	result, err := s.localAuth.mfa.Setup(r.Context(), localUser.UserID, user.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "setup MFA: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"otpauthURI": result.OtpauthURI,
		"asciiQR":    result.ASCIIQR,
	})
}

func (s *Server) handleMFAEnable(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.mfa == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "MFA is not configured")
		return
	}
	localUser, ok := auth.FromUser(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "code is required")
		return
	}
	codes, err := s.localAuth.mfa.Enable(r.Context(), localUser.UserID, code)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "enable MFA: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"recoveryCodes": codes,
		"notice":        "Save these recovery codes in a safe place. They will not be shown again.",
	})
}

func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.mfa == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "MFA is not configured")
		return
	}
	// Rate-limit per-IP (PR #459 review follow-up): the endpoint takes
	// password + 6-digit TOTP, and a half-auth session has already
	// proven the password. Without throttling, a password-holder could
	// brute-force the TOTP and permanently remove MFA. Same defaults
	// as mfaChallengeLimiter (5/min/IP -> 60s cool-off) so operators
	// have a single mental model for MFA endpoint throttling.
	ip := clientIP(s, r)
	if s.localAuth.mfaDisableLimiter != nil {
		if d := s.localAuth.mfaDisableLimiter.Check(ip); !d.Allowed {
			w.Header().Set("Retry-After", formatRetryAfter(d.RetryAfter))
			writeJSONError(w, http.StatusTooManyRequests, "rate limited; retry after "+d.RetryAfter.String())
			return
		}
	}
	localUser, ok := auth.FromUser(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	password := body.Password
	code := strings.TrimSpace(body.Code)
	if password == "" || code == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "password and code are required")
		return
	}
	verifyPassword := func(hash string) error {
		return s.localAuth.users.VerifyPassword(password, hash)
	}
	if err := s.localAuth.mfa.Disable(r.Context(), localUser.UserID, verifyPassword, code); err != nil {
		if s.localAuth.mfaDisableLimiter != nil {
			s.localAuth.mfaDisableLimiter.RecordFailure(ip)
		}
		writeJSONError(w, http.StatusBadRequest, "disable MFA: "+err.Error())
		return
	}
	if s.localAuth.mfaDisableLimiter != nil {
		s.localAuth.mfaDisableLimiter.RecordSuccess(ip)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMFAChallenge(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.mfa == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "MFA is not configured")
		return
	}
	// Rate-limit per-IP (Issue 2): a 6-digit TOTP with +/-1 drift
	// gives ~333k guesses per step, so a password-holder who reaches
	// this endpoint could otherwise brute-force. 5 attempts per
	// minute matches the /login fast threshold (same Limiter with
	// the default Config); the rate limiter is read-only at /check
	// time, and the handler records a failure on every wrong code.
	ip := clientIP(s, r)
	if s.localAuth.mfaChallengeLimiter != nil {
		if d := s.localAuth.mfaChallengeLimiter.Check(ip); !d.Allowed {
			w.Header().Set("Retry-After", formatRetryAfter(d.RetryAfter))
			writeJSONError(w, http.StatusTooManyRequests, "rate limited; retry after "+d.RetryAfter.String())
			return
		}
	}
	localUser, ok := auth.FromUser(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Check if session is half-authenticated (MFA not yet verified).
	if localUser.MFAVerified {
		writeJSONError(w, http.StatusBadRequest, "MFA already verified for this session")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "code is required")
		return
	}

	// Try TOTP first, then recovery code.
	valid, err := s.localAuth.mfa.ValidateTOTPCode(r.Context(), localUser.UserID, code)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "validate TOTP: "+err.Error())
		return
	}
	source := "mfa-challenge"
	outcome := "mfa-invalid"
	if !valid {
		valid, err = s.localAuth.mfa.ValidateRecoveryCode(r.Context(), localUser.UserID, code)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "validate recovery code: "+err.Error())
			return
		}
		if valid {
			outcome = "mfa-recovery-used"
		}
	}
	if !valid {
		if s.localAuth.mfaChallengeLimiter != nil {
			s.localAuth.mfaChallengeLimiter.RecordFailure(ip)
		}
		s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: localUser.UserID, Valid: true}, "", source, outcome, ip, r.UserAgent(), "{}")
		writeJSONError(w, http.StatusUnauthorized, "invalid code")
		return
	}
	if s.localAuth.mfaChallengeLimiter != nil {
		s.localAuth.mfaChallengeLimiter.RecordSuccess(ip)
	}

	// Mark session as MFA-verified.
	cookie, err := r.Cookie(s.localAuth.sessionMw.CookieName())
	if err != nil || cookie.Value == "" {
		writeJSONError(w, http.StatusUnauthorized, "no session cookie")
		return
	}
	cookieID, err := s.localAuth.users.VerifyCookieValue(cookie.Value)
	if err != nil || cookieID == "" {
		writeJSONError(w, http.StatusUnauthorized, "invalid session cookie")
		return
	}
	session, err := s.localAuth.users.GetSessionByCookieID(r.Context(), cookieID)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "session not found")
		return
	}
	if err := s.localAuth.users.SetSessionMFAVerified(r.Context(), session.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mark MFA verified: "+err.Error())
		return
	}

	s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: localUser.UserID, Valid: true}, "", source, "ok", ip, r.UserAgent(), "{}")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMFASetupNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handleMFAEnableNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handleMFADisableNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
func (s *Server) handleMFAChallengeNoLocal(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusServiceUnavailable, "local auth is not configured")
}
