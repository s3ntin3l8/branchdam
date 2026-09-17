package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// bulkResolveSnapshotServer builds a server plus n distinct, already-indexed
// source media nodes under one non-virtual storage location. It backs both
// the batched-lookup regression tests and the benchmarks below (issue #448),
// which need many candidate source nodes to actually exercise batching
// instead of the pre-existing PR 447 tests' single-membership snapshots.
func bulkResolveSnapshotServer(t testing.TB, n int) (srv *Server, database *db.DB, targetUUID string, sourceUUIDs []string) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "resolve-bulk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var virtualID, mediaID int64
	sourceUUIDs = make([]string, n)
	if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		v, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "resolve-virtual-bulk", RootPath: "/virtual/resolve-bulk", Tier: "PROJECTS", IsVirtual: 1,
		})
		if err != nil {
			return err
		}
		virtualID = v.ID
		m, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "media-bulk", RootPath: "/storage/media-bulk", Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		mediaID = m.ID
		for i := 0; i < n; i++ {
			id := uuid.New().String()
			sourceUUIDs[i] = id
			if _, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
				NodeUuid: id, StorageLocationID: mediaID,
				FilePath: fmt.Sprintf("/storage/media-bulk/%d.mov", i), FileName: fmt.Sprintf("%d.mov", i), FileExt: "mov",
				IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv = New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database,
		Guard: storage.NewGuard([]storage.Location{
			{ID: virtualID, Name: "resolve-virtual-bulk", RootPath: "/virtual/resolve-bulk", Tier: "PROJECTS", IsVirtual: true},
			{ID: mediaID, Name: "media-bulk", RootPath: "/storage/media-bulk", Tier: "TIER2_EXPORTS"},
		}),
	})
	return srv, database, uuid.New().String(), sourceUUIDs
}

func bulkResolveSnapshotBody(agentID, scopeID, timelineUUID string, sourceUUIDs []string) map[string]any {
	memberships := make([]map[string]any, len(sourceUUIDs))
	for i, srcUUID := range sourceUUIDs {
		path := fmt.Sprintf("D:\\clip%d.mov", i)
		evidence := map[string]any{"mediaFilePath": path, "timelineId": "tl1", "clipName": fmt.Sprintf("clip%d", i)}
		memberships[i] = map[string]any{
			"timelineId": "tl1", "mediaFilePath": path,
			"sourceNodeUuid": srcUUID, "evidenceJson": evidence,
		}
	}
	return map[string]any{
		"agentId": agentID, "scopeId": scopeID,
		"timelines": []map[string]any{{
			"timelineId": "tl1", "nodeUuid": timelineUUID,
			"filePath":    "/virtual/resolve-bulk/" + timelineUUID,
			"displayName": "Resolve: Bulk", "evidenceJson": map[string]any{"timelineName": "Bulk"},
		}},
		"memberships": memberships,
	}
}

// TestResolveSnapshotBatchedSourceResolutionRejectsUnknownSourceAmongMany
// proves batching the per-membership source lookup into one IN-list query
// still catches an individual unresolved sourceNodeUuid buried in a large
// batch, and that the whole snapshot still rolls back -- batching must not
// let one bad id in the middle of the list silently pass the others through.
func TestResolveSnapshotBatchedSourceResolutionRejectsUnknownSourceAmongMany(t *testing.T) {
	const n = 500
	srv, database, targetUUID, sourceUUIDs := bulkResolveSnapshotServer(t, n)
	scopeID := strings.Repeat("b", 64)
	body := bulkResolveSnapshotBody("agent-bulk", scopeID, targetUUID, sourceUUIDs)
	memberships := body["memberships"].([]map[string]any)
	memberships[n/2]["sourceNodeUuid"] = uuid.New().String()

	rr := postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("unknown source among %d memberships: status = %d, body = %s", n, rr.Code, rr.Body.String())
	}
	if _, err := database.Reader.GetMediaNodeByUUID(context.Background(), targetUUID); err == nil {
		t.Fatal("rejected bulk snapshot committed its timeline node; batching broke all-or-nothing rollback")
	}
}

