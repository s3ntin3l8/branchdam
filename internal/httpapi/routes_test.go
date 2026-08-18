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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

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

// inheritTestServer builds a Server whose Guard covers rootPath and seeds a
// Tier-2 location + parent/child nodes + a parent edge.
func inheritTestServer(t *testing.T, rootPath string) (*Server, *db.DB, sqlcgen.MediaNode, sqlcgen.MediaNode) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inherit.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	resolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	var locID int64
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "inherit-t2", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	guard := storage.NewGuard([]storage.Location{{ID: locID, Name: "inherit-t2", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: false}})

	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Guard: guard,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test",
	})

	parent := seedInheritNode(t, database, locID, filepath.Join(resolved, "parent.jpg"), "uuid-parent")
	child := seedInheritNode(t, database, locID, filepath.Join(resolved, "child.jpg"), "uuid-child")
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	return srv, database, parent, child
}

func seedInheritNode(t *testing.T, database *db.DB, locID int64, path, uuid string) sqlcgen.MediaNode {
	t.Helper()
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: uuid, StorageLocationID: locID, FilePath: path, FileName: filepath.Base(path),
			FileExt: "jpg", SizeBytes: 1, MtimeUnix: time.Now().Unix(),
			FastHash: &[]string{"aaaaaaaaaaaaaaaa"}[0], IndexingStatus: "INDEXED_SHALLOW",
			GraphStatus: "LINKED", LifecycleState: "ACTIVE", FilenameStem: sql.NullString{String: "x", Valid: true},
		})
		node = n
		return err
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return node
}

func TestInheritMetadataConflicts(t *testing.T) {
	t.Run("unknown asset 404s", func(t *testing.T) {
		srv, _, _, _ := inheritTestServer(t, t.TempDir())
		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/999999/inherit-metadata", nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
	})

	t.Run("read-only location 409s before exiftool", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		roRoot := t.TempDir()
		resolved, _ := filepath.EvalSymlinks(roRoot)
		var roLocID int64
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
				Name: "inherit-t3", RootPath: resolved, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
			})
			roLocID = loc.ID
			return err
		})
		if err != nil {
			t.Fatalf("seed ro location: %v", err)
		}
		err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			return q.RebaseMissingNodePath(context.Background(), sqlcgen.RebaseMissingNodePathParams{
				ID: child.ID, FilePath: filepath.Join(resolved, "child.jpg"), FileName: "child.jpg",
				StorageLocationID: roLocID, MtimeUnix: time.Now().Unix(),
			})
		})
		if err != nil {
			t.Fatalf("rebase child: %v", err)
		}
		srv.guard = storage.NewGuard([]storage.Location{{ID: roLocID, Name: "inherit-t3", RootPath: resolved, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true}})

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rr.Code)
		}
	})

	t.Run("tier-3 parent edge 409s", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		// A second parent edge resolved at tier 3 with higher confidence than
		// the seeded 0.9/2 edge becomes the winning parent -- and must be refused.
		locID := child.StorageLocationID
		t3Parent := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "t3-parent.jpg"), "uuid-t3-parent")
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
				SourceNodeID: t3Parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
				Confidence: 0.99, Tier: 3, Resolver: "heuristic", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
			})
			return err
		})
		if err != nil {
			t.Fatalf("seed tier-3 edge: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rr.Code)
		}
	})
	t.Run("needs-review parent edge 409s", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		// Reject the seeded AUTO_ACCEPTED edge, then add a NEEDS_REVIEW edge
		// from a second parent -- an unconfirmed lineage must never be used as
		// an identity source.
		var locID int64
		_ = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			rows, err := q.ListEdgesByTarget(context.Background(), child.ID)
			if err != nil {
				return err
			}
			for _, e := range rows {
				if _, err := q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{ID: e.ID, ReviewedBy: sql.NullString{}}); err != nil {
					return err
				}
			}
			locID = child.StorageLocationID
			return nil
		})
		nrParent := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "nr-parent.jpg"), "uuid-nr-parent")
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
				SourceNodeID: nrParent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
				Confidence: 0.8, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "NEEDS_REVIEW",
			})
			return err
		})
		if err != nil {
			t.Fatalf("seed needs-review edge: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rr.Code)
		}
	})
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
				_, err := q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{
					ID:         e.ID,
					ReviewedBy: sql.NullString{String: "tester", Valid: true},
				})
				return err
			}
			if state == "REJECTED" {
				_, err := q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{
					ID:         e.ID,
					ReviewedBy: sql.NullString{String: "tester", Valid: true},
				})
				return err
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

