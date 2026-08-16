package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

const routeTestAgentKey = "01234567890123456789012345678901" // 33 chars

// fullTestServer builds a Server with a live worker pool and a real,
// running goroutine set -- unlike testServer(t) in server_test.go, which
// deliberately skips Pool.Run since the four basic tests never submit
// work. Route tests that trigger a scan need the pool actually consuming
// jobs.
func fullTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routes.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := workers.New[string](2, 16)
	pool.Run(ctx)

	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(),
		Version: "test",
	})
	return srv, database
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestMeReflectsBrowserPrincipal(t *testing.T) {
	srv, _ := fullTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	req.Header.Set("X-Authentik-Groups", "dam-admins")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Kind   string   `json:"kind"`
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != string(auth.KindUser) || got.Name != "alice" {
		t.Errorf("got %+v, want user alice", got)
	}
}

func TestMeReflectsMachinePrincipalOnAgentPath(t *testing.T) {
	srv, _ := fullTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAgentRoutesRejectMissingKey(t *testing.T) {
	srv, _ := fullTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestConfigReturnsVersion(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/config", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != "test" {
		t.Errorf("version = %q, want test", got.Version)
	}
}

func TestGetAssetNotFound(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/999999", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAuditQueueEmpty(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/edges/audit", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Entries []any `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("entries = %v, want empty on a fresh database", got.Entries)
	}
}

func TestAgentEventEnqueues(t *testing.T) {
	srv, _ := fullTestServer(t)
	body := map[string]string{
		"agentId":   "workstation-1",
		"eventType": "EVENT_NODE_CREATED",
		"payload":   `{"path":"/tmp/example.jpg"}`,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytesOfJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		EventID string `json:"eventId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EventID == "" {
		t.Error("eventId is empty")
	}
}

func bytesOfJSON(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

// TestScanEndToEndThroughHTTP is the capstone: POST /api/v1/scan against a
// real directory, poll /api/v1/progress until it completes, then confirm
// the resulting nodes are visible through GET /api/v1/assets -- proving
// the HTTP layer actually wires storage.Guard + workers.Pool + pipeline +
// graph.Engine together, not just that each layer works standalone.
func TestScanEndToEndThroughHTTP(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.txt"), []byte("bravo"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	var locationID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "http-scan-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locationID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	guard := storage.NewGuard([]storage.Location{
		{ID: locationID, Name: "http-scan-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})
	srv.guard = guard

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/scan", map[string]int64{"storageLocationId": locationID})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/scan status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var scanResp struct {
		JobID int64 `json:"jobId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &scanResp); err != nil {
		t.Fatalf("unmarshal scan response: %v", err)
	}
	if scanResp.JobID == 0 {
		t.Fatal("jobId is 0")
	}

	deadline := time.Now().Add(10 * time.Second)
	var job sqlcgen.ScanJob
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, scanResp.JobID)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.State != "RUNNING" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != "COMPLETED" {
		t.Fatalf("job state = %q, want COMPLETED (last_error=%v)", job.State, job.LastError)
	}
	if job.FilesSeen != 2 {
		t.Errorf("FilesSeen = %d, want 2", job.FilesSeen)
	}

	rr = doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var assetsResp struct {
		Assets []assetDTO `json:"assets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &assetsResp); err != nil {
		t.Fatalf("unmarshal assets response: %v", err)
	}
	if len(assetsResp.Assets) != 2 {
		t.Fatalf("got %d assets, want 2: %+v", len(assetsResp.Assets), assetsResp.Assets)
	}
}

// TestScanJobOutlivesRequestContext is the C1 regression guard: the scan
// goroutine must outlive the HTTP request context that started it. A real TCP
// server (httptest.NewServer) cancels the request's context the moment the
// handler returns -- unlike doJSON's direct ServeHTTP, which never does -- so
// this test fails if RunScan inherits the request ctx: the walk's per-entry
// check aborts with context.Canceled (job FAILED) or the commit/sweep InTxs
// fail on the canceled ctx (job stuck RUNNING forever).
func TestScanJobOutlivesRequestContext(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	var locationID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "http-scan-request-ctx", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locationID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	srv.guard = storage.NewGuard([]storage.Location{
		{ID: locationID, Name: "http-scan-request-ctx", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, err := json.Marshal(map[string]int64{"storageLocationId": locationID})
	if err != nil {
		t.Fatalf("marshal scan request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/api/v1/scan", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/scan: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/scan status = %d", resp.StatusCode)
	}
	var scanResp struct {
		JobID int64 `json:"jobId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&scanResp); err != nil {
		t.Fatalf("decode scan response: %v", err)
	}
	if scanResp.JobID == 0 {
		t.Fatal("jobId is 0")
	}

	deadline := time.Now().Add(10 * time.Second)
	var job sqlcgen.ScanJob
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, scanResp.JobID)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.State != "RUNNING" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != "COMPLETED" {
		t.Fatalf("job state = %q, want COMPLETED (last_error=%v) -- scan did not outlive the request context", job.State, job.LastError)
	}
	if job.FilesSeen != 1 {
		t.Errorf("FilesSeen = %d, want 1", job.FilesSeen)
	}
}
