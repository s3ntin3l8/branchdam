package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/pairing"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
)

// agentStyleSignRequest is a verbatim copy of the workstation agent's
// request signer: s3ntin3l8/branchdam-agent internal/branchdam/client.go
// (*Client).signRequest at 55e02f4. branchdam-mobile's
// core/client/client.go (*Client).signRequest is byte-identical. It is
// copied rather than imported so this test pins the wire contract the
// shipped clients actually use: HMAC-SHA256 keyed by the RAW apiKey over
// "method\npath\nnonce\ntimestamp\n" followed by the body bytes, where
// path is the request path plus "?"+query for GETs.
func agentStyleSignRequest(apiKey, method, path, nonce, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(method + "\n" + path + "\n" + nonce + "\n" + timestamp + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// agentStyleReplayFields mirrors branchdam-agent's
// newReplayProtectionFields: UnixNano timestamp, 16 random bytes of nonce.
func agentStyleReplayFields(t *testing.T) (timestamp, nonce string) {
	t.Helper()
	var b [16]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return strconv.FormatInt(time.Now().UnixNano(), 10), hex.EncodeToString(b[:])
}

// agentStyleDo issues a request the way branchdam-agent's Client.post /
// Client.get do: X-API-Key, X-Timestamp, X-Nonce, X-Signature over the
// path-plus-query it put on the wire. signKey is the key the signature is
// computed with (normally the same as apiKey).
func agentStyleDo(t *testing.T, baseURL, method, path, apiKey, signKey string, body []byte) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, rdr)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ts, nonce := agentStyleReplayFields(t)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Nonce", nonce)
	req.Header.Set("X-Signature", agentStyleSignRequest(signKey, method, path, nonce, ts, body))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(respBody)
}

// TestAgentSigning_EndToEndWithRealPairingKey drives requests signed
// exactly as the shipped clients sign them, over a real HTTP listener,
// through the real AgentChain middleware with agent.signedRequests=true,
// authenticated by a key minted by the real pairing service (no test
// stub in the LookupKey path). Before the fix the server verified against
// HMAC-SHA256(pepper, "sign:"+apiKey), so every one of these 401'd.
func TestAgentSigning_EndToEndWithRealPairingKey(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "signing.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	pepper := sha256.Sum256([]byte("e2e-signing-pepper"))
	pairSvc := pairing.NewService(database, nil, pepper[:], nil)
	qr := func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	}
	devA, keyA, err := pairSvc.CreatePairing(context.Background(), "Workstation A", "user:tester", 0, qr)
	require.NoError(t, err)
	_, keyB, err := pairSvc.CreatePairing(context.Background(), "Workstation B", "user:tester", 0, qr)
	require.NoError(t, err)

	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{
			SignedRequests:   true,
			ReplayWindowSecs: 300,
		}},
		DB:      database,
		Prober:  probe.New(),
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
		Pairing: pairSvc,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	t.Run("signed POST handshake verifies", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{"agentId": devA.AgentID, "clientVersion": "0.0.0-test"})
		require.NoError(t, err)
		code, resp := agentStyleDo(t, ts.URL, http.MethodPost, "/api/v1/agent/handshake", keyA.Plaintext, keyA.Plaintext, body)
		require.Equal(t, http.StatusOK, code, "agent-signed handshake must verify: %s", resp)
	})

	t.Run("signed POST hello with empty body verifies", func(t *testing.T) {
		code, resp := agentStyleDo(t, ts.URL, http.MethodPost, "/api/v1/agent/hello", keyA.Plaintext, keyA.Plaintext, nil)
		require.Equal(t, http.StatusOK, code, "agent-signed hello must verify: %s", resp)
	})

	t.Run("signed GET with query string verifies", func(t *testing.T) {
		q := url.Values{}
		q.Set("fullHash", strings.Repeat("ab", 32))
		code, resp := agentStyleDo(t, ts.URL, http.MethodGet, "/api/v1/agent/check-content?"+q.Encode(), keyA.Plaintext, keyA.Plaintext, nil)
		require.Equal(t, http.StatusOK, code, "agent-signed GET must verify: %s", resp)
	})

	t.Run("signature keyed by another device's key is rejected", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{"agentId": devA.AgentID})
		require.NoError(t, err)
		code, resp := agentStyleDo(t, ts.URL, http.MethodPost, "/api/v1/agent/handshake", keyA.Plaintext, keyB.Plaintext, body)
		assert.Equal(t, http.StatusUnauthorized, code, resp)
		assert.Contains(t, resp, "invalid or missing signature")
	})

	t.Run("signature keyed by the old pepper derivation is rejected", func(t *testing.T) {
		mac := hmac.New(sha256.New, pepper[:])
		mac.Write([]byte("sign:" + keyA.Plaintext))
		oldKey := string(mac.Sum(nil))
		body, err := json.Marshal(map[string]any{"agentId": devA.AgentID})
		require.NoError(t, err)
		code, resp := agentStyleDo(t, ts.URL, http.MethodPost, "/api/v1/agent/handshake", keyA.Plaintext, oldKey, body)
		assert.Equal(t, http.StatusUnauthorized, code, resp)
	})
}
