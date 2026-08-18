package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

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

func TestListStorageLocations(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	archiveRoot := t.TempDir()
	resolvedArchive, err := filepath.EvalSymlinks(archiveRoot)
	if err != nil {
		t.Fatalf("resolve archive root: %v", err)
	}
	scratchRoot := t.TempDir()
	resolvedScratch, err := filepath.EvalSymlinks(scratchRoot)
	if err != nil {
		t.Fatalf("resolve scratch root: %v", err)
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "archive", RootPath: resolvedArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		}); err != nil {
			return err
		}
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "scratch", RootPath: resolvedScratch, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/storage-locations", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-locations status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Locations []storageLocationDTO `json:"locations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Locations) != 2 {
		t.Fatalf("got %d locations, want 2: %+v", len(got.Locations), got.Locations)
	}
	archive, scratch := got.Locations[0], got.Locations[1]
	if archive.Tier != "TIER3_MASTER_ARCHIVE" || !archive.ReadOnly {
		t.Errorf("archive = %+v, want TIER3_MASTER_ARCHIVE readOnly", archive)
	}
	if scratch.Tier != "TIER1_LOCAL_SCRATCH" || scratch.ReadOnly {
		t.Errorf("scratch = %+v, want TIER1_LOCAL_SCRATCH not readOnly", scratch)
	}
	if scratch.Name != "scratch" || scratch.RootPath != resolvedScratch || !scratch.Prunable {
		t.Errorf("scratch = %+v, want name/rootPath round-trip and prunable", scratch)
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

	if rr.Code != http.StatusAccepted {
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
	if rr.Code != http.StatusAccepted {
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
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

func TestMutatingRoutesAuthorization(t *testing.T) {
	srv, _ := fullTestServer(t)
	srv.cfg.Authz.Groups = []string{"dam-admins"}

	mutatingPaths := []string{
		"/api/v1/scan",
		"/api/v1/edges/1/confirm",
		"/api/v1/edges/1/reject",
	}

	for _, path := range mutatingPaths {
		t.Run("non-admin forbidden: "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Authentik-Username", "alice")
			req.Header.Set("X-Authentik-Groups", "dam-users")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d for non-admin on %s", rr.Code, http.StatusForbidden, path)
			}
		})

		t.Run("admin allowed: "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Authentik-Username", "bob")
			req.Header.Set("X-Authentik-Groups", "dam-users|dam-admins")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code == http.StatusForbidden {
				t.Errorf("status = %d, want non-403 for admin on %s", rr.Code, path)
			}
		})
	}
}

func TestOpenAPIEndpointExposure(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		srv, _ := fullTestServer(t)
		srv.cfg.HTTP.ExposeOpenAPI = false

		paths := []string{"/openapi.json", "/openapi.yaml", "/docs"}
		for _, p := range paths {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("X-Authentik-Username", "alice")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Errorf("GET %s when exposeOpenAPI=false: status = %d, want %d", p, rr.Code, http.StatusNotFound)
			}
		}
	})

	t.Run("enabled and gated by admin check", func(t *testing.T) {
		srv, _ := fullTestServer(t)
		srv.cfg.HTTP.ExposeOpenAPI = true
		srv.cfg.Authz.Groups = []string{"dam-admins"}

		paths := []string{"/openapi.json", "/docs"}
		for _, p := range paths {
			// Non-admin -> 403 Forbidden
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("X-Authentik-Username", "alice")
			req.Header.Set("X-Authentik-Groups", "dam-users")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("GET %s non-admin when exposeOpenAPI=true: status = %d, want %d", p, rr.Code, http.StatusForbidden)
			}

			// Admin -> 200 OK
			reqAdmin := httptest.NewRequest(http.MethodGet, p, nil)
			reqAdmin.Header.Set("X-Authentik-Username", "bob")
			reqAdmin.Header.Set("X-Authentik-Groups", "dam-admins")
			rrAdmin := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rrAdmin, reqAdmin)

			if rrAdmin.Code != http.StatusOK {
				t.Errorf("GET %s admin when exposeOpenAPI=true: status = %d, want %d", p, rrAdmin.Code, http.StatusOK)
			}
		}
	})
}

