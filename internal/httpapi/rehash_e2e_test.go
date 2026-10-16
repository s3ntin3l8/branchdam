package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/rehash"
)

// TestRehashRestoresVerificationForTrueAssertion is the other half of #505:
// once internal/rehash has re-read an agent-asserted master and matched
// it, node-status reports it verified and it authorises pruning again --
// the pause Decision 4 accepts is temporary, not permanent.
func TestRehashRestoresVerificationForTrueAssertion(t *testing.T) {
	srv, database, guard, staging, _, archive := serverWithGuard(t)
	content := []byte("the bytes the agent actually wrote")
	truth, err := hashing.FullHash(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	masterUUID, _, cacheID := agentAssertedMaster(t, srv, database, staging, archive, content, truth)

	if nodeStatusVerified(t, srv, masterUUID) || planIncludes(t, database, cacheID) {
		t.Fatal("an asserted hash counted as verified before any re-hash")
	}

	stats, err := rehash.NewWorker(database, guard, nil).ProcessBatch(context.Background())
	if err != nil || stats.Matched != 1 {
		t.Fatalf("ProcessBatch = %+v, %v; want one match", stats, err)
	}
	if !nodeStatusVerified(t, srv, masterUUID) {
		t.Error("node-status verified = false after a matching re-hash")
	}
	if !planIncludes(t, database, cacheID) {
		t.Error("prune.Plan still excludes the cache node after its master was re-hashed")
	}
}

// TestAgentRebaseRecordsAgentProvenanceOnKnownNode covers the other
// rebase branch: a device setting the first full_hash of a node it created
// records it as agent-asserted.
func TestAgentRebaseRecordsAgentProvenanceOnKnownNode(t *testing.T) {
	srv, database, _, staging, exports, _ := serverWithGuard(t)
	ctx := context.Background()
	nodeUUID := "018f0000-0000-7000-8000-0000000005b5"
	var nodeID int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: nodeUUID, StorageLocationID: 1, FilePath: filepath.Join(staging, "k.mov"), FileName: "k.mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		nodeID = n.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	claimForTestDevice(t, database, nodeUUID)

	asserted := strings.Repeat("ab", 32)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid": nodeUUID, "targetPath": filepath.Join(exports, "k.mov"), "fullHash": asserted,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rebase = %d, body = %s", rr.Code, rr.Body.String())
	}
	p, err := database.Reader.GetFullHashProvenance(ctx, nodeID)
	if err != nil {
		t.Fatalf("GetFullHashProvenance: %v", err)
	}
	if p.Source != db.HashSourceAgent || p.FullHash != asserted {
		t.Errorf("provenance = %+v, want AGENT for the asserted hash", p)
	}
}
