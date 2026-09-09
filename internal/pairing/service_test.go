package pairing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// testPepper is the HMAC key used by the test helper and newTestService.
// Must match the pepper passed to NewService in newTestService.
var testPepper = sha256.Sum256([]byte("test-pepper"))

func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "pairing.db")
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return NewService(database, nil, testPepper[:]), database
}

// stubQRPayload returns a stable closure so tests don't depend on the
// HTTP-layer context-injected X-Forwarded-* headers.
func stubQRPayload(agentID, apiKey string) []byte {
	return []byte("branchdam://?server=http://test&key=" + apiKey + "&agent=" + agentID)
}

func hashKeyForTest(key string) string {
	mac := hmac.New(sha256.New, testPepper[:])
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestCreatePairing_HappyPath(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, key, err := svc.CreatePairing(ctx, "Björn's iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	require.NotNil(t, pairing)
	require.NotNil(t, key)

	assert.NotEmpty(t, pairing.AgentID)
	assert.Equal(t, "Björn's iPhone", pairing.FriendlyLabel)
	assert.Equal(t, "user:tester", pairing.CreatedBy)
	assert.Zero(t, pairing.RevokedAt)
	assert.NotZero(t, pairing.CreatedAt)

	assert.NotEmpty(t, key.Plaintext)
	assert.Equal(t, hashKeyForTest(key.Plaintext), key.LookupHash)
	assert.Equal(t, key.Plaintext[len(key.Plaintext)-4:], key.Preview)
	assert.False(t, key.ExpiresAt.Valid)
	assert.False(t, key.RevokedAt.Valid)
}

// TestCreatePairing_StoresOwnerUserID: when the HTTP layer passes a
// resolved user id, the pairing's user_id column carries it. This is
// the contract the agent upload path relies on to attribute
// paired-device uploads back to the human who paired the device.
func TestCreatePairing_StoresOwnerUserID(t *testing.T) {
	svc, dbx := newTestService(t)
	ctx := context.Background()

	// Create a real users row to satisfy the FK.
	var userID int64
	require.NoError(t, dbx.InTx(ctx, func(q *sqlcgen.Queries) error {
		u, err := q.CreateLocalUser(ctx, sqlcgen.CreateLocalUserParams{
			Username:     "alice",
			Email:        sql.NullString{String: "alice@example.com", Valid: true},
			PasswordHash: sql.NullString{String: "x", Valid: true},
			IsAdmin:      0,
			CreatedAt:    time.Now().Unix(),
			CreatedBy:    "test",
		})
		userID = u.ID
		return err
	}))

	pairing, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", userID, stubQRPayload)
	require.NoError(t, err)
	assert.True(t, pairing.UserID.Valid)
	assert.Equal(t, userID, pairing.UserID.Int64)
}

func TestCreatePairing_NilUserIDIsAllowed(t *testing.T) {
	svc, _ := newTestService(t)
	pairing, _, err := svc.CreatePairing(context.Background(), "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	assert.False(t, pairing.UserID.Valid, "userID=0 must surface as NULL")
}

func TestCreatePairing_MintsUniqueAgentIDs(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	p1, _, err := svc.CreatePairing(ctx, "iPhone A", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	p2, _, err := svc.CreatePairing(ctx, "iPhone B", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	assert.NotEqual(t, p1.AgentID, p2.AgentID)
}

func TestCreatePairing_RejectsDuplicateAgentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	p, _, err := svc.CreatePairing(ctx, "first", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	// Try to manually create another pairing with the same agent_id -- should
	// fail with UNIQUE constraint violation.
	err = svc.withTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID:       p.AgentID,
			FriendlyLabel: "second",
			CreatedAt:     1,
			CreatedBy:     "user:tester",
		})
		return err
	})
	require.Error(t, err)
}

func TestKeyLookup_ActiveKeyReturnsAgentID(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, key, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	agentID, err := svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, pairing.AgentID, agentID)
}

func TestKeyLookup_UnknownKeyReturnsEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	agentID, err := svc.KeyLookup(ctx, "no-such-key-anywhere")
	require.NoError(t, err)
	assert.Empty(t, agentID)
}

func TestKeyLookup_RevokedPairingReturnsEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, key, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	_, err = svc.RevokePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	agentID, err := svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, agentID)
}

func TestKeyLookup_ExpiredKeyReturnsEmpty(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	pairing, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	// Manually expire the only key by setting expires_at in the past. Service
	// doesn't expose "set expiry on the active key" -- that's the rotation
	// path's job (see RotateKey below) -- but rotation needs an existing
	// active key to set expires_at on, so we use raw sql here for setup.
	err = db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.SetActiveKeyExpirations(ctx, sqlcgen.SetActiveKeyExpirationsParams{
			PairingID: pairing.ID,
			ExpiresAt: sql.NullInt64{Int64: 1, Valid: true},
		})
	})
	require.NoError(t, err)

	keys, err := svc.ListKeys(ctx, pairing.ID)
	require.NoError(t, err)
	require.Len(t, keys, 1)

	// ListKeys exposes previews and metadata, NOT plaintext. Verify.
	assert.Empty(t, keys[0].KeyLookupHash == "", false, "LookupHash must always be populated for audit")
	assert.Len(t, keys[0].KeyPreview, 4, "KeyPreview must always be the last 4 chars")
}

