package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

func TestAgentTelemetry_MissingKey_Unauthorized(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", bytesOfJSON(t, map[string]any{
		"agentId":       "test-agent",
		"timestampUnix": time.Now().Unix(),
		"scratchStorage": map[string]any{
			"totalBytes": 1000,
			"freeBytes":  500,
		},
	}))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 Unauthorized", rr.Code)
	}
}

func TestAgentTelemetry_ValidationErrors(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	// Missing agentId (required by Huma -> 422 or 400)
	reqNoAgent := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", bytesOfJSON(t, map[string]any{
		"agentId":       "",
		"timestampUnix": time.Now().Unix(),
	}))
	reqNoAgent.Header.Set("Content-Type", "application/json")
	reqNoAgent.Header.Set("X-API-Key", routeTestAgentKey)
	rrNoAgent := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrNoAgent, reqNoAgent)
	if rrNoAgent.Code != http.StatusUnprocessableEntity && rrNoAgent.Code != http.StatusBadRequest {
		t.Fatalf("empty agentId status = %d, want 422 or 400", rrNoAgent.Code)
	}

	// Invalid timestamp (<= 0 -> Huma schema validation 422 / handler 400)
	reqBadTime := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", bytesOfJSON(t, map[string]any{
		"agentId":       "agent-1",
		"timestampUnix": 0,
	}))
	reqBadTime.Header.Set("Content-Type", "application/json")
	reqBadTime.Header.Set("X-API-Key", routeTestAgentKey)
	rrBadTime := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrBadTime, reqBadTime)
	if rrBadTime.Code != http.StatusUnprocessableEntity && rrBadTime.Code != http.StatusBadRequest {
		t.Fatalf("zero timestamp status = %d, want 422 or 400", rrBadTime.Code)
	}
}

func TestAgentTelemetry_ValidPayload(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	now := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", bytesOfJSON(t, map[string]any{
		"agentId":       "workstation-macbook",
		"clientVersion": "1.2.0",
		"timestampUnix": now,
		"scratchStorage": map[string]any{
			"mountPath":            "/Volumes/Scratch",
			"totalBytes":           1000000000,
			"freeBytes":            400000000,
			"usedBytes":            600000000,
			"mirrorsSizeBytes":     200000000,
			"renderCacheSizeBytes": 300000000,
			"proxiesSizeBytes":     100000000,
			"prunableBytes":        250000000,
		},
		"pruneStats": map[string]any{
			"lastPruneTimestampUnix": now - 3600,
			"lastReclaimedBytes":     50000000,
			"lastPruneDurationMs":    1200,
			"prunedItemCounts": map[string]int{
				"mirrors":             2,
				"renderCacheProjects": 1,
			},
		},
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var res struct {
		OK                 bool  `json:"ok"`
		AcknowledgedAtUnix int64 `json:"acknowledgedAtUnix"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal telemetry response: %v", err)
	}

	if !res.OK {
		t.Errorf("res.OK = false, want true")
	}
	if res.AcknowledgedAtUnix <= 0 {
		t.Errorf("res.AcknowledgedAtUnix = %d, want > 0", res.AcknowledgedAtUnix)
	}
}

func TestAgentTelemetry_ForbiddenNonMachinePrincipal(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindUser})

	in := &AgentTelemetryInput{}
	in.Body.AgentID = "agent-1"
	in.Body.TimestampUnix = time.Now().Unix()

	_, err := srv.handleAgentTelemetry(ctx, in)
	if err == nil {
		t.Fatal("handleAgentTelemetry with KindUser: expected error, got nil")
	}
	var humaErr huma.StatusError
	if errors.As(err, &humaErr) {
		if humaErr.GetStatus() != http.StatusForbidden {
			t.Errorf("status = %d, want 403 Forbidden", humaErr.GetStatus())
		}
	}
}
