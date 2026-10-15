package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/db"
)

// mintSessionForUser inserts a users row with the given is_admin and
// source and returns a live (MFA-free) session cookie for it.
// source='forward-jit' rows get a NULL password_hash (schema CHECK); a
// session for one stands in for "an admin who is not a local admin",
// e.g. a forward-auth admin, without the both-mode proxy plumbing.
func mintSessionForUser(t *testing.T, database *db.DB, svc *users.Service, username string, isAdmin bool, source string) (cookieValue string, userID int64) {
	t.Helper()
	ctx := context.Background()
	admin := 0
	if isAdmin {
		admin = 1
	}
	var hash any
	authProvider := "local"
	if source == "local" {
		h, err := svc.HashPassword("password123")
		require.NoError(t, err)
		hash = h
	} else {
		authProvider = "authentik"
	}
	res, err := database.ExecInTx(ctx,
		"INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid) VALUES (?1, ?2, ?3, ?4, ?5, ?6, 'test', ?7, ?8)",
		username, username+"@example.com", hash, admin, source, time.Now().Unix(), authProvider, "uid:"+username,
	)
	require.NoError(t, err)
	userID, err = res.LastInsertId()
	require.NoError(t, err)

	cookieID, cookieValue, err := svc.MintCookieValue()
	require.NoError(t, err)
	now := time.Now()
	_, err = svc.CreateSession(ctx, userID, cookieID, "127.0.0.1", "test", now.Add(time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	return cookieValue, userID
}

// TestLocalNonAdmin_DeniedAdminRoutes_EmptyAuthzGroups is the regression
// for "every local user is an admin under the default config": with
// authz.groups empty, auth.IsAdmin returned true for any authenticated
// principal before consulting users.is_admin, so a non-admin local user
// could reset the admin's password (and receive the new one), write
// settings and restart the server.
func TestLocalNonAdmin_DeniedAdminRoutes_EmptyAuthzGroups(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	require.Empty(t, srv.cfg().Authz.Groups, "precondition: authz.groups is empty (default config)")
	adminCookie, adminID := mintSessionForUser(t, database, svc, "admin", true, "local")
	userCookie, userID := mintSessionForUser(t, database, svc, "bob", false, "local")

	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, fmt.Sprintf("/api/v1/admin/users/%d/reset-password", adminID), `{}`},
		{http.MethodPut, "/api/v1/settings", `{"set":{"thumbnails.maxDimension":256}}`},
		{http.MethodPost, "/api/v1/restart", ``},
		{http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", userID), `{"isAdmin":true}`},
		{http.MethodGet, "/api/v1/settings", ``},
	}
	for _, c := range cases {
		t.Run("non-admin "+c.method+" "+c.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, withSessionCookie(c.method, c.path, userCookie, []byte(c.body)))
			assert.Equal(t, http.StatusForbidden, rr.Code, "body = %s", rr.Body.String())
		})
	}

	t.Run("non-admin /me reports isAdmin=false", func(t *testing.T) {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/me", userCookie, nil))
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Body.String(), `"isAdmin":false`)
	})

	// The admin still passes every gate. Restart answers 503 (no restart
	// hook wired in tests) only after requireSettingsAdmin let it through.
	t.Run("admin passes", func(t *testing.T) {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, fmt.Sprintf("/api/v1/admin/users/%d/reset-password", userID), adminCookie, []byte(`{}`)))
		assert.Equal(t, http.StatusOK, rr.Code, "admin reset-password; body = %s", rr.Body.String())

		rr = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, "/api/v1/restart", adminCookie, nil))
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "admin restart; body = %s", rr.Body.String())

		rr = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/me", adminCookie, nil))
		assert.Contains(t, rr.Body.String(), `"isAdmin":true`)
	})
}

// TestLocalNonAdmin_MFASelfServiceNotAdminGated pins that tightening the
// admin policy did not lock non-admin local users out of their own MFA
// endpoints: the TOTP challenge at login and enrolment must reach the
// handler rather than the generic admin 403.
func TestLocalNonAdmin_MFASelfServiceNotAdminGated(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	_, _ = mintSessionForUser(t, database, svc, "admin", true, "local")
	userCookie, _ := mintSessionForUser(t, database, svc, "bob", false, "local")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, "/api/v1/mfa/setup", userCookie, []byte(`{}`)))
	assert.Equal(t, http.StatusOK, rr.Code, "non-admin MFA setup; body = %s", rr.Body.String())

	halfCookie, _ := mintHalfAuthSessionForUser(t, database, svc, "carol", "password123", true)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, "/api/v1/mfa/challenge", halfCookie, []byte(`{"code":"000000"}`)))
	assert.NotContains(t, rr.Body.String(), "admin authorization required", "challenge must reach its handler; code=%d", rr.Code)
	assert.NotEqual(t, http.StatusOK, rr.Code, "a wrong code must not verify")
}

// TestAdminUsers_LastLocalAdminGuard: disabling or demoting the last
// enabled local admin is refused with 409, even when a different admin
// (here a non-local forward-jit admin) asks for it. Without the guard the
// instance is left with nobody who can sign in with a password to
// administer it.
func TestAdminUsers_LastLocalAdminGuard(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	_, localAdminID := mintSessionForUser(t, database, svc, "root", true, "local")
	otherAdminCookie, _ := mintSessionForUser(t, database, svc, "fwd-admin", true, "forward-jit")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", localAdminID), otherAdminCookie, []byte(`{"isAdmin":false}`)))
	assert.Equal(t, http.StatusConflict, rr.Code, "demote last local admin; body = %s", rr.Body.String())

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, fmt.Sprintf("/api/v1/admin/users/%d/disable", localAdminID), otherAdminCookie, nil))
	assert.Equal(t, http.StatusConflict, rr.Code, "disable last local admin; body = %s", rr.Body.String())

	row, err := svc.GetUserByID(context.Background(), localAdminID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), row.IsAdmin, "refused demote must not change is_admin")
	assert.False(t, row.DisabledAt.Valid, "refused disable must not set disabled_at")

	// With a second local admin present the same actions succeed.
	_, _ = mintSessionForUser(t, database, svc, "root2", true, "local")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", localAdminID), otherAdminCookie, []byte(`{"isAdmin":false}`)))
	assert.Equal(t, http.StatusOK, rr.Code, "demote with another local admin present; body = %s", rr.Body.String())
}
