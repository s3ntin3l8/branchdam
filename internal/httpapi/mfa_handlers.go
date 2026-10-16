// lgtm[go/log-injection]
//
// MFA HTTP handlers (PR #459, TOTP-based MFA for local users). Four
// endpoints:
//
//	POST /api/v1/mfa/setup      -- session user, sets pending secret
//	POST /api/v1/mfa/enable     -- session user, promotes pending->active
//	                               and marks the calling session verified
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
	"errors"
	"net/http"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/qr"
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
	// qrSvg renders the same otpauth URI as a scannable QR code, so the
	// SPA can show it without a client-side QR library. It carries
	// exactly the secret already in otpauthURI, in the same response;
	// nothing is persisted or logged. A render failure only drops the
	// QR -- the URI is still enough to enrol by hand.
	body := map[string]any{
		"otpauthURI": result.OtpauthURI,
		"asciiQR":    result.ASCIIQR,
	}
	if svg, err := qr.RenderSVG(result.OtpauthURI, 0); err == nil {
		body["qrSvg"] = string(svg)
	}
	writeJSON(w, http.StatusOK, body)
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
	if err := decodeSmallJSON(w, r, &body); err != nil {
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
	// The caller just proved possession of the new second factor, so
	// the session they enrolled from counts as MFA-verified. Without
	// this, the session that enrolled is half-authed from the next
	// request on: MFAGate blocks every API call and the SPA swaps the
	// page for the challenge form while the one-time recovery codes are
	// still on screen. MFA is already enabled at this point, so a
	// failure here is only logged and the codes are still returned --
	// they are the only copy, and the worst case is one extra challenge.
	if err := s.markSessionMFAVerified(r); err != nil {
		s.log.Warn("mfa enable: could not mark the enrolling session verified", "userID", localUser.UserID, "err", err.Error())
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
		d, release := s.localAuth.mfaDisableLimiter.Begin(ip)
		defer release()
		if !d.Allowed {
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
	if err := decodeSmallJSON(w, r, &body); err != nil {
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
		d, release := s.localAuth.mfaChallengeLimiter.Begin(ip)
		defer release()
		if !d.Allowed {
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
	if err := decodeSmallJSON(w, r, &body); err != nil {
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
	if err := s.markSessionMFAVerified(r); err != nil {
		var se *sessionLookupError
		if errors.As(err, &se) {
			writeJSONError(w, http.StatusUnauthorized, se.msg)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "mark MFA verified: "+err.Error())
		return
	}

	s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: localUser.UserID, Valid: true}, "", source, "ok", ip, r.UserAgent(), "{}")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sessionLookupError is markSessionMFAVerified's "the request carries no
// usable session" failure (the challenge handler maps it to 401), as
// opposed to a database error writing the verified stamp (500).
type sessionLookupError struct{ msg string }

func (e *sessionLookupError) Error() string { return e.msg }

// markSessionMFAVerified stamps mfa_verified_at on the session the
// request's cookie names. Used by the challenge (second factor answered
// at login) and by enable (second factor proven while enrolling).
func (s *Server) markSessionMFAVerified(r *http.Request) error {
	cookie, err := r.Cookie(s.localAuth.sessionMw.CookieName())
	if err != nil || cookie.Value == "" {
		return &sessionLookupError{msg: "no session cookie"}
	}
	cookieID, err := s.localAuth.users.VerifyCookieValue(cookie.Value)
	if err != nil || cookieID == "" {
		return &sessionLookupError{msg: "invalid session cookie"}
	}
	session, err := s.localAuth.users.GetSessionByCookieID(r.Context(), cookieID)
	if err != nil {
		return &sessionLookupError{msg: "session not found"}
	}
	return s.localAuth.users.SetSessionMFAVerified(r.Context(), session.ID)
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
