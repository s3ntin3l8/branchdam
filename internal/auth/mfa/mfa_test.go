package mfa

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

const testKeyBase64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(testKeyBase64)
	require.NoError(t, err)
	return box
}

func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "mfa_test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return NewService(database, testBox(t), slog.New(slog.DiscardHandler)), database
}

func createTestUser(t *testing.T, database *db.DB, username string) int64 {
	t.Helper()
	res, err := database.ExecInTx(context.Background(),
		"INSERT INTO users (username, source, password_hash, is_admin, created_at, created_by, auth_provider, external_uid) VALUES (?1, ?2, ?3, 0, ?4, ?5, ?6, ?7)",
		username, "local", "test-hash-not-a-real-argon2", time.Now().Unix(), "test", "local", "local:"+username,
	)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func TestSetup_StoresPendingSecret(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "alice")

	result, err := svc.Setup(context.Background(), userID, "alice")
	require.NoError(t, err)
	assert.Contains(t, result.OtpauthURI, "otpauth://totp/branchDAM:alice")
	assert.Contains(t, result.OtpauthURI, "issuer=branchDAM")
	assert.Contains(t, result.ASCIIQR, "Scan this URI")
}

func TestEnable_NoSetupFails(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "bob")

	_, err := svc.Enable(context.Background(), userID, "123456")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pending MFA setup")
}

func TestEnable_InvalidCodeFails(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "carol")

	_, err := svc.Setup(context.Background(), userID, "carol")
	require.NoError(t, err)

	_, err = svc.Enable(context.Background(), userID, "000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid TOTP code")
}

func TestEnable_ValidCodeSucceeds(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "dave")

	setupResult, err := svc.Setup(context.Background(), userID, "dave")
	require.NoError(t, err)

	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)

	codes, err := svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)
	assert.Len(t, codes, RecoveryCodeCount)
	assert.False(t, svc.HasMFA(context.Background(), 999), "non-existent user should not have MFA")
	assert.True(t, svc.HasMFA(context.Background(), userID))
}

func TestValidateTOTPCode(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "eve")

	setupResult, err := svc.Setup(context.Background(), userID, "eve")
	require.NoError(t, err)
	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)

	_, err = svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)

	// Re-generate for a strictly-future step: Enable consumed the
	// current step (last_used_step), so re-using the same step's
	// code is rejected by Issue 6's replay protection.
	step = time.Now().Unix() / int64(DefaultTOTPPeriod)
	futureStep := step + 1
	validCode := computeTOTP(secret, futureStep)

	valid, err := svc.ValidateTOTPCode(context.Background(), userID, validCode)
	require.NoError(t, err)
	assert.True(t, valid)

	// Wrong code still fails.
	valid, err = svc.ValidateTOTPCode(context.Background(), userID, "000000")
	require.NoError(t, err)
	assert.False(t, valid)
}

func TestValidateRecoveryCode(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "frank")

	setupResult, err := svc.Setup(context.Background(), userID, "frank")
	require.NoError(t, err)
	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)

	codes, err := svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)
	require.Len(t, codes, RecoveryCodeCount)

	// Use the first recovery code.
	valid, err := svc.ValidateRecoveryCode(context.Background(), userID, codes[0])
	require.NoError(t, err)
	assert.True(t, valid)

	// Same code again should fail (single-use).
	valid, err = svc.ValidateRecoveryCode(context.Background(), userID, codes[0])
	require.NoError(t, err)
	assert.False(t, valid)

	// Different unused code should still work.
	valid, err = svc.ValidateRecoveryCode(context.Background(), userID, codes[1])
	require.NoError(t, err)
	assert.True(t, valid)
}

func TestDisable(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "grace")

	setupResult, err := svc.Setup(context.Background(), userID, "grace")
	require.NoError(t, err)
	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)

	_, err = svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)
	require.True(t, svc.HasMFA(context.Background(), userID))

	// Disable with correct password verifier + TOTP code.
	step = time.Now().Unix() / int64(DefaultTOTPPeriod)
	disableCode := computeTOTP(secret, step)
	err = svc.Disable(context.Background(), userID, func(hash string) error {
		return nil // password always valid in test
	}, disableCode)
	require.NoError(t, err)
	assert.False(t, svc.HasMFA(context.Background(), userID))
}

func TestDisable_NoMFAEnabledFails(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "hal")

	err := svc.Disable(context.Background(), userID, func(hash string) error {
		return nil
	}, "123456")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MFA is not enabled")
}

func TestDisable_WrongPasswordFails(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "ivy")

	setupResult, err := svc.Setup(context.Background(), userID, "ivy")
	require.NoError(t, err)
	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)

	_, err = svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)

	step = time.Now().Unix() / int64(DefaultTOTPPeriod)
	disableCode := computeTOTP(secret, step)
	err = svc.Disable(context.Background(), userID, func(hash string) error {
		return assert.AnError
	}, disableCode)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid password")
}

func TestHasMFA(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "judy")

	assert.False(t, svc.HasMFA(context.Background(), userID))

	setupResult, err := svc.Setup(context.Background(), userID, "judy")
	require.NoError(t, err)
	// Setup alone should not make HasMFA true (pending, not enabled).
	assert.False(t, svc.HasMFA(context.Background(), userID))

	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)
	_, err = svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)
	assert.True(t, svc.HasMFA(context.Background(), userID))
}

