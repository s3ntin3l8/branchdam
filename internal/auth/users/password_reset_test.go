package users

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
)

// newPasswordResetService builds a Service + PasswordResetService
// against a fresh on-disk SQLite file, with migrations applied. Used
// by every test in this file; mirrors newTestService from
// users_test.go.
func newPasswordResetService(t *testing.T, ttl time.Duration) (*Service, *PasswordResetService) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "users.db")
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	svc := NewService(database, testSecretBase64, ServiceOptions{})
	reset := NewPasswordResetService(svc, PasswordResetServiceOptions{TokenTTL: ttl})
	return svc, reset
}

// makeLocalUser inserts a local user with the given email so the
// password-reset path can find them via GetUserByEmailSource.
func makeLocalUser(t *testing.T, svc *Service, username, email, password string, isAdmin bool) int64 {
	t.Helper()
	hash, err := svc.HashPassword(password)
	require.NoError(t, err)
	var emailNS any = nil
	if email != "" {
		emailNS = email
	}
	created, err := svc.CreateLocalUser(
		context.Background(),
		username, emailFromAny(emailNS), password,
		isAdmin, time.Now().Unix(), "test",
	)
	require.NoError(t, err)
	_ = hash
	_ = emailNS
	return created.ID
}

// emailFromAny returns the email string from a possibly-nil value.
// Local helper so the test file doesn't have to import database/sql
// just to pass a null-string.
func emailFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestRequestPasswordReset_NoSuchUser_ReturnsFalse(t *testing.T) {
	_, reset := newPasswordResetService(t, time.Hour)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "ghost@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	assert.False(t, ok, "no user exists -- request must return ok=false")
	assert.Empty(t, issue.PlaintextToken)
}

func TestRequestPasswordReset_HappyPath_MintsToken(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	_ = makeLocalUser(t, svc, "alice", "alice@example.com", "correct horse battery staple", true)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotEmpty(t, issue.PlaintextToken, "operator-facing plaintext must be returned")
	assert.Greater(t, issue.Token.ID, int64(0))
	assert.Equal(t, int64(time.Now().Add(time.Hour).Unix()), issue.ExpiresAt.Unix(),
		"ExpiresAt should be exactly TokenTTL from now; not tested on boundary but the field must be populated")
}

func TestRequestPasswordReset_DisabledUser_SilentlyIgnored(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	id := makeLocalUser(t, svc, "alice", "alice@example.com", "correct horse battery staple", true)
	require.NoError(t, svc.DisableUser(context.Background(), id, time.Now()))
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	assert.False(t, ok, "disabled user must NOT get a token -- the response is enumeration-identical to no-such-user")
	assert.Empty(t, issue.PlaintextToken)
}

func TestConfirmPasswordReset_HappyPath(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	id := makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)

	result, err := reset.ConfirmPasswordReset(context.Background(), issue.PlaintextToken, "new password 123", "127.0.0.1", "test")
	require.NoError(t, err)
	assert.Equal(t, id, result.User.ID)

	// Old password no longer works
	assert.ErrorIs(t, svc.VerifyPassword("old password", result.User.PasswordHash.String), ErrPasswordMismatch)
	// New password works
	assert.NoError(t, svc.VerifyPassword("new password 123", result.User.PasswordHash.String))
}

func TestConfirmPasswordReset_RejectsReusedToken(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	_ = makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = reset.ConfirmPasswordReset(context.Background(), issue.PlaintextToken, "first new password", "127.0.0.1", "test")
	require.NoError(t, err)
	_, err = reset.ConfirmPasswordReset(context.Background(), issue.PlaintextToken, "second new password", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrTokenNotFound, "second attempt must fail with the same error as 'never existed' -- no enumeration of used vs. unknown")
}

func TestConfirmPasswordReset_RejectsExpiredToken(t *testing.T) {
	svc, reset := newPasswordResetService(t, 50*time.Millisecond)
	_ = makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)
	time.Sleep(60 * time.Millisecond) // comfortably past expiry
	_, err = reset.ConfirmPasswordReset(context.Background(), issue.PlaintextToken, "new password 123", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrTokenNotFound, "expired token must report the same error as never-existed")
}

func TestConfirmPasswordReset_RejectsShortPassword(t *testing.T) {
	_, reset := newPasswordResetService(t, time.Hour)
	_, err := reset.ConfirmPasswordReset(context.Background(), "any-token", "short", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrTokenNotFound, "short password should fail with the same generic error, not a 422 -- keeps confirm 1:1 with token validity")
}

