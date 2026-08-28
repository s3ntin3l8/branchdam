package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

const pruneOldMtime = 1000 // any TTL cutoff derived from "now" is far past this

// pruneTestServer seeds a prunable TIER1_LOCAL_SCRATCH location (with a
// real file on disk) and a TIER3_MASTER_ARCHIVE location, plus a PROXY_OF
// edge from the Tier-3 master to the Tier-1 candidate. cacheTTLHours, if
// >0, is persisted directly on the Tier-1 location's DB row (#238:
// handlePrune reads cache_ttl_hours off the row itself, not by re-joining
// the live config by root_path), and mirrored into the server's config
// too so config-level tests (e.g. validatePruneConfig-style checks) still
// see a consistent value.
func pruneTestServer(t *testing.T, cacheTTLHours int) (*Server, *db.DB, int64, sqlcgen.MediaNode) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prune.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	tier1Root := t.TempDir()
	tier3Root := t.TempDir()
	resolvedTier1, err := filepath.EvalSymlinks(tier1Root)
	if err != nil {
		t.Fatalf("resolve tier1 root: %v", err)
	}
	resolvedTier3, err := filepath.EvalSymlinks(tier3Root)
	if err != nil {
		t.Fatalf("resolve tier3 root: %v", err)
	}

	filePath := filepath.Join(resolvedTier1, "proxy.jpg")
	if err := os.WriteFile(filePath, []byte("proxy content"), 0o644); err != nil {
		t.Fatalf("write proxy file: %v", err)
	}
	masterPath := filepath.Join(resolvedTier3, "master.jpg")
	if err := os.WriteFile(masterPath, []byte("master content"), 0o644); err != nil {
		t.Fatalf("write master file: %v", err)
	}
	// Backdated well past any cacheTTLHours cutoff, then read back via
	// Lstat (not just reused verbatim) so the DB record matches exactly
	// what Execute's own file-freshness re-check will observe -- avoids
	// any filesystem mtime-precision mismatch between what Chtimes was
	// asked to set and what actually landed on disk.
	oldTime := time.Unix(pruneOldMtime, 0)
	if err := os.Chtimes(filePath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes proxy file: %v", err)
	}
	fileInfo, err := os.Lstat(filePath)
	if err != nil {
		t.Fatalf("lstat proxy file: %v", err)
	}

	var tier1ID, tier3ID int64
	var candidate sqlcgen.MediaNode
	fullHash := strings.Repeat("a", 64)
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		t1, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "t1", RootPath: resolvedTier1, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
			CacheTtlHours: int64(cacheTTLHours),
		})
		if err != nil {
			return err
		}
		tier1ID = t1.ID
		t3, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "t3", RootPath: resolvedTier3, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		})
		if err != nil {
			return err
		}
		tier3ID = t3.ID

		master, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-master", StorageLocationID: tier3ID,
			FilePath: filepath.Join(resolvedTier3, "master.jpg"), FileName: "master.jpg", FileExt: "jpg",
			SizeBytes: 100, MtimeUnix: pruneOldMtime, FullHash: &fullHash,
			IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}

		candidate, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-proxy", StorageLocationID: tier1ID,
			FilePath: filePath, FileName: "proxy.jpg", FileExt: "jpg",
			SizeBytes: fileInfo.Size(), MtimeUnix: fileInfo.ModTime().Unix(),
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}

		_, err = q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: master.ID, TargetNodeID: candidate.ID, RelationshipType: "PROXY_OF",
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed fixtures: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: tier1ID, Name: "t1", RootPath: resolvedTier1, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false},
		{ID: tier3ID, Name: "t3", RootPath: resolvedTier3, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})

	cfg := &config.Config{
		Agent:   config.Agent{APIKey: routeTestAgentKey},
		Pruning: config.Pruning{Enabled: true},
		StorageLocations: []config.StorageLocation{
			{Name: "t1", RootPath: resolvedTier1, Tier: "TIER1_LOCAL_SCRATCH", Prunable: true, CacheTTLHours: cacheTTLHours},
			{Name: "t3", RootPath: resolvedTier3, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
		},
	}

	srv := New(Deps{
		Config: cfg, DB: database, Guard: guard,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test",
	})
	return srv, database, tier1ID, candidate
}

func TestHandlePruneDryRunDefault(t *testing.T) {
	srv, database, tier1ID, candidate := pruneTestServer(t, 1) // 1h TTL, node is far older
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier1ID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Executed   bool `json:"executed"`
		Candidates []struct {
			NodeID   int64  `json:"nodeId"`
			FilePath string `json:"filePath"`
			Purged   bool   `json:"purged"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Executed {
		t.Error("executed = true, want false (execute omitted from the request)")
	}
	if len(out.Candidates) != 1 || out.Candidates[0].NodeID != candidate.ID {
		t.Fatalf("candidates = %+v, want exactly node %d", out.Candidates, candidate.ID)
	}
	if out.Candidates[0].Purged {
		t.Error("dry-run candidate reports purged=true, want false")
	}
	// Nothing must have actually been deleted or marked MISSING.
	if _, err := os.Stat(candidate.FilePath); err != nil {
		t.Errorf("file gone after dry run (stat err = %v)", err)
	}
	after, err := database.Reader.GetMediaNodeByID(context.Background(), candidate.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("lifecycle_state = %q after dry run, want ACTIVE (unchanged)", after.LifecycleState)
	}
}

// TestHandlePruneUsesPersistedCacheTTLNotConfigReJoin proves #238's fix:
// handlePrune must resolve cacheTtlHours from the storage_locations row
// itself, not by re-joining the live config by root_path. Before the fix,
// clearing (or otherwise mismatching) s.cfg.StorageLocations after the row
// was seeded -- the same shape as an operator editing rootPath in
// config.yaml and restarting, where the DB row that survives (or, as here,
// is inspected) no longer has any config entry whose RootPath matches it
// -- silently made ttlHours resolve to 0 ("never eligible") with no error.
// The cache_ttl_hours column is set once at seed time and never needs a
// second lookup, so this must still find the candidate.
func TestHandlePruneUsesPersistedCacheTTLNotConfigReJoin(t *testing.T) {
	srv, _, tier1ID, candidate := pruneTestServer(t, 1) // 1h TTL persisted on the DB row
	// Simulate the orphaning scenario directly: the live config no longer
	// has any entry matching this location's root_path (e.g. because it
	// was edited and the server restarted with a config that doesn't
	// mention this exact path anymore). The pre-fix re-join loop would
	// find nothing here and silently treat the location as "never
	// eligible" for pruning.
	srv.cfg().StorageLocations = []config.StorageLocation{
		{Name: "unrelated", RootPath: "/nowhere/near/this/location", Tier: "TIER1_LOCAL_SCRATCH", Prunable: true, CacheTTLHours: 1},
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier1ID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Candidates []struct {
			NodeID int64 `json:"nodeId"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].NodeID != candidate.ID {
		t.Fatalf("candidates = %+v, want exactly node %d (TTL must come from the DB row, not a config re-join)", out.Candidates, candidate.ID)
	}
}

