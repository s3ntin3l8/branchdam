package users

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
)

// testSecretBase64 is 32 zero bytes encoded as base64 (44 chars).
const testSecretBase64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func newTestService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "users.db")
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return NewService(database, testSecretBase64, ServiceOptions{})
}

func TestHashPassword_Verify_RoundTrip(t *testing.T) {
	svc := newTestService(t)
	hash, err := svc.HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$v="), "expected argon2id-encoded hash, got %q", hash)

	require.NoError(t, svc.VerifyPassword("correct horse battery staple", hash))
	assert.ErrorIs(t, svc.VerifyPassword("wrong", hash), ErrPasswordMismatch)
}

func TestVerifyPassword_MalformedHashReturnsError(t *testing.T) {
	svc := newTestService(t)
	assert.Error(t, svc.VerifyPassword("anything", "not-an-argon2-hash"))
	assert.Error(t, svc.VerifyPassword("anything", "$argon2id$v=19$m=1,t=1,p=1$short"))
}

func TestMintCookieValue_VerifyCookieValue_RoundTrip(t *testing.T) {
	svc := newTestService(t)
	cookieID, cookieValue, err := svc.MintCookieValue()
	require.NoError(t, err)
	assert.Len(t, cookieID, 64)

	gotID, err := svc.VerifyCookieValue(cookieValue)
	require.NoError(t, err)
	assert.Equal(t, cookieID, gotID)
}

func TestVerifyCookieValue_RejectsTampered(t *testing.T) {
	svc := newTestService(t)
	_, cookieValue, err := svc.MintCookieValue()
	require.NoError(t, err)

	// Tamper the cookieID portion: flip the first char to a value
	// other than the original. Using "flip the first hex char" rather
	// than "prepend 'a'" because the first byte is random hex and
	// happens to be 'a' 1/16 of the time, making the previous test
	// shape flake under parallel codecov runs.
	first := cookieValue[0]
	flip := byte('0')
	if first == '0' {
		flip = '1'
	}
	tampered := string(flip) + cookieValue[1:]
	require.NotEqual(t, cookieValue, tampered, "test setup invariant: tamper must differ")
	got, err := svc.VerifyCookieValue(tampered)
	require.NoError(t, err)
	assert.Empty(t, got)

	// Tamper the HMAC tag: flip the last char to a different value.
	last := cookieValue[len(cookieValue)-1]
	flipTag := byte('0')
	if last == '0' {
		flipTag = '1'
	}
	tamperedTag := cookieValue[:len(cookieValue)-1] + string(flipTag)
	require.NotEqual(t, cookieValue, tamperedTag, "test setup invariant: tamper must differ")
	got, err = svc.VerifyCookieValue(tamperedTag)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestVerifyCookieValue_RejectsMalformed(t *testing.T) {
	svc := newTestService(t)
	for _, bad := range []string{"", "no-dot", ".starts-with-dot", "ends-with-dot.", "oddhex.x", "hex.oddhex"} {
		got, err := svc.VerifyCookieValue(bad)
		require.NoError(t, err)
		assert.Empty(t, got, "malformed cookie %q should yield empty", bad)
	}
}

func TestCreateLocalUser_VerifyRoundTrip(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateLocalUser(ctx, "alice", "alice@example.com", "password123", true, time.Now().Unix(), "setup")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.True(t, user.Email.Valid)
	assert.Equal(t, "alice@example.com", user.Email.String)
	assert.True(t, user.PasswordHash.Valid)
	assert.Equal(t, int64(1), user.IsAdmin)
	assert.Equal(t, "local", user.Source)

	got, err := svc.GetUserByUsername(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, user.ID, got.ID)

	require.NoError(t, svc.VerifyPassword("password123", got.PasswordHash.String))
	assert.ErrorIs(t, svc.VerifyPassword("wrong", got.PasswordHash.String), ErrPasswordMismatch)
}

func TestCreateForwardJITUser_FallsBackToEmailLocalPart(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateForwardJITUser(ctx, "", "bob@example.com", true, time.Now().Unix(), "forward:bob")
	require.NoError(t, err)
	assert.Equal(t, "bob", user.Username)
	assert.False(t, user.PasswordHash.Valid, "JIT users never have a password")
}

func TestCreateForwardJITUser_RequiresEitherEmailOrUsername(t *testing.T) {
	svc := newTestService(t)
	// Both empty: must error (caller bug).
	_, err := svc.CreateForwardJITUser(context.Background(), "", "", true, time.Now().Unix(), "forward:nobody")
	assert.Error(t, err)
	// Username only: now allowed (caller is the JIT username-keyed path
	// that handles the "requireEmail=false" config case).
	_, err = svc.CreateForwardJITUser(context.Background(), "eve", "", true, time.Now().Unix(), "forward:eve")
	assert.NoError(t, err)
}

func TestSessionLifecycle(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateLocalUser(ctx, "dave", "", "password", false, time.Now().Unix(), "setup")
	require.NoError(t, err)

	cookieID, _, err := svc.MintCookieValue()
	require.NoError(t, err)

	now := time.Now()
	session, err := svc.CreateSession(ctx, user.ID, cookieID, "127.0.0.1", "test-agent", now.Add(24*time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, session.RevokedAt.Valid)

	got, err := svc.GetSessionByCookieID(ctx, cookieID)
	require.NoError(t, err)
	assert.Equal(t, session.ID, got.ID)

	require.NoError(t, svc.TouchSession(ctx, got.ID, now.Add(time.Minute), now.Add(time.Hour)))
	got2, err := svc.GetSessionByCookieID(ctx, cookieID)
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Minute).Unix(), got2.LastSeenAt)

	require.NoError(t, svc.RevokeSession(ctx, got2.ID))
	got3, err := svc.GetSessionByCookieID(ctx, cookieID)
	require.NoError(t, err)
	assert.True(t, got3.RevokedAt.Valid)
}

