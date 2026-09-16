package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func resolveSnapshotServer(t *testing.T) (*Server, *db.DB, string, string) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var virtualID, mediaID int64
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		v, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "resolve-virtual", RootPath: "/virtual/resolve", Tier: "PROJECTS", IsVirtual: 1,
		})
		if err != nil {
			return err
		}
		virtualID = v.ID
		m, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "media", RootPath: "/storage/media", Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		mediaID = m.ID
		_, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "018f0000-0000-7000-8000-000000000101", StorageLocationID: mediaID,
			FilePath: "/storage/media/a.mov", FileName: "a.mov", FileExt: "mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database,
		Guard: storage.NewGuard([]storage.Location{
			{ID: virtualID, Name: "resolve-virtual", RootPath: "/virtual/resolve", Tier: "PROJECTS", IsVirtual: true},
			{ID: mediaID, Name: "media", RootPath: "/storage/media", Tier: "TIER2_EXPORTS"},
		}),
	})
	return srv, database, "018f0000-0000-7000-8000-000000000102", "018f0000-0000-7000-8000-000000000101"
}

func postResolveSnapshot(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/resolve-snapshot", bytesOfJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func resolveSnapshotBody(timelineUUID, sourceUUID, path, clipName string) map[string]any {
	evidence := map[string]any{"mediaFilePath": path, "timelineId": "tl1", "clipName": clipName}
	return map[string]any{
		"agentId": "agent-a", "scopeId": strings.Repeat("a", 64),
		"timelines": []map[string]any{{
			"timelineId": "tl1", "nodeUuid": timelineUUID,
			"filePath":    "/virtual/resolve/" + timelineUUID,
			"displayName": "Resolve: Master", "evidenceJson": map[string]any{"timelineName": "Master"},
		}},
		"memberships": []map[string]any{{
			"timelineId": "tl1", "mediaFilePath": path,
			"sourceNodeUuid": sourceUUID, "evidenceJson": evidence,
		}},
	}
}

func TestResolveSnapshotCreateRefreshAndSoftDetach(t *testing.T) {
	srv, database, targetUUID, sourceUUID := resolveSnapshotServer(t)
	body := resolveSnapshotBody(targetUUID, sourceUUID, "D:\\a.mov", "original")
	rr := postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"created":1`) {
		t.Fatalf("initial snapshot = %d %s", rr.Code, rr.Body.String())
	}
	ctx := context.Background()
	target, err := database.Reader.GetMediaNodeByUUID(ctx, targetUUID)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := database.Reader.ListEdgesByTarget(ctx, target.ID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("initial edges = %d, %v", len(edges), err)
	}
	firstID := edges[0].ID
	rr = postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"unchanged":1`) {
		t.Fatalf("unchanged snapshot = %d %s", rr.Code, rr.Body.String())
	}
	member := body["memberships"].([]map[string]any)[0]
	member["evidenceJson"].(map[string]any)["clipName"] = "edited"
	rr = postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"refreshed":1`) {
		t.Fatalf("edited snapshot = %d %s", rr.Code, rr.Body.String())
	}
	updated, err := database.Reader.GetMediaEdge(ctx, firstID)
	if err != nil || !strings.Contains(updated.EvidenceJson, "edited") {
		t.Fatalf("updated edge = %+v, %v", updated, err)
	}
	body["memberships"] = []map[string]any{}
	rr = postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"removed":1`) {
		t.Fatalf("removed snapshot = %d %s", rr.Code, rr.Body.String())
	}
	retained, err := database.Reader.GetMediaEdge(ctx, firstID)
	if err != nil || retained.IsActive != 0 {
		t.Fatalf("retained inactive edge = %+v, %v", retained, err)
	}
	edges, err = database.Reader.ListEdgesByTarget(ctx, target.ID)
	if err != nil || len(edges) != 0 {
		t.Fatalf("active edges after removal = %d, %v", len(edges), err)
	}
}