func TestHandlePruneExecutePurges(t *testing.T) {
	srv, database, tier1ID, candidate := pruneTestServer(t, 1)
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier1ID,
		"execute":           true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Executed   bool `json:"executed"`
		Candidates []struct {
			NodeID int64  `json:"nodeId"`
			Purged bool   `json:"purged"`
			Error  string `json:"error"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Executed {
		t.Error("executed = false, want true")
	}
	if len(out.Candidates) != 1 || !out.Candidates[0].Purged || out.Candidates[0].Error != "" {
		t.Fatalf("candidates = %+v, want a single successful purge", out.Candidates)
	}

	if _, err := os.Stat(candidate.FilePath); !os.IsNotExist(err) {
		t.Errorf("file still exists after execute (stat err = %v)", err)
	}
	after, err := database.Reader.GetMediaNodeByID(context.Background(), candidate.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "MISSING" {
		t.Errorf("lifecycle_state = %q, want MISSING", after.LifecycleState)
	}
}

// TestHandlePruneNotPrunableLocation asserts a non-prunable location
// returns 200 with zero candidates, not a 422 -- deliberately the same
// shape as "prunable but no TTL configured" (TestHandlePruneNoTTLConfiguredNeverEligible).
// This is what lets AssetDetailPage's per-asset [Purge Cache] control,
// which has no way to know an asset's location tier up front, report
// "not eligible" uniformly instead of surfacing a raw error for the
// common case of clicking it on a Tier-2/3 asset.
func TestHandlePruneNotPrunableLocation(t *testing.T) {
	srv, _, _, _ := pruneTestServer(t, 1)
	// tier3 (index 1 in seeding order) is not prunable -- resolve its id via
	// the storage-locations list rather than hardcoding an assumed id.
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/storage-locations", nil)
	var locs struct {
		Locations []struct {
			ID       int64  `json:"id"`
			Prunable bool   `json:"prunable"`
			Tier     string `json:"tier"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &locs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var tier3ID int64
	for _, l := range locs.Locations {
		if l.Tier == "TIER3_MASTER_ARCHIVE" {
			tier3ID = l.ID
		}
	}
	if tier3ID == 0 {
		t.Fatal("tier3 location not found in list")
	}

	rrPrune := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier3ID,
	})
	if rrPrune.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rrPrune.Code, rrPrune.Body.String())
	}
	var out struct {
		Candidates []any `json:"candidates"`
	}
	if err := json.Unmarshal(rrPrune.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none (location is not prunable)", out.Candidates)
	}
}

func TestHandlePruneLocationNotFound(t *testing.T) {
	srv, _, _, _ := pruneTestServer(t, 1)
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": int64(999999),
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rr.Code, rr.Body.String())
	}
}

// TestHandlePruneNoTTLConfiguredNeverEligible proves prunable=true with no
// configured cacheTtlHours means "never eligible", not "prune everything
// immediately" -- the zero value must be the safe value.
func TestHandlePruneNoTTLConfiguredNeverEligible(t *testing.T) {
	srv, _, tier1ID, _ := pruneTestServer(t, 0) // no TTL configured
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier1ID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Candidates []any `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none (no TTL configured)", out.Candidates)
	}
}

// TestHandlePruneNodeIDsFiltersCandidates backs the per-asset [Purge Cache]
// action: nodeIds narrows the plan to exactly the requested node(s).
func TestHandlePruneNodeIDsFiltersCandidates(t *testing.T) {
	srv, _, tier1ID, candidate := pruneTestServer(t, 1)
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier1ID,
		"nodeIds":           []int64{candidate.ID + 1000}, // a node id that isn't the seeded candidate
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Candidates []any `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none (nodeIds filter excludes the only eligible node)", out.Candidates)
	}
}

// TestHandlePrune_DisabledByConfig proves that when Pruning.Enabled is false,
// /api/v1/prune returns 409 Conflict so callers and UIs get a distinct signal.
func TestHandlePrune_DisabledByConfig(t *testing.T) {
	srv, _, tier1ID, _ := pruneTestServer(t, 1)
	srv.cfgProvider = staticConfigProvider{cfg: &config.Config{
		Agent:   config.Agent{APIKey: routeTestAgentKey},
		Pruning: config.Pruning{Enabled: false},
	}}

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/prune", map[string]any{
		"storageLocationId": tier1ID,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 Conflict, body = %s", rr.Code, rr.Body.String())
	}
}
