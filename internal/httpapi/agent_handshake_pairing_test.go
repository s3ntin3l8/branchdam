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
)

// TestAgentHandshake_PairedDeviceCrossCheck verifies a paired device
// that sends a body.agentId matching its own Principal passes (200),
// and one that sends a mismatched agentId is rejected with 403.
// This is the device-pairing authentication boundary's first real
// security gate -- without it, a paired device could impersonate
// another device in /agent/handshake.
func TestAgentHandshake_PairedDeviceCrossCheck(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()

	// Create a paired device. The plaintext returned is the API key
	// the device would use to authenticate.
	p, key, err := pairSvc.CreatePairing(ctx, "iPhone A", "test-admin", func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)

	// Handshake with matching agentId -- 200.
	body := bytes.NewBufferString(`{"agentId":"` + p.AgentID + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key.Plaintext)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Handshake with mismatched agentId -- 403.
	body = bytes.NewBufferString(`{"agentId":"some-other-agent"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key.Plaintext)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestAgentHandshake_EnvBootstrapAllowsAnyAgentId verifies the env-var
// bootstrap path (the legacy workstation agent) doesn't get its body
// agentId rejected -- it has no per-device claim, so any agent_id in
// the body is fine. This is the migration path: existing operators
// don't break when they upgrade.
func TestAgentHandshake_EnvBootstrapAllowsAnyAgentId(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)
	// routeTestAgentKey is the env-var key configured on newPairingTestServer's
	// Config (it's the same constant serverWithGuard uses).
	body := bytes.NewBufferString(`{"agentId":"anything-here"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// TestAgentHandshake_PendingRotationHintAbsentWhenOnNewest verifies a
// paired device already on the newest key gets no pendingRotation in
// the response (sql.ErrNoRows from LatestActiveKey surfaces as nil DTO).
func TestAgentHandshake_PendingRotationHintAbsentWhenOnNewest(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()
	p, key, err := pairSvc.CreatePairing(ctx, "iPhone", "test", func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"agentId":      p.AgentID,
		"currentKeyId": key.ID,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key.Plaintext)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var hs struct {
		PendingRotation any `json:"pendingRotation"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hs))
	assert.Nil(t, hs.PendingRotation, "device on newest key must not get a rotation hint")
}

// TestAgentHandshake_PendingRotationHintPresentWhenBehind verifies a
// paired device on an older key (after a rotation) gets a
// pendingRotation hint pointing at the newer key's row id.
func TestAgentHandshake_PendingRotationHintPresentWhenBehind(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()
	p, k1, err := pairSvc.CreatePairing(ctx, "iPhone", "test", func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)
	k2, _, err := pairSvc.RotateKey(ctx, p.ID, "test", 60, func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"agentId":      p.AgentID,
		"currentKeyId": k1.ID,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", k1.Plaintext)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var hs struct {
		PendingRotation *struct {
			KeyID int64 `json:"keyId"`
		} `json:"pendingRotation"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hs))
	require.NotNil(t, hs.PendingRotation)
	assert.Equal(t, k2.ID, hs.PendingRotation.KeyID)
}
