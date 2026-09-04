package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

func signRequestForTest(apiKey, method, uri, nonce, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(method + "\n" + uri + "\n" + nonce + "\n" + timestamp + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAgentServer_ReplayProtectionEndToEnd(t *testing.T) {
	path := t.TempDir() + "/replay.db"
	database, err := db.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := workers.New[string](2, 16)
	pool.Run(ctx)

	cfg := &config.Config{
		Agent: config.Agent{
			APIKey:           routeTestAgentKey,
			SignedRequests:   true,
			ReplayWindowSecs: 300, // 5 minutes
		},
	}

	srv := New(Deps{
		Config:  cfg,
		DB:      database,
		Prober:  probe.New(),
		Pool:    pool,
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
	})
	handler := srv.Handler()

	t.Run("server rejects request without signature headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", routeTestAgentKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "invalid or missing signature")
	})

	t.Run("server accepts fresh signed request", func(t *testing.T) {
		body := []byte(`{"agentId":"test-macbook","clientVersion":"1.0.0"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", routeTestAgentKey)

		ts := strconv.FormatInt(time.Now().UnixNano(), 10)
		nonce := "0123456789abcdef0123456789abcdef"
		sig := signRequestForTest(routeTestAgentKey, http.MethodPost, "/api/v1/agent/handshake", nonce, ts, body)

		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "expected 200 OK: %s", rec.Body.String())
	})

	t.Run("server rejects replayed request with same nonce", func(t *testing.T) {
		body := []byte(`{"agentId":"test-macbook","clientVersion":"1.0.0"}`)
		ts := strconv.FormatInt(time.Now().UnixNano(), 10)
		nonce := "replay-unique-nonce-abc"
		sig := signRequestForTest(routeTestAgentKey, http.MethodPost, "/api/v1/agent/handshake", nonce, ts, body)

		// First call
		req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
		req1.Header.Set("Content-Type", "application/json")
		req1.Header.Set("X-API-Key", routeTestAgentKey)
		req1.Header.Set("X-Timestamp", ts)
		req1.Header.Set("X-Nonce", nonce)
		req1.Header.Set("X-Signature", sig)
		rec1 := httptest.NewRecorder()
		handler.ServeHTTP(rec1, req1)
		assert.Equal(t, http.StatusOK, rec1.Code)

		// Second call with same nonce
		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("X-API-Key", routeTestAgentKey)
		req2.Header.Set("X-Timestamp", ts)
		req2.Header.Set("X-Nonce", nonce)
		req2.Header.Set("X-Signature", sig)
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, req2)
		assert.Equal(t, http.StatusUnauthorized, rec2.Code)
		assert.Contains(t, rec2.Body.String(), "invalid or missing signature")
	})

	t.Run("server rejects tampered body", func(t *testing.T) {
		originalBody := []byte(`{"agentId":"agent-1"}`)
		ts := strconv.FormatInt(time.Now().UnixNano(), 10)
		nonce := "tamper-test-nonce-1"
		sig := signRequestForTest(routeTestAgentKey, http.MethodPost, "/api/v1/agent/handshake", nonce, ts, originalBody)

		tamperedBody := []byte(`{"agentId":"agent-hacked"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(tamperedBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", routeTestAgentKey)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("server rejects expired timestamp outside replay window", func(t *testing.T) {
		body := []byte(`{"agentId":"agent-1"}`)
		oldTs := strconv.FormatInt(time.Now().Add(-10*time.Minute).UnixNano(), 10)
		nonce := "old-timestamp-nonce"
		sig := signRequestForTest(routeTestAgentKey, http.MethodPost, "/api/v1/agent/handshake", nonce, oldTs, body)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", routeTestAgentKey)
		req.Header.Set("X-Timestamp", oldTs)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}
