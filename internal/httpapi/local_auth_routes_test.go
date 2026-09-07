// Local-auth routing tests (PR #409, Hermes re-review on PR #416,
// 2026-09-07). These tests pin the routing behavior that the
// openAPIMiddleware's noAuthLocalAuthPaths allowlist establishes: the
// no-auth local-auth endpoints must be reachable without any session
// cookie, and they must NOT 403 behind the global requireAdmin gate.
//
// Without this fix, a no-auth POST /api/v1/login would return 403
// (requireAdmin's "authentication required" response) instead of 401
// (the actual login flow's "invalid credentials" response), making the
// login form unreachable in a real deployment.

package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/ratelimit"
	"github.com/s3ntin3l8/branchdam/internal/auth/session"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

const localAuthTestAgentKey = "01234567890123456789012345678901" // 33 chars

// localAuthTestServer builds a Server with local-auth enabled and
// BRANCHDAM_SECRET_KEY-derived cookie HMAC. The DB is the same fresh
// per-test file the other routes tests use; the only difference is the
// LocalAuthDeps wiring.
func localAuthTestServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "local-auth-routes.db")
	database, err := db.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	// Build a Service with a valid base64 32-byte secret. The test
	// doesn't mint a real session, but the Service needs the key
	// to construct.
	svc := users.NewService(database, usersCookieTestBase64, users.ServiceOptions{
		CookieKey: usersCookieTestKey,
	})
	loginLimiter := ratelimit.New()
	resetLimiter := ratelimit.New()
	sessionMw := session.New(svc, session.Config{
		CookieName: "branchdam_session",
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := workers.New[string](2, 16)
	pool.Run(ctx)

	return New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: localAuthTestAgentKey}, Auth: config.Auth{Mode: string(auth.AuthModeLocal)}},
		DB:      database,
		Prober:  probe.New(),
		Pool:    pool,
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
		LocalAuth: &LocalAuthDeps{
			Users:        svc,
			LoginLimiter: loginLimiter,
			ResetLimiter: resetLimiter,
			SessionMw:    sessionMw,
			Reset:        users.NewPasswordResetService(svc, users.PasswordResetServiceOptions{TokenTTL: time.Hour}),
			AuthMode:     auth.AuthModeLocal,
		},
	})
}

const usersCookieTestBase64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

var usersCookieTestKey = []byte("test-cookie-key-must-be-32-bytes!")

// noAuthRequest builds an HTTP request with no X-Authentik-* headers and
// no session cookie, the way a fresh browser visit to /login would
// arrive.
func noAuthRequest(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestLocalAuthLogin_ReachableWithoutSession pins the re-review's
// Critical finding: an anonymous POST to /api/v1/login must NOT 403
// behind the global requireAdmin gate. The login handler's 401
// "invalid credentials" path is the success state of this test; a
// 403 from requireAdmin would mean the local-auth surface is
// unreachable in a real deployment.
func TestLocalAuthLogin_ReachableWithoutSession(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodPost, "/api/v1/login", []byte(`{"username":"alice","password":"any"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("POST /api/v1/login (no session) returned %d (forbidden); the no-auth local-auth surface is gated behind requireAdmin. body = %s", rr.Code, rr.Body.String())
	}
	// Should be 401 (no-such-user) -- the login handler's actual response.
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "expected 401 from the login handler, not 403 from requireAdmin")
}

// TestLocalAuthSetupStatus_ReachableWithoutSession covers the GET path:
// /api/v1/setup/status must be reachable without a session so the SPA
// can decide whether to render the setup form or the login form on
// first load. requireAdmin's GET bypass wouldn't help here because
// requireAdmin still requires From(r.Context()) to return a
// principal (line 81-84 of authz.go), which it doesn't for an
// anonymous request.
func TestLocalAuthSetupStatus_ReachableWithoutSession(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodGet, "/api/v1/setup/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("GET /api/v1/setup/status (no session) returned %d (forbidden); the no-auth local-auth surface is gated behind requireAdmin. body = %s", rr.Code, rr.Body.String())
	}
	// 200 with the readyForSetup flag.
	assert.Equal(t, http.StatusOK, rr.Code, "expected 200 from the setup/status handler, not 403 from requireAdmin")
}

// TestLocalAuthPasswordResetRequest_ReachableWithoutSession pins the
// other side of the re-review: a forgotten-password user has no
// session, so the /password-reset/request endpoint must be reachable
// anonymously. 403 would be a real production outage.
func TestLocalAuthPasswordResetRequest_ReachableWithoutSession(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodPost, "/api/v1/password-reset/request", []byte(`{"email":"alice@example.com"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("POST /api/v1/password-reset/request (no session) returned %d (forbidden); forgotten-password users cannot reach the recovery endpoint. body = %s", rr.Code, rr.Body.String())
	}
	assert.Equal(t, http.StatusOK, rr.Code, "expected 200 from the password-reset/request handler, not 403 from requireAdmin")
}

// TestLocalAuthPasswordResetConfirm_ReachableWithoutSession covers
// the second half of the reset flow. The confirm endpoint is also
// no-auth (the user is using the token in the URL, not a session
// cookie), so the same allowlist must apply.
func TestLocalAuthPasswordResetConfirm_ReachableWithoutSession(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodPost, "/api/v1/password-reset/confirm", []byte(`{"token":"any","newPassword":"newpassword123"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("POST /api/v1/password-reset/confirm (no session) returned %d (forbidden); the token-based reset path is gated. body = %s", rr.Code, rr.Body.String())
	}
	// 404 (token not found) -- the handler's actual response, not requireAdmin's 403.
	assert.Equal(t, http.StatusNotFound, rr.Code, "expected 404 from the password-reset/confirm handler, not 403 from requireAdmin")
}

// TestLocalAuthDeleteSession_NoCookieIsNoOp covers the DELETE
// /api/v1/session path: an anonymous request should be a no-op
// (the handler reads the cookie, finds none, and returns 204
// without revoking anything). 403 would mean the requireAdmin gate
// is blocking a request that has no per-endpoint policy to enforce.
func TestLocalAuthDeleteSession_NoCookieIsNoOp(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodDelete, "/api/v1/session", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("DELETE /api/v1/session (no cookie) returned %d (forbidden); the logout no-op is gated. body = %s", rr.Code, rr.Body.String())
	}
	// 204 No Content -- the handler's actual response.
	assert.Equal(t, http.StatusNoContent, rr.Code, "expected 204 from the logout handler, not 403 from requireAdmin")
}

// TestLocalAuthAdminResetPassword_StillGated is the negative test:
// the admin-side reset endpoint at /api/v1/admin/users/{id}/reset-password
// is NOT in the no-auth allowlist (an anonymous admin-reset would be
// a privilege escalation). It must still 403 behind requireAdmin.
func TestLocalAuthAdminResetPassword_StillGated(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodPost, "/api/v1/admin/users/1/reset-password", []byte(`{}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "anonymous POST to admin reset must 403; the no-auth allowlist must not include admin paths")
}
