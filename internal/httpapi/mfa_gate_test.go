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
	"github.com/s3ntin3l8/branchdam/internal/auth/mfa"
	"github.com/s3ntin3l8/branchdam/internal/auth/ratelimit"
	"github.com/s3ntin3l8/branchdam/internal/auth/session"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

// mfaGateTestServer builds a Server with the MFA service wired so the
// gate middleware runs. Returns the server, the database (so tests
// can seed raw rows), the user service (for cookie/session plumbing),
// and the mfa service (for seeding an enrolled user).
func mfaGateTestServer(t *testing.T) (*Server, *db.DB, *users.Service, *mfa.Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mfa-gate.db")
	database, err := db.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	svc := users.NewService(database, usersCookieTestBase64, users.ServiceOptions{
		CookieKey: usersCookieTestKey,
	})
	loginLimiter := ratelimit.New()
	resetLimiter := ratelimit.New()
	mfaChallengeLimiter := ratelimit.New()
	mfaDisableLimiter := ratelimit.New()
	sessionMw := session.New(svc, session.Config{CookieName: "branchdam_session"})

	box, err := secrets.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	require.NoError(t, err)
	mfaSvc := mfa.NewService(database, box, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := workers.New[string](2, 16)
	pool.Run(ctx)

	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: localAuthTestAgentKey}, Auth: config.Auth{Mode: string(auth.AuthModeLocal)}},
		DB:      database,
		Prober:  probe.New(),
		Pool:    pool,
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
		LocalAuth: &LocalAuthDeps{
			Users:               svc,
			LoginLimiter:        loginLimiter,
			ResetLimiter:        resetLimiter,
			SessionMw:           sessionMw,
			Reset:               users.NewPasswordResetService(svc, users.PasswordResetServiceOptions{TokenTTL: time.Hour}),
			MFA:                 mfaSvc,
			MFAChallengeLimiter: mfaChallengeLimiter,
			MFADisableLimiter:   mfaDisableLimiter,
			AuthMode:            auth.AuthModeLocal,
		},
	})
	return srv, database, svc, mfaSvc
}

// mintHalfAuthSessionForUser seeds a local user, optionally enrolls
// MFA, then mints a session cookie. Used by the MFA-gate tests below
// to simulate the "logged in but MFA not yet verified" state.
func mintHalfAuthSessionForUser(t *testing.T, database *db.DB, svc *users.Service, username, password string, enrollMFA bool) (cookieValue string, userID int64) {
	t.Helper()
	ctx := context.Background()

	hash, err := svc.HashPassword(password)
	require.NoError(t, err)
	res, err := database.ExecInTx(ctx,
		"INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid) VALUES (?1, ?2, ?3, 0, 'local', ?4, 'test', 'local', ?5)",
		username, username+"@example.com", hash, time.Now().Unix(), "local:"+username,
	)
	require.NoError(t, err)
	userID, err = res.LastInsertId()
	require.NoError(t, err)

	if enrollMFA {
		// Enroll MFA by directly inserting a credential row -- we
		// don't need the full setup/enable TOTP dance here, just
		// the row that HasMFA looks for.
		_, err := database.ExecInTx(ctx,
			"INSERT INTO mfa_credentials (user_id, secret_encrypted, algo, digits, period, last_used_step, recovery_code_salt) VALUES (?1, ?2, ?3, ?4, ?5, 0, ?6)",
			userID, "v1:dummy", "SHA1", 6, 30, "salt",
		)
		require.NoError(t, err)
	}

	cookieID, cookieValue, err := svc.MintCookieValue()
	require.NoError(t, err)
	now := time.Now()
	_, err = svc.CreateSession(ctx, userID, cookieID, "127.0.0.1", "test", now.Add(time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	return cookieValue, userID
}

// withSessionCookie builds a request that carries the session cookie
// the half-auth test minted. Tests use this to drive the gate as if
// a real browser followed the login -> MFA-prompted flow.
func withSessionCookie(method, path string, cookieValue string, body []byte) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "branchdam_session", Value: cookieValue})
	return req
}

// TestMFAGate_RestrictedAPIPath_Returns403 pins Issue 1: a half-authed
// user (local session + MFA enrolled + MFAVerified=false) hitting
// /api/v1/users must get 403 with mfaChallengeRequired:true. The
// allowlist endpoints must still respond normally.
func TestMFAGate_RestrictedAPIPath_Returns403(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	cookieValue, _ := mintHalfAuthSessionForUser(t, database, svc, "alice", "password123", true)

	// Non-allowlisted API path: /api/v1/users (admin listing).
	req := withSessionCookie(http.MethodGet, "/api/v1/users", cookieValue, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code, "half-authed user must be blocked from /api/v1/users; body = %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "mfaChallengeRequired", "403 body must include mfaChallengeRequired flag so SPA can branch")
}

// TestMFAGate_AllowedAPIPath_ReachesHandler pins the positive side of
// the allowlist: GET /api/v1/me must reach the handler even when the
// user is half-authed. The SPA needs this to detect "MFA not verified
// yet" and render the challenge page.
func TestMFAGate_AllowedAPIPath_ReachesHandler(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	cookieValue, _ := mintHalfAuthSessionForUser(t, database, svc, "alice", "password123", true)

	req := withSessionCookie(http.MethodGet, "/api/v1/me", cookieValue, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code, "GET /api/v1/me must be allowed in the gate allowlist; body = %s", rr.Body.String())
}

// TestMFAGate_NoMFAEnrolled_AllowsAllAPIPaths pins the negative case:
// a user without MFA enrolled has nothing to gate, even when their
// session is unauthenticated from MFA's perspective. Half-authed is
// impossible in practice (login only sets mfa_verified_at=NULL when
// MFA IS enrolled), but the gate must defend against future code paths
// that might attach a LocalUserView with UserID != 0 and MFAVerified=false
// to a user without mfa_credentials.
func TestMFAGate_NoMFAEnrolled_AllowsAllAPIPaths(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	cookieValue, _ := mintHalfAuthSessionForUser(t, database, svc, "bob", "password123", false)

	req := withSessionCookie(http.MethodGet, "/api/v1/users", cookieValue, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	assert.NotEqual(t, http.StatusForbidden, rr.Code, "no MFA enrolled -> gate must NOT 403; response was %d body=%s", rr.Code, rr.Body.String())
}
