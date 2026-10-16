package httpapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

// openSealedRotationKey is a client's half of the S13 contract
// (docs/agent-api.md), written out with the standard library only so it
// doubles as a reference for branchdam-agent and branchdam-mobile:
// AES-256-GCM, key = HMAC-SHA256(key = current API key, message =
// "branchdam-key-rotation-v1"), sealed = "v1:" + base64(nonce(12) ||
// ciphertext || tag(16)).
func openSealedRotationKey(t *testing.T, currentKey, sealed string) string {
	t.Helper()
	require.True(t, strings.HasPrefix(sealed, "v1:"), "sealed key %q lacks the v1: prefix", sealed)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	require.NoError(t, err)
	mac := hmac.New(sha256.New, []byte(currentKey))
	mac.Write([]byte("branchdam-key-rotation-v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	require.Greater(t, len(raw), gcm.NonceSize())
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	require.NoError(t, err, "sealed key does not open with the current API key")
	return string(plain)
}

type rotationHandshake struct {
	CurrentKeyID    int64 `json:"currentKeyId"`
	PendingRotation *struct {
		KeyID                int64  `json:"keyId"`
		APIKey               string `json:"apiKey"`
		SealedAPIKey         string `json:"sealedApiKey"`
		PreviousKeyExpiresAt int64  `json:"previousKeyExpiresAtUnix"`
	} `json:"pendingRotation"`
}

// handshakeWith runs POST /api/v1/agent/handshake authenticated with
// apiKey, returning the status and (on 200) the decoded body.
func handshakeWith(t *testing.T, srv *Server, agentID, apiKey string) (int, rotationHandshake) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"agentId": agentID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var hs rotationHandshake
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hs), "body=%s", rec.Body.String())
	}
	return rec.Code, hs
}

func rotationTestBox(t *testing.T) *secrets.Box {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(k))
	require.NoError(t, err)
	return box
}

func qrPayload(agentID, apiKey string) []byte {
	return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
}

// TestAgentKeyRotationRoundTrip is S13's end-to-end contract: after an
// admin rotates a device key, the device's next handshake (still on the old
// key, inside the grace window) names its current key and carries the new
// key sealed for it; the device opens it, authenticates with it -- which
// adopts it -- and from then on the old key is refused even though its
// grace window has not run out.
func TestAgentKeyRotationRoundTrip(t *testing.T) {
	srv, _, pairSvc := newPairingTestServerWithBox(t, rotationTestBox(t))
	ctx := context.Background()
	p, k1, err := pairSvc.CreatePairing(ctx, "Pixel", "test-admin", 0, qrPayload)
	require.NoError(t, err)

	code, hs := handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, k1.ID, hs.CurrentKeyID)
	assert.Nil(t, hs.PendingRotation, "no rotation yet")

	k2, graceEnd, err := pairSvc.RotateKey(ctx, p.ID, "test-admin", 60, qrPayload)
	require.NoError(t, err)

	// Old key, inside the grace window: still accepted, and told about K2.
	code, hs = handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, k1.ID, hs.CurrentKeyID)
	require.NotNil(t, hs.PendingRotation)
	assert.Equal(t, k2.ID, hs.PendingRotation.KeyID)
	assert.Empty(t, hs.PendingRotation.APIKey, "the new key is never sent in the clear")
	assert.Equal(t, graceEnd, hs.PendingRotation.PreviousKeyExpiresAt)
	require.NotEmpty(t, hs.PendingRotation.SealedAPIKey)
	assert.NotContains(t, hs.PendingRotation.SealedAPIKey, k2.Plaintext)
	assert.Equal(t, k2.Plaintext, openSealedRotationKey(t, k1.Plaintext, hs.PendingRotation.SealedAPIKey))

	// Adoption: the first request on the new key.
	code, hs = handshakeWith(t, srv, p.AgentID, k2.Plaintext)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, k2.ID, hs.CurrentKeyID)
	assert.Nil(t, hs.PendingRotation)

	// The old key is retired now, well before graceEnd.
	require.Greater(t, graceEnd, time.Now().Unix())
	code, _ = handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, code, "old key must be refused once the device adopted the new one")
	code, _ = handshakeWith(t, srv, p.AgentID, k2.Plaintext)
	assert.Equal(t, http.StatusOK, code)
}

// TestAgentKeyRotationGraceExpiry: a device that never picks up the
// rotation loses its old key when the grace window ends -- it can no
// longer fetch the new key and has to be re-paired; the new key works.
func TestAgentKeyRotationGraceExpiry(t *testing.T) {
	srv, database, pairSvc := newPairingTestServerWithBox(t, rotationTestBox(t))
	ctx := context.Background()
	p, k1, err := pairSvc.CreatePairing(ctx, "Pixel", "test-admin", 0, qrPayload)
	require.NoError(t, err)
	k2, _, err := pairSvc.RotateKey(ctx, p.ID, "test-admin", 60, qrPayload)
	require.NoError(t, err)

	code, _ := handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	require.Equal(t, http.StatusOK, code, "old key works inside the grace window")

	// The grace window ends.
	_, err = database.ExecInTx(ctx, `UPDATE device_pairing_keys SET expires_at = ? WHERE id = ?`, time.Now().Unix()-1, k1.ID)
	require.NoError(t, err)

	code, _ = handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, code, "old key must be refused after the grace window")
	code, hs := handshakeWith(t, srv, p.AgentID, k2.Plaintext)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, k2.ID, hs.CurrentKeyID)
}

// TestAgentKeyRotationKeylessServer: without BRANCHDAM_SECRET_KEY the new
// key is never stored, so the handshake names it but cannot deliver it;
// adoption after a QR re-pair still retires the old key.
func TestAgentKeyRotationKeylessServer(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()
	p, k1, err := pairSvc.CreatePairing(ctx, "Pixel", "test-admin", 0, qrPayload)
	require.NoError(t, err)
	k2, _, err := pairSvc.RotateKey(ctx, p.ID, "test-admin", 60, qrPayload)
	require.NoError(t, err)

	code, hs := handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, hs.PendingRotation)
	assert.Equal(t, k2.ID, hs.PendingRotation.KeyID)
	assert.Empty(t, hs.PendingRotation.SealedAPIKey)

	code, _ = handshakeWith(t, srv, p.AgentID, k2.Plaintext)
	require.Equal(t, http.StatusOK, code)
	code, _ = handshakeWith(t, srv, p.AgentID, k1.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, code)
}