// TestResolveSnapshotBatchedCyclePreventionAcrossManyCandidates proves the
// once-per-timeline descendant walk (replacing a per-candidate cycle CTE)
// still catches an indirect, multi-hop cycle when it is buried among many
// otherwise-valid candidates, and that the whole snapshot rolls back.
func TestResolveSnapshotBatchedCyclePreventionAcrossManyCandidates(t *testing.T) {
	const n = 500
	srv, database, targetUUID, sourceUUIDs := bulkResolveSnapshotServer(t, n)
	scopeID := strings.Repeat("c", 64)

	empty := bulkResolveSnapshotBody("agent-bulk", scopeID, targetUUID, nil)
	if rr := postResolveSnapshot(t, srv, empty); rr.Code != http.StatusOK {
		t.Fatalf("initial empty snapshot: %d %s", rr.Code, rr.Body.String())
	}

	ctx := context.Background()
	target, err := database.Reader.GetMediaNodeByUUID(ctx, targetUUID)
	if err != nil {
		t.Fatal(err)
	}
	cycleSource := sourceUUIDs[n/2]
	cycleNode, err := database.Reader.GetMediaNodeByUUID(ctx, cycleSource)
	if err != nil {
		t.Fatal(err)
	}

	// Plant target -> intermediate -> cycleNode outside this request, so
	// cycleNode is already a two-hop descendant of target before the batched
	// candidate check runs. Adding cycleNode -> target (what this snapshot
	// asks for) would close that loop.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		intermediate, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: uuid.New().String(), StorageLocationID: target.StorageLocationID,
			FilePath: "/storage/media-bulk/intermediate.mov", FileName: "intermediate.mov", FileExt: "mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		if _, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: target.ID, TargetNodeID: intermediate.ID, RelationshipType: "PROJECT_SIDECAR",
			Confidence: 1, Tier: 1, Resolver: "resolve_project_db", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		}); err != nil {
			return err
		}
		_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: intermediate.ID, TargetNodeID: cycleNode.ID, RelationshipType: "PROJECT_SIDECAR",
			Confidence: 1, Tier: 1, Resolver: "resolve_project_db", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	body := bulkResolveSnapshotBody("agent-bulk", scopeID, targetUUID, sourceUUIDs)
	rr := postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "lineage cycle") {
		t.Fatalf("cycle among %d candidates: status = %d, body = %s", n, rr.Code, rr.Body.String())
	}

	edges, err := database.Reader.ListEdgesByTarget(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("rejected cyclic bulk snapshot left %d edges committed; batching broke all-or-nothing rollback", len(edges))
	}
}

// BenchmarkResolveSnapshot1k and BenchmarkResolveSnapshot10k are the
// representative 1k/10k-membership benchmarks issue #448 asks for. They
// exercise the full HTTP handler (source resolution + cycle check +
// reconciliation) against a real SQLite DB, not just reconcileResolveTimeline
// in isolation, so a regression in the batching would show up here too.
func benchmarkResolveSnapshot(b *testing.B, n int) {
	scopeID := strings.Repeat("d", 64)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		srv, _, targetUUID, sourceUUIDs := bulkResolveSnapshotServer(b, n)
		body := bulkResolveSnapshotBody("agent-bulk", scopeID, targetUUID, sourceUUIDs)
		b.StartTimer()
		rr := postResolveSnapshot(b, srv, body)
		if rr.Code != http.StatusOK {
			b.Fatalf("snapshot of %d memberships: status = %d, body = %s", n, rr.Code, rr.Body.String())
		}
	}
}

func BenchmarkResolveSnapshot1k(b *testing.B)  { benchmarkResolveSnapshot(b, 1_000) }
func BenchmarkResolveSnapshot10k(b *testing.B) { benchmarkResolveSnapshot(b, 10_000) }
