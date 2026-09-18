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

func TestLocalAuthAdminCreateUser_GatedWhenAnonymous(t *testing.T) {
	srv := localAuthTestServer(t)
	req := noAuthRequest(t, http.MethodPost, "/api/v1/admin/users", []byte(`{"username":"bob"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "anonymous POST to admin create user must 403")
}

func TestLocalAuthAdminCreateUser_Success(t *testing.T) {
	srv := localAuthTestServer(t)
	body := []byte(`{"username":"bob","email":"bob@example.com","password":"password123","isAdmin":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithPrincipal(req.Context(), adminPrincipal())
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"username":"bob"`)
	assert.Contains(t, rr.Body.String(), `"email":"bob@example.com"`)
	assert.Contains(t, rr.Body.String(), `"isAdmin":true`)
}

func TestLocalAuthAdminCreateUser_AutoGeneratePassword(t *testing.T) {
	srv := localAuthTestServer(t)
	body := []byte(`{"username":"charlie"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithPrincipal(req.Context(), adminPrincipal())
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"temporaryPassword":`)
	assert.Contains(t, rr.Body.String(), `"shownOnceNotice":`)
}

func TestLocalAuthAdminDisableUser_Success(t *testing.T) {
	srv := localAuthTestServer(t)
	// Create user first
	createBody := []byte(`{"username":"dan","password":"password123"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createCtx := auth.WithPrincipal(createReq.Context(), adminPrincipal())
	createReq = createReq.WithContext(createCtx)

	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	// Disable user
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/disable", nil)
	ctx := auth.WithPrincipal(req.Context(), adminPrincipal())
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"ok":true`)
}

func localAuthTestServerWithGroups(t *testing.T, groups []string) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "local-auth-routes-groups.db")
	database, err := db.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

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
		Config: &config.Config{
			Agent: config.Agent{APIKey: localAuthTestAgentKey},
			Auth:  config.Auth{Mode: string(auth.AuthModeLocal)},
			Authz: config.Authz{Groups: groups},
		},
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

func TestLocalAuthAdminRoutes_RejectsNonAdminPrincipal(t *testing.T) {
	srv := localAuthTestServerWithGroups(t, []string{"dam-admins"})
	nonAdmin := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "regular",
		Email:         "regular@example.com",
		Groups:        []string{"regular-users"},
		Authenticated: true,
	}

	endpoints := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodPost, "/api/v1/admin/users", []byte(`{"username":"newguy"}`)},
		{http.MethodPost, "/api/v1/admin/users/1/disable", nil},
		{http.MethodPost, "/api/v1/admin/users/1/reset-password", nil},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			var rdr *bytes.Reader
			if ep.body != nil {
				rdr = bytes.NewReader(ep.body)
			} else {
				rdr = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(ep.method, ep.path, rdr)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(auth.WithPrincipal(req.Context(), nonAdmin))

			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			assert.Equal(t, http.StatusForbidden, rr.Code, "expected 403 Forbidden for non-admin principal on %s, got %d (body: %s)", ep.path, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "admin authorization required")
		})
	}
}

func TestLocalAuthAdminCreateUser_ConflictDifferentiation(t *testing.T) {
	srv := localAuthTestServer(t)

	// Create initial user
	body1 := []byte(`{"username":"user1","email":"user1@example.com","password":"password123"}`)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1 = req1.WithContext(auth.WithPrincipal(req1.Context(), adminPrincipal()))
	rr1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusCreated, rr1.Code)

	// Duplicate username
	body2 := []byte(`{"username":"user1","email":"other@example.com","password":"password123"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2 = req2.WithContext(auth.WithPrincipal(req2.Context(), adminPrincipal()))
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)
	assert.Equal(t, http.StatusConflict, rr2.Code)
	assert.Contains(t, rr2.Body.String(), "username is already taken")

	// Duplicate email
	body3 := []byte(`{"username":"user2","email":"user1@example.com","password":"password123"}`)
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(body3))
	req3.Header.Set("Content-Type", "application/json")
	req3 = req3.WithContext(auth.WithPrincipal(req3.Context(), adminPrincipal()))
	rr3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr3, req3)
	assert.Equal(t, http.StatusConflict, rr3.Code)
	assert.Contains(t, rr3.Body.String(), "email is already taken")
}

func TestLocalAuthAdminDisableUser_NotFound(t *testing.T) {
	srv := localAuthTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/99999/disable", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "user not found")
}

func TestLocalAuthAdminDisableUser_CannotDisableSelf(t *testing.T) {
	srv := localAuthTestServer(t)

	// Create user
	createBody := []byte(`{"username":"adminuser","password":"password123"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	// Disable self (UserID 1 matches target 1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/disable", nil)
	ctx := auth.WithPrincipal(req.Context(), adminPrincipal())
	ctx = auth.WithLocalUserView(ctx, auth.LocalUserView{UserID: 1, IsAdmin: true})
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "cannot disable current user")
}

// TestLocalAuthAdminUpdateUser_HappyPath: PATCH on a non-admin user
// promoting them to admin returns 200 with the full updated user
// shape. Pins the contract that PATCH's response carries every
// AttributionUser field (including the freshly-added externalUid +
// lastSeenAt from the Hermes re-review) so the SPA's typecheck passes.
func TestLocalAuthAdminUpdateUser_HappyPath(t *testing.T) {
	srv := localAuthTestServer(t)

	// Create a non-admin user (auto-assigned id 1; the calling admin is
	// only authenticated via adminPrincipal() which doesn't carry a
	// LocalUserView, so the self-target guard does not fire).
	createBody := []byte(`{"username":"target","password":"password123","isAdmin":false}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	// Promote user 1 to admin via PATCH.
	patchBody := []byte(`{"isAdmin":true}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/1", bytes.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"ok":true`)
	assert.Contains(t, rr.Body.String(), `"isAdmin":true`)
	assert.Contains(t, rr.Body.String(), `"username":"target"`)
	// Hermes re-review Issue 1: the PATCH response must carry the
	// same AttributionUser-required fields the create endpoint does.
	// For source='local' rows, external_uid stores the username
	// (CreateLocalUser's INSERT pins ?1 into both columns); lastSeenAt
	// defaults to unixepoch() on insert, so for a brand-new user it
	// equals createdAt.
	assert.Contains(t, rr.Body.String(), `"externalUid":"target"`, "externalUid must be present (for local users, equals the username)")
	assert.Contains(t, rr.Body.String(), `"lastSeenAt":`, "lastSeenAt must be present so the SPA's AttributionUser type-checks")
	assert.Contains(t, rr.Body.String(), `"createdAt":`)
	assert.Contains(t, rr.Body.String(), `"createdBy":`)
	assert.Contains(t, rr.Body.String(), `"source":"local"`)
}

// TestLocalAuthAdminUpdateUser_AlreadyActive: PATCH on a user whose
// isAdmin already matches the requested value returns 400 "no
// changes requested" rather than a silent no-op 200. The handler
// compares the new value to the current row and rejects no-ops so
// the audit log doesn't fill up with redundant entries.
func TestLocalAuthAdminUpdateUser_AlreadyActive(t *testing.T) {
	srv := localAuthTestServer(t)

	// Create user with isAdmin=true (the calling admin has no
	// LocalUserView, so self-target guard doesn't fire).
	createBody := []byte(`{"username":"alreadyadmin","password":"password123","isAdmin":true}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	// PATCH asking for isAdmin=true on a user that's already admin.
	patchBody := []byte(`{"isAdmin":true}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/1", bytes.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "no changes requested")
}

// TestLocalAuthAdminUpdateUser_NotFound: PATCH with a user id that
// has no row returns 404 (not 400 -- the id parses fine, the row is
// just absent). Pins the GetUserByID-NotFound branch.
func TestLocalAuthAdminUpdateUser_NotFound(t *testing.T) {
	srv := localAuthTestServer(t)

	patchBody := []byte(`{"isAdmin":true}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/99999", bytes.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "user not found")
}

// TestLocalAuthAdminEnableUser_HappyPath: disable, then enable via
// POST /enable. Pins the 200 contract and the "user not disabled"
// 400 path's absence here.
func TestLocalAuthAdminEnableUser_HappyPath(t *testing.T) {
	srv := localAuthTestServer(t)

	// Create user, then disable it.
	createBody := []byte(`{"username":"re-enable-me","password":"password123"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	disableReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/disable", nil)
	disableReq = disableReq.WithContext(auth.WithPrincipal(disableReq.Context(), adminPrincipal()))
	disableRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(disableRR, disableReq)
	require.Equal(t, http.StatusOK, disableRR.Code)

	// Re-enable via POST /enable.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/enable", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"ok":true`)
	assert.Contains(t, rr.Body.String(), `"id":1`)
}

// TestLocalAuthAdminEnableUser_AlreadyActive: POST /enable on a user
// whose disabled_at is NULL returns 400 "user is not disabled" --
// the handler explicitly checks disabled_at before issuing the SQL
// UPDATE so a redundant /enable call can't silently succeed.
func TestLocalAuthAdminEnableUser_AlreadyActive(t *testing.T) {
	srv := localAuthTestServer(t)

	createBody := []byte(`{"username":"alreadyactive","password":"password123"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/enable", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "user is not disabled")
}

// TestLocalAuthAdminEnableUser_NotFound: POST /enable on a missing
// user id 404s. Same lookup-then-update shape as /disable.
func TestLocalAuthAdminEnableUser_NotFound(t *testing.T) {
	srv := localAuthTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/99999/enable", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "user not found")
}

// TestLocalAuthAdminRevokeSessions_CannotRevokeSelf: Hermes re-review
// Issue 2 -- /revoke-sessions must guard against the calling admin
// targeting their own user id, just like /disable and PATCH do.
// Revoking your own sessions would kill the session cookie attached
// to this very request mid-flight; the session middleware drops
// revoked sessions to anonymous on the next request, which would
// surface a confusing 401 to the SPA.
func TestLocalAuthAdminRevokeSessions_CannotRevokeSelf(t *testing.T) {
	srv := localAuthTestServer(t)

	createBody := []byte(`{"username":"self","password":"password123"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	// Caller's LocalUserView matches the target id.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/revoke-sessions", nil)
	ctx := auth.WithPrincipal(req.Context(), adminPrincipal())
	ctx = auth.WithLocalUserView(ctx, auth.LocalUserView{UserID: 1, IsAdmin: true})
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "cannot revoke your own sessions")
}

// TestLocalAuthAdminRevokeSessions_HappyPath: revoke all sessions
// for a different user returns 200 with the revokedCount. Seeds
// two sessions for the target, asserts both are revoked and the
// count surfaces in the response.
func TestLocalAuthAdminRevokeSessions_HappyPath(t *testing.T) {
	srv := localAuthTestServer(t)

	// Create target user (id 1) and seed two sessions.
	createBody := []byte(`{"username":"victim","password":"password123"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(auth.WithPrincipal(createReq.Context(), adminPrincipal()))
	createRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	usersSvc := srv.localAuth.users
	now := time.Now()
	for i := 0; i < 2; i++ {
		cid, _, err := usersSvc.MintCookieValue()
		require.NoError(t, err)
		_, err = usersSvc.CreateSession(context.Background(), 1, cid, "127.0.0.1", "test", now.Add(time.Hour), now.Add(time.Minute))
		require.NoError(t, err)
	}

	// Caller is a different user id (2) -- self-target guard does not fire.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/1/revoke-sessions", nil)
	ctx := auth.WithPrincipal(req.Context(), adminPrincipal())
	ctx = auth.WithLocalUserView(ctx, auth.LocalUserView{UserID: 2, IsAdmin: true})
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"ok":true`)
	assert.Contains(t, rr.Body.String(), `"revokedCount":2`)
}

// TestLocalAuthAdminRevokeSessions_NotFound: /revoke-sessions on a
// missing user id 404s. Same lookup-then-revoke shape as /disable.
func TestLocalAuthAdminRevokeSessions_NotFound(t *testing.T) {
	srv := localAuthTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/99999/revoke-sessions", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal()))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "user not found")
}
