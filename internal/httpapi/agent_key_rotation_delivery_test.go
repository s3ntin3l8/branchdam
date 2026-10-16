package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/pairing"
)

// assertRotationSecretsCleared checks no key of the pairing still holds a
// sealed delivery copy.
func assertRotationSecretsCleared(t *testing.T, svc *pairing.Service, pairingID int64) {
	t.Helper()
	keys, err := svc.ListKeys(context.Background(), pairingID)
	require.NoError(t, err)
	for _, k := range keys {
		assert.False(t, k.RotationSealedKey.Valid, "key %d still holds a sealed rotation copy", k.ID)
	}
}

// TestAgentKeyRotationRepeatedRotation: two rotations before the device
// checks in deliver only the newest key, drop the superseded one's sealed
// copy, and adopting the newest retires both older keys.
func TestAgentKeyRotationRepeatedRotation(t *testing.T) {
	srv, _, pairSvc := newPairingTestServerWithBox(t, rotationTestBox(t))
	ctx := context.Background()
	p, k1, err := pairSvc.CreatePairing(ctx, "Pixel", "test-admin", 0, qrPayload)
	require.NoError(t, err)
	k2, _, err := pairSvc.RotateKey(ctx, p.ID, "test-admin", 60, qrPayload)
	require.NoError(t, err)
	k3, _, err := pairSvc.RotateKey(ctx, p.ID, "test-admin", 60, qrPayload)
	require.NoError(t, err)

	keys, err := pairSvc.ListKeys(ctx, p.ID)
	require.NoError(t, err)
	for _, k := range keys {
		assert.Equal(t, k.ID == k3.ID, k.RotationSealedKey.Valid, "key %d sealed copy", k.ID)
	}

	code, hs := handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, hs.PendingRotation)
	assert.Equal(t, k3.ID, hs.PendingRotation.KeyID)
	assert.Equal(t, k3.Plaintext, pairingOpen(t, k1.Plaintext, hs.PendingRotation.SealedAPIKey))

	code, _ = handshakeWith(t, srv, p.AgentID, k3.Plaintext)
	require.Equal(t, http.StatusOK, code)
	for _, old := range []string{k1.Plaintext, k2.Plaintext} {
		code, _ = handshakeWith(t, srv, p.AgentID, old)
		assert.Equal(t, http.StatusUnauthorized, code)
	}
	assertRotationSecretsCleared(t, pairSvc, p.ID)

	audit, err := pairSvc.ListAudit(ctx, p.ID, 10, 0)
	require.NoError(t, err)
	adopted := 0
	for _, a := range audit {
		if a.Event == "KEY_ADOPTED" {
			adopted++
		}
	}
	assert.Equal(t, 1, adopted, "exactly one KEY_ADOPTED audit row")
}

// pairingOpen is the server package's own reference opener; it must agree
// with the standard-library one above.
func pairingOpen(t *testing.T, currentKey, sealed string) string {
	t.Helper()
	plain, err := pairing.OpenRotatedKey(currentKey, sealed)
	require.NoError(t, err)
	require.Equal(t, plain, openSealedRotationKey(t, currentKey, sealed))
	return plain
}