func TestHandleListPathRewrites(t *testing.T) {
	srv, _ := fullTestServer(t)
	srv.cfg.PathRewrites = []config.PathRewrite{
		{From: "D:\\Footage", To: "/storage/footage"},
		{From: "/Volumes/NAS", To: "/storage/nas"},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/path-rewrites", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/config/path-rewrites: status = %d, want %d", rr.Code, http.StatusOK)
	}

	var rewrites []PathRewriteDTO
	if err := json.NewDecoder(rr.Body).Decode(&rewrites); err != nil {
		t.Fatalf("decode json response: %v", err)
	}

	if len(rewrites) != 2 {
		t.Fatalf("got %d path rewrites, want 2", len(rewrites))
	}
	if rewrites[0].From != "D:\\Footage" || rewrites[0].To != "/storage/footage" {
		t.Errorf("rewrite 0 mismatch: %+v", rewrites[0])
	}
	if rewrites[1].From != "/Volumes/NAS" || rewrites[1].To != "/storage/nas" {
		t.Errorf("rewrite 1 mismatch: %+v", rewrites[1])
	}
}

func TestAssetLineageNotFound(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/999999/lineage", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAssetLineageInvalidDepth(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/1/lineage?depth=10", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for out-of-range depth, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAssetLineageTraversalDiamondAndDepth(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	// Seed 4-level deep graph with a diamond and rejected/archived nodes:
	// N1 (Root) -> N2 -> N4 (Diamond)
	// N1 (Root) -> N3 -> N4 (Diamond) -> N5 (Level 4)
	// N6 -> N1 (REJECTED edge)
	// N7 (ARCHIVED node)
	var n1, n2, n3, n4, n5, n6, n7 sqlcgen.MediaNode

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "test-loc", RootPath: "/tmp/loc", Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}

		createNode := func(name string, state string) (sqlcgen.MediaNode, error) {
			return q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          uuid.New().String(),
				StorageLocationID: loc.ID,
				FilePath:          "/tmp/" + name,
				FileName:          name,
				FileExt:           ".jpg",
				SizeBytes:         100,
				MtimeUnix:         time.Now().Unix(),
				IndexingStatus:    "INDEXED_FULL",
				GraphStatus:       "LINKED",
				LifecycleState:    state,
			})
		}

		if n1, err = createNode("n1.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n2, err = createNode("n2.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n3, err = createNode("n3.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n4, err = createNode("n4.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n5, err = createNode("n5.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n6, err = createNode("n6.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n7, err = createNode("n7.jpg", "ARCHIVED"); err != nil {
			return err
		}

		createEdge := func(src, tgt int64, state string) error {
			e, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
				SourceNodeID:     src,
				TargetNodeID:     tgt,
				RelationshipType: "DERIVED_FROM",
				Confidence:       0.9,
				ReviewState:      "AUTO_ACCEPTED",
				Resolver:         "test",
				Tier:             1,
			})
			if err != nil {
				return err
			}
			if state == "CONFIRMED" {
				return q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{
					ID:         e.ID,
					ReviewedBy: sql.NullString{String: "tester", Valid: true},
				})
			}
			if state == "REJECTED" {
				return q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{
					ID:         e.ID,
					ReviewedBy: sql.NullString{String: "tester", Valid: true},
				})
			}
			return nil
		}

		if err := createEdge(n1.ID, n2.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n1.ID, n3.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n2.ID, n4.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n3.ID, n4.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n4.ID, n5.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n6.ID, n1.ID, "REJECTED"); err != nil {
			return err
		}
		if err := createEdge(n1.ID, n7.ID, "CONFIRMED"); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Fatalf("seed test graph: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+toStr(n1.ID)+"/lineage?depth=3", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got struct {
		RootID int64      `json:"rootId"`
		Nodes  []assetDTO `json:"nodes"`
		Edges  []edgeDTO  `json:"edges"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.RootID != n1.ID {
		t.Errorf("rootId = %d, want %d", got.RootID, n1.ID)
	}

	// Verify diamond deduplication: n4 should appear exactly ONCE
	n4Count := 0
	nodeIDs := make(map[int64]bool)
	for _, node := range got.Nodes {
		nodeIDs[node.ID] = true
		if node.ID == n4.ID {
			n4Count++
		}
		if node.ID == n6.ID {
			t.Errorf("node %d (REJECTED edge source) should not be included in lineage", n6.ID)
		}
		if node.ID == n7.ID {
			t.Errorf("node %d (ARCHIVED node) should not be included in lineage", n7.ID)
		}
	}

	if n4Count != 1 {
		t.Errorf("node n4 count = %d, want 1 (diamond deduplication failure)", n4Count)
	}

	if !nodeIDs[n1.ID] || !nodeIDs[n2.ID] || !nodeIDs[n3.ID] || !nodeIDs[n4.ID] || !nodeIDs[n5.ID] {
		t.Errorf("nodes set missing expected graph nodes: got %+v", nodeIDs)
	}

	// Verify querying an ARCHIVED root asset returns 404
	rrArchived := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+toStr(n7.ID)+"/lineage", nil)
	if rrArchived.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/assets/%d/lineage (ARCHIVED) status = %d, want 404", n7.ID, rrArchived.Code)
	}
}

func TestCreateManualEdge(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var loc sqlcgen.StorageLocation
	var n1, n2 sqlcgen.MediaNode

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		n1, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-m1", FilePath: "/m1.raw", FileName: "m1.raw", FileExt: ".raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		n2, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-m2", FilePath: "/m2.jpg", FileName: "m2.jpg", FileExt: ".jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	// 1. Create valid manual edge n1 -> n2
	body := map[string]any{
		"sourceNodeId":     n1.ID,
		"targetNodeId":     n2.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/edges status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	var created edgeDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.SourceNodeID != n1.ID || created.TargetNodeID != n2.ID || created.Resolver != "manual" || created.ReviewState != "CONFIRMED" {
		t.Errorf("created edge = %+v, unexpected fields", created)
	}

	// 2. Self-loop attempt n1 -> n1 returns 422
	selfLoopBody := map[string]any{
		"sourceNodeId":     n1.ID,
		"targetNodeId":     n1.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rrSelf := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", selfLoopBody)
	if rrSelf.Code != http.StatusUnprocessableEntity {
		t.Errorf("self-loop status = %d, want 422", rrSelf.Code)
	}

	// 3. Cycle attempt n2 -> n1 (when n1 -> n2 exists) returns 409 Conflict
	cycleBody := map[string]any{
		"sourceNodeId":     n2.ID,
		"targetNodeId":     n1.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rrCycle := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", cycleBody)
	if rrCycle.Code != http.StatusConflict {
		t.Errorf("cycle status = %d, want 409, body = %s", rrCycle.Code, rrCycle.Body.String())
	}
}

func TestStorageHealth(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	tmpDir := t.TempDir()
	invalidDir := filepath.Join(tmpDir, "nonexistent-mount-path")

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc1, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "valid", RootPath: tmpDir, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		if err != nil {
			return err
		}
		_, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "invalid", RootPath: invalidDir, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		})
		if err != nil {
			return err
		}
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc1.ID, NodeUuid: "uuid-s1", FilePath: filepath.Join(tmpDir, "f1.raw"), FileName: "f1.raw", FileExt: ".raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed storage health test data: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/storage-health", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var got struct {
		Locations []struct {
			Name            string  `json:"name"`
			NodeCount       int64   `json:"nodeCount"`
			TotalBytes      uint64  `json:"totalBytes"`
			IsDegraded      bool    `json:"isDegraded"`
			DegradedMessage *string `json:"degradedMessage"`
		} `json:"locations"`
		Queues struct {
			WorkerPoolCapacity int   `json:"workerPoolCapacity"`
			RunningScanJobs    int64 `json:"runningScanJobs"`
		} `json:"queues"`
	}

	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal storage health response: %v", err)
	}

	if len(got.Locations) != 2 {
		t.Fatalf("got %d locations, want 2", len(got.Locations))
	}

	var validLoc, invalidLoc *struct {
		Name            string  `json:"name"`
		NodeCount       int64   `json:"nodeCount"`
		TotalBytes      uint64  `json:"totalBytes"`
		IsDegraded      bool    `json:"isDegraded"`
		DegradedMessage *string `json:"degradedMessage"`
	}

	for i := range got.Locations {
		if got.Locations[i].Name == "valid" {
			validLoc = &got.Locations[i]
		} else if got.Locations[i].Name == "invalid" {
			invalidLoc = &got.Locations[i]
		}
	}

	if validLoc == nil || invalidLoc == nil {
		t.Fatalf("locations missing valid or invalid entry: %+v", got.Locations)
	}

	if validLoc.IsDegraded || validLoc.NodeCount != 1 || validLoc.TotalBytes == 0 {
		t.Errorf("valid location unexpected state: %+v", validLoc)
	}

	if !invalidLoc.IsDegraded || invalidLoc.DegradedMessage == nil {
		t.Errorf("invalid location should be degraded: %+v", invalidLoc)
	}
}

func toStr(v int64) string {
	return fmt.Sprintf("%d", v)
}