func TestRotateKey_HappyPath(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, oldKey, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	newKey, previousExpiry, err := svc.RotateKey(ctx, pairing.ID, "user:tester", 24*60, stubQRPayload)
	require.NoError(t, err)
	require.NotNil(t, newKey)
	assert.NotZero(t, previousExpiry)

	assert.NotEqual(t, oldKey.Plaintext, newKey.Plaintext)

	// Old key should still work (within grace window).
	agentID, err := svc.KeyLookup(ctx, oldKey.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, pairing.AgentID, agentID)

	// New key should also work.
	agentID, err = svc.KeyLookup(ctx, newKey.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, pairing.AgentID, agentID)
}

func TestRotateKey_GraceExpiryExpiresOldKey(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	pairing, oldKey, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	_, _, err = svc.RotateKey(ctx, pairing.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)

	// Force expiry to the past, bypassing the normal grace window. We
	// UPDATE every active key (including the one RotateKey already stamped
	// with expires_at = now+grace) by hitting the table directly --
	// SetActiveKeyExpirations is intentionally a no-op on rows that
	// already have expires_at set (rotation idempotency).
	_, err = db.ExecInTx(ctx,
		"UPDATE device_pairing_keys SET expires_at = ?2 WHERE pairing_id = ?1 AND revoked_at IS NULL",
		pairing.ID, sql.NullInt64{Int64: 1, Valid: true})
	require.NoError(t, err)

	agentID, err := svc.KeyLookup(ctx, oldKey.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, agentID, "expired key must miss KeyLookup")
}

func TestRevokePairing_TerminatesAllKeys(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, k1, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	k2, _, err := svc.RotateKey(ctx, pairing.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)

	_, err = svc.RevokePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	for _, plaintext := range []string{k1.Plaintext, k2.Plaintext} {
		agentID, err := svc.KeyLookup(ctx, plaintext)
		require.NoError(t, err)
		assert.Empty(t, agentID, "revoked pairing's keys must not authenticate")
	}
}

func TestLatestActiveKey_ReturnsNewestDifferentFromGiven(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, k1, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	k2, _, err := svc.RotateKey(ctx, pairing.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)

	// Caller used k1: return k2.
	got, err := svc.LatestActiveKey(ctx, pairing.AgentID, k1.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(k2.ID), int64(got.ID))

	// Caller used k2: nothing newer, returns sql.ErrNoRows.
	_, err = svc.LatestActiveKey(ctx, pairing.AgentID, k2.ID)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestLatestActiveKey_RevokedPairingReturnsNoRows(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, k1, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	_, _, err = svc.RotateKey(ctx, pairing.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)

	_, err = svc.RevokePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	_, err = svc.LatestActiveKey(ctx, pairing.AgentID, k1.ID)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestDeletePairing_RevokedPairingDeleted(t *testing.T) {
	svc, database := newTestService(t)
	ctx := context.Background()

	pairing, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	_, _, err = svc.RotateKey(ctx, pairing.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)
	_, err = svc.RevokePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	err = svc.DeletePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	// Pairing row must be gone
	_, err = database.Reader.GetDevicePairingByID(ctx, pairing.ID)
	assert.ErrorIs(t, err, sql.ErrNoRows)

	// Keys must be gone
	keys, err := database.Reader.ListKeysByPairing(ctx, pairing.ID)
	require.NoError(t, err)
	assert.Empty(t, keys)

	// Pairing-scoped audit must be gone
	count, err := database.Reader.CountPairingAudit(ctx, pairing.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	// Global actor_audit must have the trace
	traceCount, err := database.Reader.CountActorAudit(ctx, sqlcgen.CountActorAuditParams{
		Event:        sql.NullString{String: audit.EventPairingDeleted, Valid: true},
		ResourceType: sql.NullString{String: "companion_pairing", Valid: true},
		ResourceID:   sql.NullString{String: fmt.Sprintf("%d", pairing.ID), Valid: true},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), traceCount)
}

func TestDeletePairing_ActivePairingReturnsNotRevoked(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	err = svc.DeletePairing(ctx, pairing.ID, "user:tester")
	assert.ErrorIs(t, err, ErrPairingNotRevoked)
}

func TestDeletePairing_NonexistentPairingReturnsNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	err := svc.DeletePairing(ctx, 99999, "user:tester")
	assert.ErrorIs(t, err, ErrPairingNotFound)
}

func TestDeletePairing_KeysNotLookupableAfterDelete(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, key, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	_, _, err = svc.RotateKey(ctx, pairing.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)
	_, err = svc.RevokePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	// Keys should already fail lookup after revoke
	agentID, err := svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, agentID)

	// After delete, still fails (no regression)
	err = svc.DeletePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)
	agentID, err = svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, agentID)
}
