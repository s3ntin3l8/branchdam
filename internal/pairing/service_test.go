package pairing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

// testPepper is the HMAC key used by the test helper and newTestService.
// Must match the pepper passed to NewService in newTestService.
var testPepper = sha256.Sum256([]byte("test-pepper"))

// testSecretBase64 is a fixed 32-byte key for secrets.Box in tests.
// Deterministic so seal/open round-trips across helpers are stable.
const testSecretBase64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	return newTestServiceWithBox(t, nil)
}

// newTestServiceWithBox is newTestService with an explicit secrets.Box
// (nil = keyless server: plaintext qr_svg, NULL pairing_url).
func newTestServiceWithBox(t *testing.T, box *secrets.Box) (*Service, *db.DB) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "pairing.db")
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return NewService(database, nil, testPepper[:], box), database
}

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(testSecretBase64)
	require.NoError(t, err)
	require.NotNil(t, box)
	return box
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

	result, err := svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, pairing.AgentID, result.AgentID)
	// The signing key must be non-empty for an authenticated hit -- it's
	// the HMAC key AgentChain uses to validate signed requests from this
	// device (issue #453 PR C). Length 32 == SHA-256 output bytes.
	assert.Len(t, result.SigningKey, 32)
}

func TestKeyLookup_UnknownKeyReturnsEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	result, err := svc.KeyLookup(ctx, "no-such-key-anywhere")
	require.NoError(t, err)
	assert.Empty(t, result.AgentID)
	assert.Nil(t, result.SigningKey)
}

func TestKeyLookup_RevokedPairingReturnsEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pairing, key, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	_, err = svc.RevokePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)

	result, err := svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, result.AgentID)
	assert.Nil(t, result.SigningKey)
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
	oldResult, err := svc.KeyLookup(ctx, oldKey.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, pairing.AgentID, oldResult.AgentID)
	assert.Len(t, oldResult.SigningKey, 32)

	// New key should also work.
	newResult, err := svc.KeyLookup(ctx, newKey.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, pairing.AgentID, newResult.AgentID)
	assert.Len(t, newResult.SigningKey, 32)
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

	result, err := svc.KeyLookup(ctx, oldKey.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, result.AgentID, "expired key must miss KeyLookup")
	assert.Nil(t, result.SigningKey)
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
		result, err := svc.KeyLookup(ctx, plaintext)
		require.NoError(t, err)
		assert.Empty(t, result.AgentID, "revoked pairing's keys must not authenticate")
		assert.Nil(t, result.SigningKey)
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
	result, err := svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, result.AgentID)

	// After delete, still fails (no regression)
	err = svc.DeletePairing(ctx, pairing.ID, "user:tester")
	require.NoError(t, err)
	result, err = svc.KeyLookup(ctx, key.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, result.AgentID)
}

// --- credential sealing / reveal / rename (feature: show credentials) ---