func TestResolveSnapshotUnresolvedAndReviewedSafety(t *testing.T) {
	srv, database, targetUUID, sourceUUID := resolveSnapshotServer(t)
	body := resolveSnapshotBody(targetUUID, sourceUUID, "D:\\a.mov", "original")
	if rr := postResolveSnapshot(t, srv, body); rr.Code != http.StatusOK {
		t.Fatalf("initial snapshot: %d %s", rr.Code, rr.Body.String())
	}
	target, _ := database.Reader.GetMediaNodeByUUID(context.Background(), targetUUID)
	edges, _ := database.Reader.ListEdgesByTarget(context.Background(), target.ID)
	edgeID := edges[0].ID
	member := body["memberships"].([]map[string]any)[0]
	delete(member, "sourceNodeUuid")
	delete(member, "evidenceJson")
	rr := postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"unresolved":1`) {
		t.Fatalf("unresolved snapshot: %d %s", rr.Code, rr.Body.String())
	}
	active, _ := database.Reader.GetMediaEdge(context.Background(), edgeID)
	if active.IsActive != 1 {
		t.Fatal("unresolved membership removed an existing edge")
	}
	if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.ConfirmMediaEdge(context.Background(), sqlcgen.ConfirmMediaEdgeParams{ID: edgeID, ReviewedBy: sql.NullString{String: "human", Valid: true}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body["memberships"] = []map[string]any{}
	rr = postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"reviewedConflicts":1`) {
		t.Fatalf("reviewed removal: %d %s", rr.Code, rr.Body.String())
	}
	retained, _ := database.Reader.GetMediaEdge(context.Background(), edgeID)
	if retained.IsActive != 1 || retained.ReviewState != "CONFIRMED" {
		t.Fatalf("human-reviewed edge changed: %+v", retained)
	}
}

func TestResolveSnapshotRollbackAndAgentIsolation(t *testing.T) {
	srv, database, targetUUID, sourceUUID := resolveSnapshotServer(t)
	body := resolveSnapshotBody(targetUUID, sourceUUID, "D:\\a.mov", "original")
	member := body["memberships"].([]map[string]any)[0]
	member["sourceNodeUuid"] = "018f0000-0000-7000-8000-000000000999"
	rr := postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("missing source status = %d %s", rr.Code, rr.Body.String())
	}
	if _, err := database.Reader.GetMediaNodeByUUID(context.Background(), targetUUID); err == nil {
		t.Fatal("failed snapshot committed its timeline node")
	}
	member["sourceNodeUuid"] = sourceUUID
	if rr := postResolveSnapshot(t, srv, body); rr.Code != http.StatusOK {
		t.Fatalf("initial agent snapshot = %d %s", rr.Code, rr.Body.String())
	}
	body["agentId"] = "agent-b"
	rr = postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("cross-agent claim status = %d %s", rr.Code, rr.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSnapshotEmptyDatabaseRetiresPriorTimelineEdges(t *testing.T) {
	srv, database, targetUUID, sourceUUID := resolveSnapshotServer(t)
	body := resolveSnapshotBody(targetUUID, sourceUUID, "D:\\a.mov", "original")
	if rr := postResolveSnapshot(t, srv, body); rr.Code != http.StatusOK {
		t.Fatalf("initial snapshot: %d %s", rr.Code, rr.Body.String())
	}
	body["timelines"] = []map[string]any{}
	body["memberships"] = []map[string]any{}
	rr := postResolveSnapshot(t, srv, body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"removed":1`) {
		t.Fatalf("empty database snapshot: %d %s", rr.Code, rr.Body.String())
	}
	target, _ := database.Reader.GetMediaNodeByUUID(context.Background(), targetUUID)
	edges, err := database.Reader.ListEdgesByTarget(context.Background(), target.ID)
	if err != nil || len(edges) != 0 {
		t.Fatalf("old timeline active edges = %d, err = %v", len(edges), err)
	}
}
