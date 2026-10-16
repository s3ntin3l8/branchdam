package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// TestAgentRebase_RefusesTrashedNode: RebaseNodePathByUUID sets
// lifecycle_state='ACTIVE' unconditionally, so before this check the device
// that created a node could rebase it straight out of the trash, undoing a
// user's delete. The node here is owned by the calling device, so the 404
// comes from the TRASHED refusal (the same answer as for ARCHIVED), not
// from the ownership check.
func TestAgentRebase_RefusesTrashedNode(t *testing.T) {
	srv, database, _, staging, exports, _ := serverWithGuard(t)
	ctx := context.Background()

	trashedUUID := "018f0000-0000-7000-8000-0000000000ab"
	originalPath := filepath.Join(staging, "deleted.mov")
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          trashedUUID,
			StorageLocationID: 1,
			FilePath:          originalPath,
			FileName:          "deleted.mov",
			LifecycleState:    "TRASHED",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		}); err != nil {
			return err
		}
		return q.SetNodeCreator(ctx, sqlcgen.SetNodeCreatorParams{NodeUuid: trashedUUID, AgentID: "test-device"})
	})
	if err != nil {
		t.Fatalf("seed trashed node: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   trashedUUID,
		"targetPath": filepath.Join(exports, "resurrected.mov"),
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("rebase of trashed node status = %d, want 404 Not Found, body = %s", rr.Code, rr.Body.String())
	}

	node, err := database.Reader.GetMediaNodeByUUID(ctx, trashedUUID)
	if err != nil {
		t.Fatalf("reload node: %v", err)
	}
	if node.LifecycleState != "TRASHED" {
		t.Errorf("lifecycle_state = %q, want TRASHED -- a refused rebase must not restore the node", node.LifecycleState)
	}
	if node.FilePath != originalPath {
		t.Errorf("file_path = %q, want unchanged %q", node.FilePath, originalPath)
	}
}
