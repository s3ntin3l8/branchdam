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
		writeJSONError(w, http.StatusBadRequest, "disable MFA: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMFAChallenge(w http.ResponseWriter, r *http.Request) {
	if s.localAuth == nil || s.localAuth.mfa == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "MFA is not configured")
		return
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
		s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: localUser.UserID, Valid: true}, "", source, outcome, clientIP(s, r), r.UserAgent(), "{}")
		writeJSONError(w, http.StatusUnauthorized, "invalid code")
		return
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

	s.localAuth.users.WriteLoginAudit(r.Context(), sql.NullInt64{Int64: localUser.UserID, Valid: true}, "", source, "ok", clientIP(s, r), r.UserAgent(), "{}")
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
