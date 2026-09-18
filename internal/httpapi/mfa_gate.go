package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/mfa"
)

// MFAGate returns a middleware that enforces MFA verification
// server-side for local users with MFA enrolled (review feedback on
// PR #459, Issue 1).
//
// Without this gate, /login issues a full-privilege session even when
// the user has MFA enrolled: the password-only path bypasses TOTP
// entirely, and LocalUserView.MFAVerified sits on the session row
// unused. The gate closes that gap.
//
// Behaviour matrix:
//
//	view.UserID == 0 (no local session)         -> allow (no MFA to enforce)
//	view.MFAVerified == true                    -> allow (already verified)
//	!user.HasMFA()                              -> allow (no MFA enrolled)
//	user has MFA enrolled, MFAVerified == false -> restrict to allowlist
//
// When restricted, paths under /api/v1/* NOT in mfaGateAllowlist
// return 403 with {"mfaChallengeRequired": true} so the SPA can branch
// to the MFA challenge page. Non-/api paths (the SPA shell + assets)
// are always allowed so the SPA itself can load and render the
// challenge UI.
func MFAGate(m *mfa.Service, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			view, ok := auth.FromUser(r.Context())
			if !ok || view.UserID == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if view.MFAVerified {
				next.ServeHTTP(w, r)
				return
			}
			if !m.HasMFA(r.Context(), view.UserID) {
				next.ServeHTTP(w, r)
				return
			}
			if isMFAAllowedPath(r) {
				next.ServeHTTP(w, r)
				return
			}
			log.Info("mfa gate: blocking unverified session",
				"userID", view.UserID,
				"path", r.URL.Path,
				"method", r.Method,
			)
			writeMFAChallengeRequired(w)
		})
	}
}

// mfaGateAllowlist is the set of paths a half-authed user is allowed
// to reach. Keys are "<METHOD> <PATH>" so a GET on /api/v1/me is
// allowed while a POST on the same path would still be gated.
//
// Per the review spec, the allowlist covers:
//   - /mfa/challenge            -- recovery path (Issue 1)
//   - /mfa/setup                -- enrollment / re-enrollment after
//     admin reset (Issue 10)
//   - /mfa/disable              -- recovery path (Issue 10)
//   - /mfa/enable               -- finalize setup
//   - /api/v1/me                -- SPA fetches identity / MFA status
//   - DELETE /api/v1/session    -- explicit logout
//
// Anything else under /api/v1/* returns 403 with
// {"mfaChallengeRequired": true} so the SPA can branch to the MFA
// challenge page.
var mfaGateAllowlist = map[string]struct{}{
	"GET /api/v1/mfa/setup":      {},
	"POST /api/v1/mfa/setup":     {},
	"GET /api/v1/mfa/enable":     {},
	"POST /api/v1/mfa/enable":    {},
	"POST /api/v1/mfa/disable":   {},
	"POST /api/v1/mfa/challenge": {},
	"GET /api/v1/me":             {},
	"DELETE /api/v1/session":     {},
}

func isMFAAllowedPath(r *http.Request) bool {
	key := r.Method + " " + r.URL.Path
	_, ok := mfaGateAllowlist[key]
	return ok
}

func writeMFAChallengeRequired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"$schema":"https://huma.rocks/schema/error.json","title":"Forbidden","status":403,"detail":"MFA challenge required","mfaChallengeRequired":true}` + "\n"))
}