// TestCreateManualEdgeSetsGraphStatusLinked backs M3's third fix: a manual
// edge is CONFIRMED at insert time (CreateManualMediaEdge), the same
// review_state write confirm/reject makes -- the target node's
// graph_status must reflect that immediately, not stay UNLINKED until an
// unrelated resolve pass happens to touch it.
func TestCreateManualEdgeSetsGraphStatusLinked(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	n1, n2 := seedTwoUnlinkedNodes(t, database)

	body := map[string]any{
		"sourceNodeId":     n1.ID,
		"targetNodeId":     n2.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/edges status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	target, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target node: %v", err)
	}
	if target.GraphStatus != "LINKED" {
		t.Errorf("target graph_status = %q, want LINKED", target.GraphStatus)
	}
}

// seedTwoUnlinkedNodes is a small shared fixture for the confirm/reject/
// manual-create tests below: two UNLINKED nodes in one storage location, no
// edge between them yet.
func seedTwoUnlinkedNodes(t *testing.T, database *db.DB) (n1, n2 sqlcgen.MediaNode) {
	t.Helper()
	ctx := context.Background()
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: t.Name(), RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		n1, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-1", FilePath: "/p-" + t.Name() + ".raw",
			FileName: "p.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		n2, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-2", FilePath: "/c-" + t.Name() + ".jpg",
			FileName: "c.jpg", FileExt: "jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	return n1, n2
}

func TestConfirmRejectEdgeNotFoundReturns404(t *testing.T) {
	srv, _ := fullTestServer(t)

	rrConfirm := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges/999999/confirm", nil)
	if rrConfirm.Code != http.StatusNotFound {
		t.Errorf("confirm nonexistent edge status = %d, want 404, body = %s", rrConfirm.Code, rrConfirm.Body.String())
	}

	rrReject := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges/999999/reject", nil)
	if rrReject.Code != http.StatusNotFound {
		t.Errorf("reject nonexistent edge status = %d, want 404, body = %s", rrReject.Code, rrReject.Body.String())
	}
}

// TestConfirmEdgeRecomputesGraphStatus backs M3: confirming a NEEDS_REVIEW
// edge must flip the target node's graph_status to LINKED in the same
// request, not leave it stale until the next scan happens to re-resolve it.
func TestConfirmEdgeRecomputesGraphStatus(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	n1, n2 := seedTwoUnlinkedNodes(t, database)

	var edge sqlcgen.MediaEdge
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		edge, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: n1.ID, TargetNodeID: n2.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.60, Tier: 2, Resolver: "test", ReviewState: "NEEDS_REVIEW",
		})
		return err
	}); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	before, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target before confirm: %v", err)
	}
	if before.GraphStatus != "UNLINKED" {
		t.Fatalf("target graph_status before confirm = %q, want UNLINKED", before.GraphStatus)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", edge.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target after confirm: %v", err)
	}
	if after.GraphStatus != "LINKED" {
		t.Errorf("target graph_status after confirm = %q, want LINKED", after.GraphStatus)
	}
}

// TestRejectEdgeRecomputesGraphStatus backs M3: rejecting a node's only
// AUTO_ACCEPTED edge must revert graph_status to UNLINKED, not leave it
// stuck at LINKED with nothing in the audit queue to explain why.
func TestRejectEdgeRecomputesGraphStatus(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	n1, n2 := seedTwoUnlinkedNodes(t, database)

	var edge sqlcgen.MediaEdge
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		edge, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: n1.ID, TargetNodeID: n2.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.95, Tier: 1, Resolver: "test", ReviewState: "AUTO_ACCEPTED",
		})
		if err != nil {
			return err
		}
		// Simulate the graph engine having already set LINKED for this
		// AUTO_ACCEPTED edge, the state a real resolve pass would leave.
		return q.UpdateMediaNodeGraphStatus(ctx, sqlcgen.UpdateMediaNodeGraphStatusParams{ID: n2.ID, GraphStatus: "LINKED"})
	}); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reject", edge.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target after reject: %v", err)
	}
	if after.GraphStatus != "UNLINKED" {
		t.Errorf("target graph_status after reject = %q, want UNLINKED (its only edge was just rejected)", after.GraphStatus)
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
		switch got.Locations[i].Name {
		case "valid":
			validLoc = &got.Locations[i]
		case "invalid":
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

	if got.Queues.WorkerPoolCapacity != 16 {
		t.Errorf("got WorkerPoolCapacity = %d, want 16", got.Queues.WorkerPoolCapacity)
	}

	if got.Queues.RunningScanJobs != 0 {
		t.Errorf("got RunningScanJobs = %d, want 0", got.Queues.RunningScanJobs)
	}

	// Verify server with nil pool handles request cleanly
	nilPoolServer := New(Deps{DB: database})
	rrNil := doJSON(t, nilPoolServer.Handler(), http.MethodGet, "/api/v1/storage-health", nil)
	if rrNil.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health nil pool status = %d, want 200", rrNil.Code)
	}
}