// TestValidateTOTPCode_RejectsReplay pins Issue 6's replay protection:
// once a step has been accepted, the same step's code is rejected on
// every subsequent call within the same 30s window. Without this gate,
// an attacker who observes a valid code (e.g. via a phishing copy-
// paste) could replay it for the same window.
func TestValidateTOTPCode_RejectsReplay(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "kira")

	setupResult, err := svc.Setup(context.Background(), userID, "kira")
	require.NoError(t, err)
	secret := extractSecret(setupResult.OtpauthURI)
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)
	_, err = svc.Enable(context.Background(), userID, code)
	require.NoError(t, err)

	// First accept advances last_used_step.
	step = time.Now().Unix() / int64(DefaultTOTPPeriod)
	firstCode := computeTOTP(secret, step+1)
	valid, err := svc.ValidateTOTPCode(context.Background(), userID, firstCode)
	require.NoError(t, err)
	assert.True(t, valid)

	// Replaying the SAME code must fail -- the step is no longer
	// strictly greater than last_used_step.
	valid, err = svc.ValidateTOTPCode(context.Background(), userID, firstCode)
	require.NoError(t, err)
	assert.False(t, valid, "replay of an accepted code must be rejected")
}

// TestEnable_RejectsExpiredPendingSecret pins Issue 7's TTL enforcement:
// a pending secret older than PendingSecretTTL is refused at Enable
// time. The test sets up a user, manually rewinds the
// mfa_pending_secret_created_at column past the TTL, and confirms the
// next Enable returns the "expired" error and clears the stale row.
func TestEnable_RejectsExpiredPendingSecret(t *testing.T) {
	svc, database := newTestService(t)
	userID := createTestUser(t, database, "leon")

	_, err := svc.Setup(context.Background(), userID, "leon")
	require.NoError(t, err)

	// Rewind the pending-secret timestamp past PendingSecretTTL.
	// The TTL constant lives in this package; use it directly so a
	// future config knob change is reflected here too.
	stale := time.Now().Add(-2 * PendingSecretTTL).Unix()
	_, err = database.ExecInTx(context.Background(),
		"UPDATE users SET mfa_pending_secret_created_at = ?1 WHERE id = ?2",
		stale, userID,
	)
	require.NoError(t, err)

	secret := ""
	{
		row, err := database.Reader.GetMFAPendingSecret(context.Background(), userID)
		require.NoError(t, err)
		require.True(t, row.MfaPendingSecret.Valid)
		secret, err = testDecrypt(row.MfaPendingSecret.String)
		require.NoError(t, err)
	}
	step := time.Now().Unix() / int64(DefaultTOTPPeriod)
	code := computeTOTP(secret, step)

	_, err = svc.Enable(context.Background(), userID, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")

	// Stale pending row should be cleared by Enable so the next
	// /mfa/setup call isn't blocked by an old envelope.
	row, err := database.Reader.GetMFAPendingSecret(context.Background(), userID)
	require.NoError(t, err)
	assert.False(t, row.MfaPendingSecret.Valid, "Enable should clear the stale pending secret")
}

// TestRecoveryCodes_AreSaltedPerUser pins Issue 8: recovery codes for
// one user must NOT verify against a different user's salt (otherwise
// the per-user salt isn't actually doing anything). Uses the same
// plaintext code twice across two enrolled users with different salts.
func TestRecoveryCodes_AreSaltedPerUser(t *testing.T) {
	svc, database := newTestService(t)
	u1 := createTestUser(t, database, "mia")
	u2 := createTestUser(t, database, "nia")

	enroll := func(t *testing.T, userID int64) []string {
		t.Helper()
		setupResult, err := svc.Setup(context.Background(), userID, "x")
		require.NoError(t, err)
		secret := extractSecret(setupResult.OtpauthURI)
		step := time.Now().Unix() / int64(DefaultTOTPPeriod)
		codes, err := svc.Enable(context.Background(), userID, computeTOTP(secret, step))
		require.NoError(t, err)
		return codes
	}
	u1Codes := enroll(t, u1)
	u2Codes := enroll(t, u2)

	// u1's code must NOT validate against u2 (different salt).
	for _, c := range u1Codes {
		valid, err := svc.ValidateRecoveryCode(context.Background(), u2, c)
		require.NoError(t, err)
		assert.False(t, valid, "u1's recovery code must not validate against u2")
	}
	// u2's code must NOT validate against u1.
	for _, c := range u2Codes {
		valid, err := svc.ValidateRecoveryCode(context.Background(), u1, c)
		require.NoError(t, err)
		assert.False(t, valid, "u2's recovery code must not validate against u1")
	}
}

// testDecrypt is a thin wrapper around the same Box used by newTestService
// so the TTL test can recover the secret without depending on the
// mfa.Service.Open API. Lives here (test only) instead of exporting an
// internal helper.
func testDecrypt(ciphertext string) (string, error) {
	box := testBox(&testing.T{})
	return box.Open(ciphertext)
}

// extractSecret parses the TOTP secret from an otpauth:// URI.
func extractSecret(uri string) string {
	for _, part := range splitPairs(uri) {
		if len(part) > 7 && part[:7] == "secret=" {
			return part[7:]
		}
	}
	return ""
}

func splitPairs(s string) []string {
	q := -1
	for i, c := range s {
		if c == '?' {
			q = i
			break
		}
	}
	if q < 0 {
		return nil
	}
	s = s[q+1:]
	var result []string
	start := 0
	for i, c := range s {
		if c == '&' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}
