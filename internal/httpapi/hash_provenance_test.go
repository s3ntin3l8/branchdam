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

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/prune"
)

// agentAssertedMaster rebases an unknown node onto a real file in the
// Tier-3 archive with an agent-supplied fullHash (the offline-staged-file
// path of POST /api/v1/agent/rebase), then links a past-TTL Tier-0 cache
// node to it with an AUTO_ACCEPTED PROXY_OF edge. It returns the master's
// and the cache node's UUIDs and ids. assertedHash is what the agent
// claims; the file's real content is content.
func agentAssertedMaster(t *testing.T, srv *Server, database *db.DB, staging, archive string, content []byte, assertedHash string) (masterUUID string, masterID, cacheID int64) {
	t.Helper()
	ctx := context.Background()
	masterUUID = "018f0000-0000-7000-8000-0000000005a5"
	masterPath := filepath.Join(archive, "master.mov")
	if err := os.WriteFile(masterPath, content, 0o644); err != nil {
		t.Fatalf("write master: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   masterUUID,
		"targetPath": masterPath,
		"fileName":   "master.mov",
		"sizeBytes":  len(content),
		"fullHash":   assertedHash,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rebase status = %d, body = %s", rr.Code, rr.Body.String())
	}
	master, err := database.Reader.GetMediaNodeByUUID(ctx, masterUUID)
	if err != nil {
		t.Fatalf("get master: %v", err)
	}
	if master.FullHash == nil || *master.FullHash != assertedHash {
		t.Fatalf("master full_hash = %v, want the asserted %s", master.FullHash, assertedHash)
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		cache, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "018f0000-0000-7000-8000-0000000005c5", StorageLocationID: 1,
			FilePath: filepath.Join(staging, "proxy.mov"), FileName: "proxy.mov", FileExt: ".mov",
			SizeBytes: 10, MtimeUnix: 1000,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		cacheID = cache.ID
		_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: master.ID, TargetNodeID: cache.ID, RelationshipType: "PROXY_OF",
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed cache node: %v", err)
	}
	return masterUUID, master.ID, cacheID
}

// nodeStatusVerified asks POST /api/v1/agent/node-status about one node.
func nodeStatusVerified(t *testing.T, srv *Server, nodeUUID string) bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/node-status", bytesOfJSON(t, map[string]any{
		"nodeUuids": []string{nodeUUID},
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("node-status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Statuses []AgentNodeStatusEntry `json:"statuses"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Statuses) != 1 || !res.Statuses[0].Found {
		t.Fatalf("node-status = %+v, want one found entry", res.Statuses)
	}
	return res.Statuses[0].Verified
}

// planIncludes reports whether prune.Plan over the Tier-0 staging location
// offers nodeID.
func planIncludes(t *testing.T, database *db.DB, nodeID int64) bool {
	t.Helper()
	got, err := prune.Plan(context.Background(), database.Reader, 1, 2000)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, c := range got {
		if c.NodeID == nodeID {
			return true
		}
	}
	return false
}

// TestAgentAssertedHashIsNotVerified is #505's regression test: a fullHash a
// paired device asserted through POST /api/v1/agent/rebase is a well-formed
// 64-character value on a live Tier-3 master, which used to be all
// node-status "verified" and Tier-1 pruning checked. The server never read
// those bytes, so neither may treat the hash as verified -- here the
// assertion is even false.
func TestAgentAssertedHashIsNotVerified(t *testing.T) {
	srv, database, _, staging, _, archive := serverWithGuard(t)
	content := []byte("the bytes the agent actually wrote")
	wrong, err := hashing.FullHash(bytes.NewReader([]byte("some other content")))
	if err != nil {
		t.Fatal(err)
	}
	masterUUID, _, cacheID := agentAssertedMaster(t, srv, database, staging, archive, content, wrong)

	if nodeStatusVerified(t, srv, masterUUID) {
		t.Error("node-status verified = true for an agent-asserted full_hash, want false")
	}
	if planIncludes(t, database, cacheID) {
		t.Error("prune.Plan offered a cache node whose only Tier-3 master hash is agent-asserted")
	}
}