// TestStatfsWithTimeoutSucceedsOnFastPath is statfsWithTimeout's happy
// path: a real, existing directory with a generous timeout must behave
// exactly like a direct unix.Statfs call.
func TestStatfsWithTimeoutSucceedsOnFastPath(t *testing.T) {
	stat, err := statfsWithTimeout(t.TempDir(), 5*time.Second)
	if err != nil {
		t.Fatalf("statfsWithTimeout: %v", err)
	}
	if stat.Blocks == 0 {
		t.Error("stat.Blocks = 0, want a real filesystem stat for an existing directory")
	}
}

// TestStatfsWithTimeoutReturnsErrorWhenExceeded backs M4: a hung NFS/SMB
// mount must degrade the location, not block the whole /api/v1/
// storage-health response indefinitely. Statfs itself can't be reproduced
// hanging in a portable unit test (it would need a genuinely wedged
// mount), so this uses statfsWithTimeoutFn to substitute a stub that
// blocks forever, forcing the timeout branch deterministically against a
// real, non-degenerate timeout -- an earlier version of this test raced a
// 1ns timeout against the real syscall instead, which was flaky in CI: a
// fast enough real Statfs call occasionally won that race, so the test
// would flip-flop between exercising the timeout branch and the success
// branch depending on runner load, not the code under test.
func TestStatfsWithTimeoutReturnsErrorWhenExceeded(t *testing.T) {
	blockForever := func(_ string, _ *unix.Statfs_t) error {
		select {}
	}
	_, err := statfsWithTimeoutFn(blockForever, "/irrelevant", 20*time.Millisecond)
	if err == nil {
		t.Fatal("statfsWithTimeoutFn with a permanently-blocked syscall returned nil error, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, want it to mention a timeout", err.Error())
	}
}

func TestFilteredAssetsAndFacets(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "scratch", RootPath: "/scratch", Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		if err != nil {
			return err
		}
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-c1", FilePath: "/scratch/c1.jpg", FileName: "c1.jpg", FileExt: ".jpg",
			IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE", CameraModel: sql.NullString{String: "Sony A7IV", Valid: true},
		})
		if err != nil {
			return err
		}
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-c2", FilePath: "/scratch/c2.jpg", FileName: "c2.jpg", FileExt: ".jpg",
			IndexingStatus: "INDEXED_FULL", GraphStatus: "LINKED", LifecycleState: "ACTIVE", CameraModel: sql.NullString{String: "Canon R5", Valid: true},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed test nodes: %v", err)
	}

	// 1. Facets endpoint
	rrFacets := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/facets", nil)
	if rrFacets.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets/facets status = %d, want 200", rrFacets.Code)
	}
	var facets struct {
		CameraModels []string `json:"cameraModels"`
	}
	if err := json.Unmarshal(rrFacets.Body.Bytes(), &facets); err != nil {
		t.Fatalf("unmarshal facets: %v", err)
	}
	if len(facets.CameraModels) != 2 {
		t.Errorf("got %d camera models, want 2", len(facets.CameraModels))
	}

	// 2. Filtered assets: unlinkedOnly=true
	rrUnlinked := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets?unlinkedOnly=true", nil)
	if rrUnlinked.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets?unlinkedOnly=true status = %d, want 200", rrUnlinked.Code)
	}
	var unlinkedRes struct {
		Assets []assetDTO `json:"assets"`
		Total  int64      `json:"total"`
	}
	if err := json.Unmarshal(rrUnlinked.Body.Bytes(), &unlinkedRes); err != nil {
		t.Fatalf("unmarshal unlinked res: %v", err)
	}
	if len(unlinkedRes.Assets) != 1 || unlinkedRes.Total != 1 {
		t.Errorf("unlinked filter got %d assets (total %d), want 1", len(unlinkedRes.Assets), unlinkedRes.Total)
	}

	// 3. Filtered assets: cameraModel=Canon R5
	rrCamera := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets?cameraModel=Canon+R5", nil)
	if rrCamera.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets?cameraModel=Canon+R5 status = %d, want 200", rrCamera.Code)
	}
	var cameraRes struct {
		Assets []assetDTO `json:"assets"`
		Total  int64      `json:"total"`
	}
	if err := json.Unmarshal(rrCamera.Body.Bytes(), &cameraRes); err != nil {
		t.Fatalf("unmarshal camera res: %v", err)
	}
	if len(cameraRes.Assets) != 1 || cameraRes.Assets[0].CameraModel != "Canon R5" {
		t.Errorf("camera filter got %+v, want Canon R5", cameraRes.Assets)
	}
}

