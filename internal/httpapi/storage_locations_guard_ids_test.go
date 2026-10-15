package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// TestPutStorageLocationKeepsGuardLocationIDs is the regression test for the
// hot-reload that rebuilt every Guard location with ID 0. After a
// storage-location PUT, Guard.Resolve must still return the real
// storage_locations id, and an agent EVENT_NODE_CREATED drained against that
// Guard must land with that id as storage_location_id instead of failing its
// FK check (and being marked FAILED permanently).
func TestPutStorageLocationKeepsGuardLocationIDs(t *testing.T) {
	ctx := context.Background()
	scratch := t.TempDir()
	exports := t.TempDir()

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "guard-ids.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfgLocs := []config.StorageLocation{
		{Name: "exports", RootPath: exports, Tier: "TIER2_EXPORTS"},
		{Name: "scratch", RootPath: scratch, Tier: "TIER1_LOCAL_SCRATCH"},
	}
	// Seed rows the way cmd/branchdam's seedStorageLocations does (keyed on
	// the raw config rootPath). Two rows so the id under test is not 1.
	ids := make(map[string]int64, len(cfgLocs))
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, c := range cfgLocs {
			row, err := q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
				Name: c.Name, RootPath: c.RootPath, Tier: c.Tier,
			})
			if err != nil {
				return err
			}
			ids[c.Name] = row.ID
		}
		return nil
	}); err != nil {
		t.Fatalf("seed storage locations: %v", err)
	}
	scratchID := ids["scratch"]
	if scratchID <= 1 {
		t.Fatalf("scratch id = %d, want > 1 so a zeroed or defaulted id cannot match", scratchID)
	}

	// Boot-time Guard, built the same way main.go builds it.
	guard, skipped, err := storage.LoadGuard(ctx, database, nil)
	if err != nil {
		t.Fatalf("LoadGuard: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("LoadGuard skipped %v", skipped)
	}

	base := config.Config{Authz: config.Authz{Groups: []string{"dam-admins"}}, StorageLocations: cfgLocs}
	store, err := settings.NewStore(ctx, database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	srv := New(Deps{Settings: store, DB: database, Hub: sse.New(), Guard: guard, Version: "test", agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(scratchID), settingsGetJSON(map[string]any{
		"set": map[string]any{"name": "scratch-renamed"},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	resolvedScratch, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		t.Fatalf("resolve scratch: %v", err)
	}
	filePath := filepath.Join(resolvedScratch, "IMG_0001.JPG")
	if err := os.WriteFile(filePath, []byte("jpeg"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	loc, err := guard.Resolve(filePath)
	if err != nil {
		t.Fatalf("Resolve after PUT: %v", err)
	}
	if loc.Name != "scratch-renamed" {
		t.Fatalf("guard was not reloaded: name = %q, want %q", loc.Name, "scratch-renamed")
	}
	if loc.ID != scratchID {
		t.Fatalf("Resolve(...).ID after PUT = %d, want %d", loc.ID, scratchID)
	}

	nodeUUID := uuid.New().String()
	payload, err := json.Marshal(agent.NodeCreatedPayload{
		NodeUUID:  nodeUUID,
		FilePath:  filePath,
		SizeBytes: 4,
		MtimeUnix: 1700000000,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.EnqueueAgentEvent(ctx, sqlcgen.EnqueueAgentEventParams{
			EventUuid:   uuid.New().String(),
			AgentID:     "agent-test",
			EventType:   agent.EventNodeCreated,
			PayloadJson: string(payload),
		})
		return err
	}); err != nil {
		t.Fatalf("enqueue event: %v", err)
	}

	stats, err := agent.NewDrainer(database, guard, nil).ProcessPending(ctx, 10)
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if stats.Processed != 1 || stats.Failed != 0 {
		t.Fatalf("drain stats = %+v, want 1 processed and 0 failed", stats)
	}

	node, err := database.Reader.GetMediaNodeByUUID(ctx, nodeUUID)
	if err != nil {
		t.Fatalf("GetMediaNodeByUUID: %v", err)
	}
	if node.StorageLocationID != scratchID {
		t.Errorf("node storage_location_id = %d, want %d", node.StorageLocationID, scratchID)
	}
}
