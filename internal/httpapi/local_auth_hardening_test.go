package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func postJSON(srv *Server, path, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, path, "", []byte(body)))
	return rr
}

// TestSelfServiceReset_KeepsMFAChallenge is the regression for "a
// self-service password reset deletes MFA": ConfirmPasswordReset cleared
// mfa_credentials and recovery codes, so anyone who could read the
// user's mailbox could mint a reset token, set a password and log in
// with no second factor. After a self-service reset the next login must
// still stop at the TOTP challenge.
func TestSelfServiceReset_KeepsMFAChallenge(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	_, userID := mintHalfAuthSessionForUser(t, database, svc, "alice", "old-password-123", true)

	issue, ok, err := srv.localAuth.reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)

	rr := postJSON(srv, "/api/v1/password-reset/confirm", `{"token":"`+issue.PlaintextToken+`","newPassword":"brand-new-password"}`)
	require.Equal(t, http.StatusOK, rr.Code, "confirm; body = %s", rr.Body.String())

	_, err = database.Reader.GetMFACredentials(context.Background(), userID)
	require.NoError(t, err, "mfa_credentials must survive a self-service reset")

	rr = postJSON(srv, "/api/v1/login", `{"username":"alice","password":"brand-new-password"}`)
	require.Equal(t, http.StatusOK, rr.Code, "login; body = %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"mfaRequired":true`, "login after a self-service reset must still require TOTP")
}

// TestLogin_DisabledAccount_GenericFailureAndCounted: a disabled account
// with the RIGHT password used to get a distinct 403 "account disabled"
// and no limiter hit -- an oracle confirming the guessed password, with
// unlimited guesses. It must look exactly like a wrong password.
func TestLogin_DisabledAccount_GenericFailureAndCounted(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	_, userID := mintHalfAuthSessionForUser(t, database, svc, "dora", "correct-password", false)
	require.NoError(t, svc.DisableUser(context.Background(), userID, time.Now()))

	wrong := postJSON(srv, "/api/v1/login", `{"username":"dora","password":"wrong-password"}`)
	require.Equal(t, http.StatusUnauthorized, wrong.Code)

	right := postJSON(srv, "/api/v1/login", `{"username":"dora","password":"correct-password"}`)
	assert.Equal(t, http.StatusUnauthorized, right.Code, "body = %s", right.Body.String())
	assert.JSONEq(t, wrong.Body.String(), right.Body.String(), "disabled + right password must be indistinguishable from a wrong password")

	// Each attempt is a limiter failure: the fast window (5 failures)
	// trips, so further correct-password guesses are throttled too.
	for i := 0; i < 5; i++ {
		_ = postJSON(srv, "/api/v1/login", `{"username":"dora","password":"correct-password"}`)
	}
	rr := postJSON(srv, "/api/v1/login", `{"username":"dora","password":"correct-password"}`)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "disabled-account logins must count toward the rate limit")
}

// TestPasswordResetRequest_FoundUserDoesNoWriterWorkOnRequestPath pins
// the timing fix: for a found user the handler used to mint the token
// (a writer-pool transaction) before answering, while a no-such-user
// answered after one reader lookup -- a measurable difference, more so
// under write load, that enumerated accounts despite the identical body.
// The mint now runs off the request path, so the 200 is written even
// while the single writer connection is held by someone else.
func TestPasswordResetRequest_FoundUserDoesNoWriterWorkOnRequestPath(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	_, userID := mintHalfAuthSessionForUser(t, database, svc, "erin", "password123", false)

	held := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- database.InTx(context.Background(), func(*sqlcgen.Queries) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	respDone := make(chan int, 1)
	go func() {
		respDone <- postJSON(srv, "/api/v1/password-reset/request", `{"email":"erin@example.com"}`).Code
	}()
	select {
	case code := <-respDone:
		assert.Equal(t, http.StatusOK, code)
	case <-time.After(3 * time.Second):
		close(release)
		<-respDone
		t.Fatal("found-user reset request blocked on the writer pool: the token mint is still on the request path")
	}

	close(release)
	require.NoError(t, <-writerDone)
	srv.waitPasswordResetJobs()

	pending, err := srv.localAuth.reset.ListPendingForUser(context.Background(), userID)
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the background job must still mint the token")
}