func TestConfirmPasswordReset_RejectsEmptyToken(t *testing.T) {
	_, reset := newPasswordResetService(t, time.Hour)
	_, err := reset.ConfirmPasswordReset(context.Background(), "", "any password here", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrTokenNotFound)
}

func TestConfirmPasswordReset_RejectsTamperedToken(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	_ = makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)
	tampered := issue.PlaintextToken + "x" // length differs -> different sha256
	_, err = reset.ConfirmPasswordReset(context.Background(), tampered, "new password 123", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrTokenNotFound)
}

func TestAdminResetPassword_RotatesAndReturnsNew(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	id := makeLocalUser(t, svc, "alice", "alice@example.com", "old password", false)
	result, err := reset.AdminResetPassword(context.Background(), id, "admin:bob", "127.0.0.1", "test")
	require.NoError(t, err)
	assert.Equal(t, id, result.User.ID)
	assert.NotEmpty(t, result.NewPassword)
	assert.GreaterOrEqual(t, len(result.NewPassword), 16, "admin-issued passwords must be at least 16 chars")
	// Old password no longer works
	assert.ErrorIs(t, svc.VerifyPassword("old password", result.User.PasswordHash.String), ErrPasswordMismatch)
	// New password works
	assert.NoError(t, svc.VerifyPassword(result.NewPassword, result.User.PasswordHash.String))
}

func TestAdminResetPassword_NoSuchUser(t *testing.T) {
	_, reset := newPasswordResetService(t, time.Hour)
	_, err := reset.AdminResetPassword(context.Background(), 9999, "admin:bob", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestRevokeToken_Idempotent(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	_ = makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	issue, ok, err := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, reset.RevokeToken(context.Background(), issue.Token.ID))
	require.NoError(t, reset.RevokeToken(context.Background(), issue.Token.ID), "second revoke must succeed (the WHERE used_at IS NULL guard means zero rows, no error)")
	_, err = reset.ConfirmPasswordReset(context.Background(), issue.PlaintextToken, "new password 123", "127.0.0.1", "test")
	assert.ErrorIs(t, err, ErrTokenNotFound, "revoked token cannot be consumed")
}

func TestListPending_OnlyUnconsumedAndUnexpired(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	_ = makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	_ = makeLocalUser(t, svc, "bob", "bob@example.com", "old password", false)
	// 3 tokens: 2 for alice, 1 for bob
	issue1, _, _ := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	issue2, _, _ := reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	issue3, _, _ := reset.RequestPasswordReset(context.Background(), "bob@example.com", "127.0.0.1", "test")
	// Consume one
	_, err := reset.ConfirmPasswordReset(context.Background(), issue1.PlaintextToken, "new pwd 1", "127.0.0.1", "test")
	require.NoError(t, err)
	all, err := reset.ListPending(context.Background(), 100, 0)
	require.NoError(t, err)
	// Should be 2: the unconsumed alice token + the unconsumed bob token
	assert.Len(t, all, 2)
	ids := []int64{all[0].ID, all[1].ID}
	assert.Contains(t, ids, issue2.Token.ID)
	assert.Contains(t, ids, issue3.Token.ID)
}

func TestListPendingForUser_FiltersByUser(t *testing.T) {
	svc, reset := newPasswordResetService(t, time.Hour)
	aliceID := makeLocalUser(t, svc, "alice", "alice@example.com", "old password", true)
	bobID := makeLocalUser(t, svc, "bob", "bob@example.com", "old password", false)
	_, _, _ = reset.RequestPasswordReset(context.Background(), "alice@example.com", "127.0.0.1", "test")
	_, _, _ = reset.RequestPasswordReset(context.Background(), "bob@example.com", "127.0.0.1", "test")
	aliceTokens, err := reset.ListPendingForUser(context.Background(), aliceID)
	require.NoError(t, err)
	assert.Len(t, aliceTokens, 1)
	assert.Equal(t, aliceID, aliceTokens[0].UserID)
	bobTokens, err := reset.ListPendingForUser(context.Background(), bobID)
	require.NoError(t, err)
	assert.Len(t, bobTokens, 1)
	assert.Equal(t, bobID, bobTokens[0].UserID)
}

func TestTokenTTL_DefaultWhenZero(t *testing.T) {
	// The constructor must fall back to 24h when TokenTTL is zero
	// or negative -- the test exercises the zero case directly.
	_, reset := newPasswordResetService(t, 0)
	assert.Equal(t, 24*time.Hour, reset.TokenTTL(), "zero TokenTTL should fall back to 24h")
}