func TestRevokeAllUserSessions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateLocalUser(ctx, "eve", "", "password", false, time.Now().Unix(), "setup")
	require.NoError(t, err)

	now := time.Now()
	for i := 0; i < 3; i++ {
		cid, _, err := svc.MintCookieValue()
		require.NoError(t, err)
		_, err = svc.CreateSession(ctx, user.ID, cid, "127.0.0.1", "test", now.Add(time.Hour), now.Add(time.Minute))
		require.NoError(t, err)
	}

	require.NoError(t, svc.RevokeAllUserSessions(ctx, user.ID))

	cid, _, err := svc.MintCookieValue()
	require.NoError(t, err)
	created, err := svc.CreateSession(ctx, user.ID, cid, "127.0.0.1", "test", now.Add(time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, created.RevokedAt.Valid)
	require.NoError(t, svc.RevokeAllUserSessions(ctx, user.ID))
	got, err := svc.GetSessionByCookieID(ctx, cid)
	require.NoError(t, err)
	assert.True(t, got.RevokedAt.Valid)
}

func TestCountUsers(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	n, err := svc.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	_, err = svc.CreateLocalUser(ctx, "frank", "", "password", false, time.Now().Unix(), "setup")
	require.NoError(t, err)

	n, err = svc.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestDisableUser_IsIdempotent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateLocalUser(ctx, "gina", "", "password", false, time.Now().Unix(), "setup")
	require.NoError(t, err)
	assert.False(t, user.DisabledAt.Valid)

	require.NoError(t, svc.DisableUser(ctx, user.ID, time.Now()))
	require.NoError(t, svc.DisableUser(ctx, user.ID, time.Now()))

	got, err := svc.GetUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.True(t, got.DisabledAt.Valid)
}

func TestWriteLoginAudit_BestEffort(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	svc.WriteLoginAudit(ctx, sql.NullInt64{}, "probed-username", "local", "no-such-user", "127.0.0.1", "test-agent", "{}")

	user, err := svc.CreateLocalUser(ctx, "henry", "", "password", false, time.Now().Unix(), "setup")
	require.NoError(t, err)
	svc.WriteLoginAudit(ctx, sql.NullInt64{Int64: user.ID, Valid: true}, "henry", "local", "ok", "127.0.0.1", "test-agent", "{}")
}