func TestCreatePairing_SealsCredentialsWithBox(t *testing.T) {
	svc, database := newTestServiceWithBox(t, testBox(t))
	ctx := context.Background()

	p, key, err := svc.CreatePairing(ctx, "Sealed iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	row, err := database.Reader.GetDevicePairingByID(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, secrets.IsSealed(row.QrSvg), "qr_svg must be sealed at rest")
	require.True(t, row.PairingUrl.Valid, "pairing_url must be stored when box is set")
	require.True(t, secrets.IsSealed([]byte(row.PairingUrl.String)), "pairing_url must be sealed")

	// Read path decrypts back to the original payload.
	creds, err := svc.ActiveCredentials(ctx, p.ID)
	require.NoError(t, err)
	assert.Contains(t, string(creds.QRSVG), "<svg")
	assert.True(t, strings.HasPrefix(creds.PairingURL, "branchdam://"), "got %q", creds.PairingURL)
	assert.Contains(t, creds.PairingURL, key.Plaintext)
}

func TestCreatePairing_KeylessStoresPlaintextSVGNullURL(t *testing.T) {
	svc, database := newTestService(t)
	ctx := context.Background()

	p, _, err := svc.CreatePairing(ctx, "Keyless", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	row, err := database.Reader.GetDevicePairingByID(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, secrets.IsSealed(row.QrSvg), "keyless server stores plaintext SVG")
	require.False(t, row.PairingUrl.Valid, "keyless server never writes pairing_url")

	creds, err := svc.ActiveCredentials(ctx, p.ID)
	require.NoError(t, err)
	assert.Empty(t, creds.PairingURL)
	assert.Contains(t, string(creds.QRSVG), "<svg")
}

func TestRotateKey_ReSealsCredentials(t *testing.T) {
	svc, database := newTestServiceWithBox(t, testBox(t))
	ctx := context.Background()

	p, oldKey, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	newKey, _, err := svc.RotateKey(ctx, p.ID, "user:tester", 60, stubQRPayload)
	require.NoError(t, err)

	creds, err := svc.ActiveCredentials(ctx, p.ID)
	require.NoError(t, err)
	assert.Contains(t, creds.PairingURL, newKey.Plaintext, "credentials must reflect the rotated key")
	assert.NotContains(t, creds.PairingURL, oldKey.Plaintext)

	row, err := database.Reader.GetDevicePairingByID(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, secrets.IsSealed(row.QrSvg))
	require.True(t, row.PairingUrl.Valid)
}

func TestActiveCredentials_RevokedPairingReturnsNoActiveKey(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	p, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	_, err = svc.RevokePairing(ctx, p.ID, "user:tester")
	require.NoError(t, err)

	_, err = svc.ActiveCredentials(ctx, p.ID)
	assert.ErrorIs(t, err, ErrNoActiveKey)
}

func TestRenamePairing_UpdatesLabelAndAudits(t *testing.T) {
	svc, database := newTestService(t)
	ctx := context.Background()

	p, _, err := svc.CreatePairing(ctx, "Old name", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	renamed, err := svc.RenamePairing(ctx, p.ID, "New name", "user:tester")
	require.NoError(t, err)
	assert.Equal(t, "New name", renamed.FriendlyLabel)

	// Persisted label.
	got, err := svc.GetPairing(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, "New name", got.FriendlyLabel)

	// LABEL_RENAMED audit row with old/new in details.
	events, err := database.Reader.ListPairingAudit(ctx, sqlcgen.ListPairingAuditParams{
		PairingID: p.ID,
		Limit:     10,
		Offset:    0,
	})
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Event == "LABEL_RENAMED" {
			found = true
			assert.Contains(t, e.Details, `"old":"Old name"`)
			assert.Contains(t, e.Details, `"new":"New name"`)
		}
	}
	assert.True(t, found, "expected LABEL_RENAMED audit event")
}

func TestRenamePairing_EmptyLabelRejected(t *testing.T) {
	svc, _ := newTestService(t)
	p, _, err := svc.CreatePairing(context.Background(), "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)

	_, err = svc.RenamePairing(context.Background(), p.ID, "   ", "user:tester")
	assert.ErrorIs(t, err, ErrEmptyLabel)
}

func TestRenamePairing_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RenamePairing(context.Background(), 99999, "x", "user:tester")
	assert.ErrorIs(t, err, ErrPairingNotFound)
}

func TestRenamePairing_AllowedOnRevokedPairing(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	p, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	_, err = svc.RevokePairing(ctx, p.ID, "user:tester")
	require.NoError(t, err)

	renamed, err := svc.RenamePairing(ctx, p.ID, "Retired iPhone", "user:tester")
	require.NoError(t, err)
	assert.Equal(t, "Retired iPhone", renamed.FriendlyLabel)
}

func TestRecordCredentialReveal_WritesAudit(t *testing.T) {
	svc, database := newTestService(t)
	ctx := context.Background()

	p, _, err := svc.CreatePairing(ctx, "iPhone", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	require.NoError(t, svc.RecordCredentialReveal(ctx, p.ID, "user:tester", "qr_svg"))

	events, err := database.Reader.ListPairingAudit(ctx, sqlcgen.ListPairingAuditParams{
		PairingID: p.ID,
		Limit:     10,
		Offset:    0,
	})
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Event == "CREDENTIALS_REVEALED" {
			found = true
			assert.Contains(t, e.Details, `"channel":"qr_svg"`)
		}
	}
	assert.True(t, found, "expected CREDENTIALS_REVEALED audit event")
}

func TestBackfillSealedCredentials_SealsLegacyPlaintext(t *testing.T) {
	svc, database := newTestServiceWithBox(t, testBox(t))
	ctx := context.Background()

	// Seed a legacy plaintext row directly (pre-sealing behavior).
	p, _, err := svc.CreatePairing(ctx, "Legacy", "user:tester", 0, stubQRPayload)
	require.NoError(t, err)
	// Force plaintext QR into the row (simulating a pre-seal write).
	plaintextSVG := []byte("<?xml version='1.0'?><svg></svg>")
	_, err = database.ExecInTx(ctx,
		"UPDATE device_pairings SET qr_svg = ?2, pairing_url = NULL WHERE id = ?1",
		p.ID, plaintextSVG)
	require.NoError(t, err)

	sealed, err := svc.BackfillSealedCredentials(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, sealed)

	row, err := database.Reader.GetDevicePairingByID(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, secrets.IsSealed(row.QrSvg), "legacy plaintext QR must be sealed")

	// Idempotent: second run seals nothing.
	sealed, err = svc.BackfillSealedCredentials(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, sealed)
}

func TestBackfillSealedCredentials_KeylessNoop(t *testing.T) {
	svc, _ := newTestService(t)
	sealed, err := svc.BackfillSealedCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, sealed)
}