func TestListJobsFiltered(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: "/loc", Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		if err != nil {
			return err
		}
		_, err = q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: loc.ID, Valid: true}, Kind: "FULL_SCAN",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed scan job: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/jobs?kind=FULL_SCAN", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs status = %d, want 200", rr.Code)
	}

	var res struct {
		Jobs  []scanJobDTO `json:"jobs"`
		Total int64        `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal jobs res: %v", err)
	}

	if len(res.Jobs) != 1 || res.Total != 1 || res.Jobs[0].Kind != "FULL_SCAN" {
		t.Errorf("jobs filter got %+v (total %d), want 1 FULL_SCAN job", res.Jobs, res.Total)
	}
}

func toStr(v int64) string {
	return fmt.Sprintf("%d", v)
}

func serverWithGuard(t *testing.T) (*Server, *db.DB, *storage.Guard, string, string, string) {
	t.Helper()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	exports := filepath.Join(root, "exports")
	archive := filepath.Join(root, "archive")
	for _, d := range []string{staging, exports, archive} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	resStaging, _ := filepath.EvalSymlinks(staging)
	resExports, _ := filepath.EvalSymlinks(exports)
	resArchive, _ := filepath.EvalSymlinks(archive)

	dbPath := filepath.Join(root, "routes.db")
	database, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	var loc1, loc2, loc3 sqlcgen.StorageLocation
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc1, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		loc2, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "exports", RootPath: resExports, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		loc3, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("upsert locations: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: loc1.ID, Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: false},
		{ID: loc2.ID, Name: "exports", RootPath: resExports, Tier: "TIER2_EXPORTS", ReadOnly: false},
		{ID: loc3.ID, Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})

	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:      database,
		Guard:   guard,
		Prober:  probe.New(),
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
	})

	return srv, database, guard, resStaging, resExports, resArchive
}

func TestAgentHandshake_Success_And_Auth(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	// Enqueue and mark an event processed for agent-alpha
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.EnqueueAgentEvent(ctx, sqlcgen.EnqueueAgentEventParams{
			EventUuid:   "018f0000-0000-7000-8000-000000000001",
			AgentID:     "agent-alpha",
			EventType:   "EVENT_NODE_CREATED",
			PayloadJson: `{"filePath":"/test.raw"}`,
		})
		if err != nil {
			return err
		}
		return q.MarkAgentEventProcessed(ctx, ev.ID)
	})
	if err != nil {
		t.Fatalf("setup event: %v", err)
	}

	// 1. Missing auth header -> 401
	reqNoAuth := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytesOfJSON(t, map[string]string{
		"agentId": "agent-alpha",
	}))
	reqNoAuth.Header.Set("Content-Type", "application/json")
	rrNoAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrNoAuth, reqNoAuth)
	if rrNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 Unauthorized", rrNoAuth.Code)
	}

	// 2. Valid handshake request -> 200 OK with server version and acknowledged event
	reqAuth := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytesOfJSON(t, map[string]string{
		"agentId":       "agent-alpha",
		"clientVersion": "0.1.0",
	}))
	reqAuth.Header.Set("Content-Type", "application/json")
	reqAuth.Header.Set("X-API-Key", routeTestAgentKey)
	rrAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrAuth, reqAuth)

	if rrAuth.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rrAuth.Code, rrAuth.Body.String())
	}

	var res struct {
		OK                    bool   `json:"ok"`
		ServerVersion         string `json:"serverVersion"`
		ServerTimeUnix        int64  `json:"serverTimeUnix"`
		AcknowledgedEventUUID string `json:"acknowledgedEventUuid"`
		PendingEventsCount    int64  `json:"pendingEventsCount"`
	}
	if err := json.Unmarshal(rrAuth.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal handshake response: %v", err)
	}

	if !res.OK || res.ServerVersion != "test" || res.AcknowledgedEventUUID != "018f0000-0000-7000-8000-000000000001" {
		t.Errorf("unexpected handshake response: %+v", res)
	}
}

func TestAgentRebase_Known_And_Unknown_Nodes_And_Tier3_Safety(t *testing.T) {
	srv, database, _, staging, exports, archive := serverWithGuard(t)
	ctx := context.Background()

	// Insert existing node on staging with an edge
	knownUUID := "018f0000-0000-7000-8000-000000000010"
	childUUID := "018f0000-0000-7000-8000-000000000011"
	stagingPath := filepath.Join(staging, "clip_orig.mov")

	var parentID, childID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		p, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          knownUUID,
			StorageLocationID: 1,
			FilePath:          stagingPath,
			FileName:          "clip_orig.mov",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		if err != nil {
			return err
		}
		parentID = p.ID

		c, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          childUUID,
			StorageLocationID: 1,
			FilePath:          filepath.Join(staging, "clip_proxy.mov"),
			FileName:          "clip_proxy.mov",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		if err != nil {
			return err
		}
		childID = c.ID

		_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID:     parentID,
			TargetNodeID:     childID,
			RelationshipType: "PROXY_OF",
			Confidence:       1.0,
			Tier:             1,
			Resolver:         "manual",
			EvidenceJson:     "{}",
			ReviewState:      "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes & edge: %v", err)
	}

	// 1. Rebase known node from staging to exports -> preserves node ID and edges
	rebasedExportPath := filepath.Join(exports, "clip_final.mov")
	reqRebaseKnown := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   knownUUID,
		"targetPath": rebasedExportPath,
		"fileName":   "clip_final.mov",
	}))
	reqRebaseKnown.Header.Set("Content-Type", "application/json")
	reqRebaseKnown.Header.Set("X-API-Key", routeTestAgentKey)
	rrRebaseKnown := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrRebaseKnown, reqRebaseKnown)

	if rrRebaseKnown.Code != http.StatusOK {
		t.Fatalf("rebase known status = %d, body = %s", rrRebaseKnown.Code, rrRebaseKnown.Body.String())
	}
	var outKnown struct {
		ID       int64  `json:"id"`
		NodeUUID string `json:"nodeUuid"`
		FilePath string `json:"filePath"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rrRebaseKnown.Body.Bytes(), &outKnown); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if outKnown.ID != parentID || outKnown.Status != "REBASED" || outKnown.FilePath != rebasedExportPath {
		t.Errorf("rebase known unexpected: %+v, want ID=%d status=REBASED", outKnown, parentID)
	}

	// Verify edges survived untouched
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		edges, err := q.ListEdgesBySource(ctx, parentID)
		if err != nil {
			return err
		}
		if len(edges) != 1 || edges[0].TargetNodeID != childID {
			t.Errorf("edges after rebase = %+v, want 1 edge to childID %d", edges, childID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("check edges: %v", err)
	}

	// 2. Rebase unknown node -> creates new node (agent is source of truth)
	unknownUUID := "018f0000-0000-7000-8000-000000000099"
	unknownExportPath := filepath.Join(exports, "staged_offline.arw")
	reqRebaseUnknown := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   unknownUUID,
		"targetPath": unknownExportPath,
		"fileName":   "staged_offline.arw",
		"fileExt":    ".arw",
		"sizeBytes":  2048,
	}))
	reqRebaseUnknown.Header.Set("Content-Type", "application/json")
	reqRebaseUnknown.Header.Set("X-API-Key", routeTestAgentKey)
	rrRebaseUnknown := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrRebaseUnknown, reqRebaseUnknown)

	if rrRebaseUnknown.Code != http.StatusOK {
		t.Fatalf("rebase unknown status = %d, body = %s", rrRebaseUnknown.Code, rrRebaseUnknown.Body.String())
	}
	var outUnknown struct {
		ID       int64  `json:"id"`
		NodeUUID string `json:"nodeUuid"`
		FilePath string `json:"filePath"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rrRebaseUnknown.Body.Bytes(), &outUnknown); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if outUnknown.Status != "CREATED" || outUnknown.NodeUUID != unknownUUID {
		t.Errorf("rebase unknown unexpected: %+v, want status=CREATED", outUnknown)
	}

	// 3. Rebase target resolving to Tier 3 (read-only archive) -> Refused!
	tier3Path := filepath.Join(archive, "forbidden.raw")
	reqTier3 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   knownUUID,
		"targetPath": tier3Path,
	}))
	reqTier3.Header.Set("Content-Type", "application/json")
	reqTier3.Header.Set("X-API-Key", routeTestAgentKey)
	rrTier3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTier3, reqTier3)

	if rrTier3.Code != http.StatusBadRequest {
		t.Fatalf("rebase to tier 3 status = %d, want 400 Bad Request", rrTier3.Code)
	}
}
