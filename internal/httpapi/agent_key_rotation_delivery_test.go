package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/pairing"
	"github.com/s3ntin3l8/branchdam/internal/sse"
)

// assertRotationSecretsCleared checks no key of the pairing still holds a
// sealed delivery copy.
func assertRotationSecretsCleared(t *testing.T, svc *pairing.Service, pairingID int64) {
	t.Helper()
	keys, err := svc.ListKeys(context.Background(), pairingID)
	require.NoError(t, err)
	for _, k := range keys {
		assert.False(t, k.RotationSealedKey.Valid, "key %d still holds a sealed rotation copy", k.ID)
		assert.Zero(t, k.AwaitingAdoption, "key %d still reads as awaiting adoption", k.ID)
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

// TestHandshakePendingRotationLookupFailureIs500: a DB failure while
// resolving the pending rotation is a 500 in the agent error envelope
// (INTERNAL_ERROR), not a handshake that silently drops the hint.
func TestHandshakePendingRotationLookupFailureIs500(t *testing.T) {
	mainDB, err := db.Open(context.Background(), t.TempDir()+"/main.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainDB.Close() })
	pairDB, err := db.Open(context.Background(), t.TempDir()+"/pairing.db")
	require.NoError(t, err)
	require.NoError(t, pairDB.Close()) // every pairing lookup now fails

	srv := New(Deps{
		Config:         &config.Config{Agent: config.Agent{}},
		DB:             mainDB,
		Hub:            sse.New(),
		Version:        "test",
		Pairing:        pairing.NewService(pairDB, nil, nil, nil),
		agentKeyLookup: NewStubAgentKeyLookup(map[string]string{"stub-key": "pixel"}),
	})
	body, err := json.Marshal(map[string]any{"agentId": "pixel"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "stub-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"INTERNAL_ERROR"`)
}
