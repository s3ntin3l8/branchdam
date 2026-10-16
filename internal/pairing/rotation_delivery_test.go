package pairing

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

func rotPayload(agentID, apiKey string) []byte {
	return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
}

// TestSealForDeviceOnlyOpensWithCurrentKey: the transport seal opens with
// the device's current key and with nothing else, and never contains the
// plaintext.
func TestSealForDeviceOnlyOpensWithCurrentKey(t *testing.T) {
	sealed, err := SealForDevice("current-key-0123456789", "new-key-abcdef")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sealed, "v1:"))
	assert.NotContains(t, sealed, "new-key-abcdef")

	plain, err := OpenRotatedKey("current-key-0123456789", sealed)
	require.NoError(t, err)
	assert.Equal(t, "new-key-abcdef", plain)

	_, err = OpenRotatedKey("some-other-key", sealed)
	assert.ErrorIs(t, err, secrets.ErrDecryptFailed)
}

// TestRotateKeyStoresSealedCopyOnlyWithBox: a rotation-minted key awaits
// adoption either way, but its plaintext is kept (sealed under the
// server's box, never in the clear) only when a box is configured.
func TestRotateKeyStoresSealedCopyOnlyWithBox(t *testing.T) {
	ctx := context.Background()
	for _, withBox := range []bool{true, false} {
		var box *secrets.Box
		if withBox {
			box = testBox(t)
		}
		svc, _ := newTestServiceWithBox(t, box)
		p, k1, err := svc.CreatePairing(ctx, "dev", "test", 0, rotPayload)
		require.NoError(t, err)
		k2, _, err := svc.RotateKey(ctx, p.ID, "test", 60, rotPayload)
		require.NoError(t, err)

		keys, err := svc.ListKeys(ctx, p.ID)
		require.NoError(t, err)
		for _, k := range keys {
			switch k.ID {
			case k1.ID:
				assert.Zero(t, k.AwaitingAdoption)
				assert.False(t, k.RotationSealedKey.Valid)
			case k2.ID:
				assert.Equal(t, int64(1), k.AwaitingAdoption)
				assert.Equal(t, withBox, k.RotationSealedKey.Valid, "sealed copy stored iff a box is configured")
				if withBox {
					assert.NotContains(t, k.RotationSealedKey.String, k2.Plaintext)
					plain, err := box.Open(k.RotationSealedKey.String)
					require.NoError(t, err)
					assert.Equal(t, k2.Plaintext, plain)
				}
			}
		}
	}
}

// TestPendingRotationForAndAdoption covers the service half of S13:
// PendingRotationFor reports the newer key sealed for the presenting
// device, KeyLookup with the new key adopts it (once), and the old key
// stops resolving.
func TestPendingRotationForAndAdoption(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestServiceWithBox(t, testBox(t))
	p, k1, err := svc.CreatePairing(ctx, "dev", "test", 0, rotPayload)
	require.NoError(t, err)

	rot, err := svc.PendingRotationFor(ctx, p.AgentID, k1.Plaintext)
	require.NoError(t, err)
	require.NotNil(t, rot)
	assert.Equal(t, k1.ID, rot.CurrentKeyID)
	assert.Zero(t, rot.KeyID)

	k2, graceEnd, err := svc.RotateKey(ctx, p.ID, "test", 60, rotPayload)
	require.NoError(t, err)
	rot, err = svc.PendingRotationFor(ctx, p.AgentID, k1.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, k2.ID, rot.KeyID)
	assert.Equal(t, graceEnd, rot.PreviousKeyExpiresAt)
	plain, err := OpenRotatedKey(k1.Plaintext, rot.SealedAPIKey)
	require.NoError(t, err)
	assert.Equal(t, k2.Plaintext, plain)

	// Another device's agent_id gets nothing for this key.
	rot, err = svc.PendingRotationFor(ctx, "someone-else", k1.Plaintext)
	require.NoError(t, err)
	assert.Nil(t, rot)

	for range 2 { // second lookup must be a no-op, not a second adoption
		res, err := svc.KeyLookup(ctx, k2.Plaintext)
		require.NoError(t, err)
		assert.Equal(t, p.AgentID, res.AgentID)
	}
	res, err := svc.KeyLookup(ctx, k1.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, res.AgentID, "old key must not resolve after adoption")

	audit, err := svc.ListAudit(ctx, p.ID, 20, 0)
	require.NoError(t, err)
	adopted := 0
	for _, a := range audit {
		if a.Event == "KEY_ADOPTED" {
			adopted++
		}
	}
	assert.Equal(t, 1, adopted)
}

// TestRevokePairingClearsSealedRotationCopy: revoking a pairing drops a
// pending rotation's sealed copy, so a revoked pairing's key can never be
// delivered (RevokeAllKeysForPairing sets rotation_sealed_key = NULL).
func TestRevokePairingClearsSealedRotationCopy(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestServiceWithBox(t, testBox(t))
	p, _, err := svc.CreatePairing(ctx, "dev", "test", 0, rotPayload)
	require.NoError(t, err)
	k2, _, err := svc.RotateKey(ctx, p.ID, "test", 60, rotPayload)
	require.NoError(t, err)

	sealedFor := func() bool {
		keys, err := svc.ListKeys(ctx, p.ID)
		require.NoError(t, err)
		for _, k := range keys {
			if k.ID == k2.ID {
				return k.RotationSealedKey.Valid
			}
		}
		t.Fatalf("rotated key %d not listed", k2.ID)
		return false
	}
	require.True(t, sealedFor(), "precondition: rotation stored a sealed copy")

	_, err = svc.RevokePairing(ctx, p.ID, "test")
	require.NoError(t, err)
	assert.False(t, sealedFor(), "revoke must clear the sealed rotation copy")
}

// TestPendingRotationWithUnopenableSealedCopy: when the stored copy no
// longer opens (BRANCHDAM_SECRET_KEY changed since the rotation), the
// handshake still reports the rotation's keyId but no sealedApiKey, so
// the device falls back to re-pairing instead of getting an error.
func TestPendingRotationWithUnopenableSealedCopy(t *testing.T) {
	ctx := context.Background()
	svc, database := newTestServiceWithBox(t, testBox(t))
	p, k1, err := svc.CreatePairing(ctx, "dev", "test", 0, rotPayload)
	require.NoError(t, err)
	k2, _, err := svc.RotateKey(ctx, p.ID, "test", 60, rotPayload)
	require.NoError(t, err)

	otherBox, err := secrets.NewBox("ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	require.NoError(t, err)
	restarted := NewService(database, nil, testPepper[:], otherBox)

	rot, err := restarted.PendingRotationFor(ctx, p.AgentID, k1.Plaintext)
	require.NoError(t, err)
	require.NotNil(t, rot)
	assert.Equal(t, k1.ID, rot.CurrentKeyID)
	assert.Equal(t, k2.ID, rot.KeyID)
	assert.Empty(t, rot.SealedAPIKey, "an unopenable copy must not be delivered")
}
