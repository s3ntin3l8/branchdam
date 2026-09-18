// Integration tests for handlePasswordResetRequest's email-delivery
// base-URL selection (CodeQL go/email-content-injection alert #86 on
// PR #460). Earlier revisions fell back to the inbound request's Host
// header whenever auth.email.baseURL was unset, embedding attacker-
// controlled data directly in the reset email. A follow-up revision
// tried to keep that fallback for provider=log only, reasoning that
// logSender never transmits externally -- but CodeQL resolves Send
// through the email.Notifier interface and can't see that a provider
// string comparison guarantees which concrete Send runs, so it
// re-flagged the Host-derived value as reaching smtpSender.Send
// regardless (confirmed live: the alert reopened on that revision).
// passwordResetBaseURL therefore has no Host-header fallback for any
// provider; these tests pin that an unset baseURL skips delivery
// entirely, for both provider=smtp and provider=log.
package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

// recordingNotifier is an email.Notifier stub that records every Send
// call, including the rendered text body (so tests can inspect which
// base URL ended up in the reset link). Safe for concurrent use:
// handlePasswordResetRequest dispatches Send from a background
// goroutine.
type recordingNotifier struct {
	mu       sync.Mutex
	calls    int
	textBody string
}

func (n *recordingNotifier) Send(_ context.Context, _, _, _, textBody string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	n.textBody = textBody
	return nil
}

func (n *recordingNotifier) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

func (n *recordingNotifier) lastTextBody() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.textBody
}

// passwordResetEmailTestServer builds a Server with local-auth and an
// email notifier wired, a single local user with a known email, and
// the given provider/auth.email.baseURL. Mirrors localAuthTestServer
// in local_auth_routes_test.go.
func passwordResetEmailTestServer(t *testing.T, provider, baseURL string, notifier *recordingNotifier) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password-reset-email.db")
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

	const testEmail = "alice@example.com"
	_, err = svc.CreateLocalUser(context.Background(), "alice", testEmail, "correct horse battery staple", true, time.Now().Unix(), "test")
	require.NoError(t, err)

	srv := New(Deps{
		Config: &config.Config{
			Agent: config.Agent{APIKey: localAuthTestAgentKey},
			Auth: config.Auth{
				Mode:  string(auth.AuthModeLocal),
				Email: config.AuthEmail{Provider: provider, BaseURL: baseURL},
			},
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
			Email:        notifier,
		},
	})
	return srv, testEmail
}

func requestPasswordReset(t *testing.T, srv *Server, host, email string) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(`{"email":"` + email + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/password-reset/request", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if host != "" {
		req.Host = host
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestHandlePasswordResetRequest_SMTP_SkipsEmailWhenBaseURLUnset(t *testing.T) {
	notifier := &recordingNotifier{}
	srv, userEmail := passwordResetEmailTestServer(t, "smtp", "", notifier)

	rr := requestPasswordReset(t, srv, "attacker.example", userEmail)
	assert.Equal(t, http.StatusOK, rr.Code)

	// The goroutine path is never entered when baseURL is unset -- there
	// is nothing to wait on -- so no synchronization is needed here.
	assert.Equal(t, 0, notifier.callCount(), "must not fall back to the Host header; email delivery should be skipped entirely")
}

func TestHandlePasswordResetRequest_LogProvider_SkipsEmailWhenBaseURLUnset(t *testing.T) {
	notifier := &recordingNotifier{}
	srv, userEmail := passwordResetEmailTestServer(t, "log", "", notifier)

	rr := requestPasswordReset(t, srv, "attacker.example", userEmail)
	assert.Equal(t, http.StatusOK, rr.Code)

	assert.Equal(t, 0, notifier.callCount(), "provider=log must also skip delivery when baseURL is unset -- the interface-level fix has no per-provider carve-out")
}

func TestHandlePasswordResetRequest_SendsEmailWhenBaseURLConfigured(t *testing.T) {
	notifier := &recordingNotifier{}
	srv, userEmail := passwordResetEmailTestServer(t, "smtp", "https://branchdam.example.com", notifier)

	rr := requestPasswordReset(t, srv, "attacker.example", userEmail)
	assert.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool {
		return notifier.callCount() == 1
	}, time.Second, 5*time.Millisecond, "email delivery runs in a background goroutine")
	assert.Contains(t, notifier.lastTextBody(), "https://branchdam.example.com/password-reset?token=")
	assert.NotContains(t, notifier.lastTextBody(), "attacker.example", "the spoofed Host header must never reach the reset link")
}
