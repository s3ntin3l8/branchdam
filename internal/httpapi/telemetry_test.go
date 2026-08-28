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

func TestAgentTelemetry_PersistenceAndStorageHealthQuery(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	now := time.Now().Unix()
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", bytesOfJSON(t, map[string]any{
		"agentId":       "studio-mac-pro",
		"clientVersion": "1.3.0",
		"timestampUnix": now,
		"scratchStorage": map[string]any{
			"mountPath":            "/Volumes/ScratchDisk",
			"totalBytes":           2000000000000,
			"freeBytes":            150000000000, // 7.5% free -> isLowSpace: true
			"usedBytes":            1850000000000,
			"mirrorsSizeBytes":     300000000000,
			"renderCacheSizeBytes": 900000000000,
			"proxiesSizeBytes":     400000000000,
			"prunableBytes":        500000000000,
		},
		"pruneStats": map[string]any{
			"lastPruneTimestampUnix": now - 1800,
			"lastReclaimedBytes":     120000000000,
			"lastPruneDurationMs":    3250,
			"prunedItemCounts": map[string]int{
				"mirrors":             10,
				"renderCacheProjects": 3,
				"proxies":             5,
			},
		},
	}))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-API-Key", routeTestAgentKey)
	postRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/agent/telemetry status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	// Query unified storage health
	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/storage-health", nil)
	healthRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(healthRec, healthReq)

	if healthRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health status = %d, body = %s", healthRec.Code, healthRec.Body.String())
	}

	var healthRes struct {
		Locations []any                   `json:"locations"`
		Queues    any                     `json:"queues"`
		Agents    []agentScratchHealthDTO `json:"agents"`
	}
	if err := json.Unmarshal(healthRec.Body.Bytes(), &healthRes); err != nil {
		t.Fatalf("unmarshal storage-health: %v", err)
	}

	if len(healthRes.Agents) != 1 {
		t.Fatalf("len(healthRes.Agents) = %d, want 1", len(healthRes.Agents))
	}
	ag := healthRes.Agents[0]
	if ag.AgentID != "studio-mac-pro" {
		t.Errorf("ag.AgentID = %q, want studio-mac-pro", ag.AgentID)
	}
	if ag.ClientVersion != "1.3.0" {
		t.Errorf("ag.ClientVersion = %q, want 1.3.0", ag.ClientVersion)
	}
	if ag.MountPath != "/Volumes/ScratchDisk" {
		t.Errorf("ag.MountPath = %q, want /Volumes/ScratchDisk", ag.MountPath)
	}
	if ag.RenderCacheSizeBytes != 900000000000 {
		t.Errorf("ag.RenderCacheSizeBytes = %d, want 900000000000", ag.RenderCacheSizeBytes)
	}
	if ag.LastReclaimedBytes != 120000000000 {
		t.Errorf("ag.LastReclaimedBytes = %d, want 120000000000", ag.LastReclaimedBytes)
	}
	if ag.PrunedItemCounts["mirrors"] != 10 || ag.PrunedItemCounts["renderCacheProjects"] != 3 {
		t.Errorf("ag.PrunedItemCounts = %+v, want 10 mirrors & 3 renderCacheProjects", ag.PrunedItemCounts)
	}
	if !ag.IsLowSpace {
		t.Errorf("ag.IsLowSpace = false, want true (7.5%% free)")
	}
	if ag.IsCriticalSpace {
		t.Errorf("ag.IsCriticalSpace = true, want false (7.5%% > 5%%)")
	}
	if ag.IsStale {
		t.Errorf("ag.IsStale = true, want false")
	}

	// Query dedicated GET /api/v1/storage-health/agents
	agentListReq := httptest.NewRequest(http.MethodGet, "/api/v1/storage-health/agents", nil)
	agentListRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentListRec, agentListReq)
	if agentListRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health/agents status = %d", agentListRec.Code)
	}

	var listRes struct {
		Agents []agentScratchHealthDTO `json:"agents"`
	}
	if err := json.Unmarshal(agentListRec.Body.Bytes(), &listRes); err != nil {
		t.Fatalf("unmarshal list agents: %v", err)
	}
	if len(listRes.Agents) != 1 {
		t.Fatalf("len(listRes.Agents) = %d, want 1", len(listRes.Agents))
	}

	// Delete agent via DELETE /api/v1/storage-health/agents/{agentId}
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/storage-health/agents/studio-mac-pro", nil)
	delReq.Header.Set("X-Authentik-Username", "admin")
	delRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/v1/storage-health/agents/studio-mac-pro status = %d, body=%s", delRec.Code, delRec.Body.String())
	}

	// Verify agent is deleted
	afterListReq := httptest.NewRequest(http.MethodGet, "/api/v1/storage-health/agents", nil)
	afterListRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(afterListRec, afterListReq)
	if err := json.Unmarshal(afterListRec.Body.Bytes(), &listRes); err != nil {
		t.Fatalf("unmarshal list agents after delete: %v", err)
	}
	if len(listRes.Agents) != 0 {
		t.Errorf("len(listRes.Agents) = %d, want 0 after deletion", len(listRes.Agents))
	}
}
