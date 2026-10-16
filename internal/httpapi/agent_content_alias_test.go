package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

const (
	aliasTestMobileKey   = "mobile-device-key-0123456789abcdef"
	aliasTestAgentKey    = "workstation-key-0123456789abcdef"
	aliasTestMobileID    = "phone-01"
	aliasTestWorkstation = "workstation-01"
)

// aliasTestServer is a server with two paired devices (a phone and a
// workstation), a writable Tier-0 staging location and a writable Tier-3
// archive that POST /api/v1/agent/upload lands in.
func aliasTestServer(t *testing.T) (*Server, *db.DB, *storage.Guard, string, string) {
	t.Helper()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	archive := filepath.Join(root, "archive")
	for _, d := range []string{staging, archive} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	resStaging, err := filepath.EvalSymlinks(staging)
	require.NoError(t, err)
	resArchive, err := filepath.EvalSymlinks(archive)
	require.NoError(t, err)

	database, err := db.Open(context.Background(), filepath.Join(root, "alias.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	var locStaging, locArchive sqlcgen.StorageLocation
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		locStaging, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING",
		})
		if err != nil {
			return err
		}
		locArchive, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE",
		})
		return err
	}))
	guard := storage.NewGuard([]storage.Location{
		{ID: locStaging.ID, Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING"},
		{ID: locArchive.ID, Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE"},
	})
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{}},
		DB:      database,
		Guard:   guard,
		Version: "test",
		agentKeyLookup: NewStubAgentKeyLookup(map[string]string{
			aliasTestMobileKey: aliasTestMobileID,
			aliasTestAgentKey:  aliasTestWorkstation,
		}),
	})
	return srv, database, guard, resStaging, resArchive
}

func aliasPostJSON(t *testing.T, h http.Handler, key, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAgentRebase_MobileThenAgentDuplicateRebasesCleanly is the audit's
// content-dedup scenario: a phone uploads a photo, then the workstation
// agent ingests the same bytes from the camera card. The agent's
// EVENT_NODE_CREATED is content-deduplicated (no second node), and its
// follow-up POST /api/v1/agent/rebase used to fall through to the insert
// branch and 500 on ux_media_nodes_live_full_hash on every retry. The
// agent's UUID is now an alias of the phone's node: rebase acknowledges it
// without touching that node, and node-status reports it.
func TestAgentRebase_MobileThenAgentDuplicateRebasesCleanly(t *testing.T) {
	srv, database, guard, staging, archive := aliasTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	data := []byte("the same photo, seen by the phone first and the camera card second")
	hasher := blake3.New()
	_, _ = hasher.Write(data)
	fullHash := hex.EncodeToString(hasher.Sum(nil))

	// 1. The phone uploads it.
	upReq := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	upReq.Header.Set("X-API-Key", aliasTestMobileKey)
	upReq.Header.Set("X-Filename", "PXL_20261016_120000.jpg")
	upReq.Header.Set("X-Blake3-Hash", fullHash)
	upRec := httptest.NewRecorder()
	h.ServeHTTP(upRec, upReq)
	require.Equal(t, http.StatusCreated, upRec.Code, upRec.Body.String())
	var up AgentUploadResponse
	require.NoError(t, json.Unmarshal(upRec.Body.Bytes(), &up))
	phoneNode, err := database.Reader.GetMediaNodeByUUID(ctx, up.NodeUUID)
	require.NoError(t, err)
	// The server hashed the upload itself. Backdate the row so a re-record
	// by anything below would be visible.
	prov, err := database.Reader.GetFullHashProvenance(ctx, phoneNode.ID)
	require.NoError(t, err)
	require.Equal(t, db.HashSourceServer, prov.Source)
	_, err = database.ExecInTx(ctx, `UPDATE full_hash_provenance SET recorded_at = 1000 WHERE node_id = ?`, phoneNode.ID)
	require.NoError(t, err)

	// 2. The workstation agent ingests the same bytes from the card and
	// announces them under its own UUID.
	agentUUID := "018f9a00-0000-7000-8000-00000000a001"
	payload, err := json.Marshal(agent.NodeCreatedPayload{
		NodeUUID: agentUUID,
		FilePath: filepath.Join(staging, "DSC00001.jpg"),
		FullHash: &fullHash,
	})
	require.NoError(t, err)
	evRec := aliasPostJSON(t, h, aliasTestAgentKey, "/api/v1/agent/events", map[string]any{
		"eventUuid": "018f9a00-0000-7000-8000-00000000e001",
		"agentId":   aliasTestWorkstation,
		"eventType": agent.EventNodeCreated,
		"payload":   string(payload),
	})
	require.Equal(t, http.StatusAccepted, evRec.Code, evRec.Body.String())

	stats, err := agent.NewDrainer(database, guard, nil).DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 0, stats.Failed)
	_, err = database.Reader.GetMediaNodeByUUID(ctx, agentUUID)
	require.Error(t, err, "content dedup must not create a second node")

	// 3. The agent copies its card file into the archive and rebases.
	agentArchivePath := filepath.Join(archive, "2026", "DSC00001.jpg")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentArchivePath), 0o755))
	require.NoError(t, os.WriteFile(agentArchivePath, data, 0o644))
	rbRec := aliasPostJSON(t, h, aliasTestAgentKey, "/api/v1/agent/rebase", map[string]any{
		"nodeUuid":   agentUUID,
		"targetPath": agentArchivePath,
		"fullHash":   fullHash,
		"sizeBytes":  len(data),
	})
	require.Equal(t, http.StatusOK, rbRec.Code, rbRec.Body.String())
	var rb AgentRebaseOutput
	require.NoError(t, json.Unmarshal(rbRec.Body.Bytes(), &rb.Body))
	assert.Equal(t, "ALIASED", rb.Body.Status)
	assert.Equal(t, up.NodeUUID, rb.Body.NodeUUID)
	assert.Equal(t, phoneNode.ID, rb.Body.ID)

	// The phone's node is untouched: same path, still the phone's.
	after, err := database.Reader.GetMediaNodeByUUID(ctx, up.NodeUUID)
	require.NoError(t, err)
	assert.Equal(t, phoneNode.FilePath, after.FilePath)
	assert.Equal(t, phoneNode.StorageLocationID, after.StorageLocationID)
	creator, err := database.Reader.GetNodeCreator(ctx, up.NodeUUID)
	require.NoError(t, err)
	assert.Equal(t, aliasTestMobileID, creator)
	// Neither the content-deduplicated EVENT_NODE_CREATED nor the aliased
	// rebase (both carrying the workstation's fullHash assertion) touched
	// the phone node's provenance: its hash is the server's own.
	prov, err = database.Reader.GetFullHashProvenance(ctx, phoneNode.ID)
	require.NoError(t, err)
	assert.Equal(t, db.HashSourceServer, prov.Source)
	assert.Equal(t, int64(1000), prov.RecordedAt)

	// A retry of the same rebase is just as clean (idempotent).
	rbRec2 := aliasPostJSON(t, h, aliasTestAgentKey, "/api/v1/agent/rebase", map[string]any{
		"nodeUuid":   agentUUID,
		"targetPath": agentArchivePath,
		"fullHash":   fullHash,
	})
	require.Equal(t, http.StatusOK, rbRec2.Code, rbRec2.Body.String())

	// 4. node-status resolves the agent's UUID to the phone's node, so the
	// agent can safely reclaim its local edit copy.
	nsRec := aliasPostJSON(t, h, aliasTestAgentKey, "/api/v1/agent/node-status", map[string]any{
		"nodeUuids": []string{agentUUID},
	})
	require.Equal(t, http.StatusOK, nsRec.Code, nsRec.Body.String())
	var ns AgentNodeStatusOutput
	require.NoError(t, json.Unmarshal(nsRec.Body.Bytes(), &ns.Body))
	require.Len(t, ns.Body.Statuses, 1)
	st := ns.Body.Statuses[0]
	assert.Equal(t, agentUUID, st.NodeUUID)
	assert.True(t, st.Found)
	assert.Equal(t, up.NodeUUID, st.CanonicalNodeUUID)
	assert.True(t, st.Verified)
	assert.Equal(t, "TIER3_MASTER_ARCHIVE", st.Tier)

	// Another device does not see through the workstation's alias.
	nsPhone := aliasPostJSON(t, h, aliasTestMobileKey, "/api/v1/agent/node-status", map[string]any{
		"nodeUuids": []string{agentUUID},
	})
	require.Equal(t, http.StatusOK, nsPhone.Code, nsPhone.Body.String())
	var nsP AgentNodeStatusOutput
	require.NoError(t, json.Unmarshal(nsPhone.Body.Bytes(), &nsP.Body))
	require.Len(t, nsP.Body.Statuses, 1)
	assert.False(t, nsP.Body.Statuses[0].Found)
	assert.Empty(t, nsP.Body.Statuses[0].CanonicalNodeUUID)
}

// TestAgentRebase_UnknownUUIDWithExistingHashIs409 covers the rebase that
// arrives with no alias to resolve (its EVENT_NODE_CREATED never reached
// the server): inserting would collide with ux_media_nodes_live_full_hash,
// which used to be a 500. It is a 409 naming the existing node instead.
func TestAgentRebase_UnknownUUIDWithExistingHashIs409(t *testing.T) {
	srv, database, _, staging, _ := aliasTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	fullHash := "1111111111111111111111111111111111111111111111111111111111111111"
	existingUUID := "018f9a00-0000-7000-8000-00000000b001"
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          existingUUID,
			StorageLocationID: 1,
			FilePath:          filepath.Join(staging, "existing.jpg"),
			FileName:          "existing.jpg",
			FullHash:          &fullHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	}))

	newUUID := "018f9a00-0000-7000-8000-00000000b002"
	rec := aliasPostJSON(t, h, aliasTestAgentKey, "/api/v1/agent/rebase", map[string]any{
		"nodeUuid":   newUUID,
		"targetPath": filepath.Join(staging, "copy.jpg"),
		"fullHash":   fullHash,
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
	var body struct {
		Status           int    `json:"status"`
		Detail           string `json:"detail"`
		Code             string `json:"code"`
		ExistingNodeUUID string `json:"existingNodeUuid"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, http.StatusConflict, body.Status)
	assert.Equal(t, "HASH_CONFLICT", body.Code)
	assert.Equal(t, existingUUID, body.ExistingNodeUUID)
	assert.Contains(t, body.Detail, existingUUID)

	_, err := database.Reader.GetMediaNodeByUUID(ctx, newUUID)
	require.Error(t, err, "no node may be created for the conflicting UUID")
	// Nor may the refused assertion be recorded against the existing node.
	existing, err := database.Reader.GetMediaNodeByUUID(ctx, existingUUID)
	require.NoError(t, err)
	_, err = database.Reader.GetFullHashProvenance(ctx, existing.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestAgentRebase_AliasedUUIDIsAcknowledgedBeforeTargetValidation: POST
// /agent/rebase for a UUID this device had content-deduplicated onto an
// existing node changes nothing, so it answers ALIASED whatever the target
// is. The alias is resolved before the read-only / Tier-3 target checks,
// which used to refuse it first (READ_ONLY_TARGET for a read-only non-Tier-3
// target, ARCHIVE_FILE_NOT_PRESENT for a Tier-3 path not copied yet).
func TestAgentRebase_AliasedUUIDIsAcknowledgedBeforeTargetValidation(t *testing.T) {
	srv, database, guard, staging, archive := aliasTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	roDir := filepath.Join(t.TempDir(), "readonly-import")
	require.NoError(t, os.MkdirAll(roDir, 0o755))
	resRoDir, err := filepath.EvalSymlinks(roDir)
	require.NoError(t, err)
	var roLoc sqlcgen.StorageLocation
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		roLoc, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: 1,
		})
		return err
	}))
	stagingLoc, err := guard.Resolve(staging)
	require.NoError(t, err)
	archiveLoc, err := guard.Resolve(archive)
	require.NoError(t, err)
	require.NoError(t, guard.ReloadLocations([]storage.Location{stagingLoc, archiveLoc, {
		ID: roLoc.ID, Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: true,
	}}))

	fullHash := "2222222222222222222222222222222222222222222222222222222222222222"
	phoneUUID := "018f9a00-0000-7000-8000-00000000c001"
	agentUUID := "018f9a00-0000-7000-8000-00000000c002"
	for _, ev := range []struct{ key, agentID, eventUUID, nodeUUID, name string }{
		{aliasTestMobileKey, aliasTestMobileID, "018f9a00-0000-7000-8000-00000000e101", phoneUUID, "PXL_0001.jpg"},
		{aliasTestAgentKey, aliasTestWorkstation, "018f9a00-0000-7000-8000-00000000e102", agentUUID, "DSC0001.jpg"},
	} {
		payload, err := json.Marshal(agent.NodeCreatedPayload{
			NodeUUID: ev.nodeUUID, FilePath: filepath.Join(staging, ev.name), FullHash: &fullHash,
		})
		require.NoError(t, err)
		rec := aliasPostJSON(t, h, ev.key, "/api/v1/agent/events", map[string]any{
			"eventUuid": ev.eventUUID, "agentId": ev.agentID, "eventType": agent.EventNodeCreated, "payload": string(payload),
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	}
	stats, err := agent.NewDrainer(database, guard, nil).DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)
	phoneNode, err := database.Reader.GetMediaNodeByUUID(ctx, phoneUUID)
	require.NoError(t, err)

	readOnlyTarget := filepath.Join(resRoDir, "DSC0001.jpg")
	require.NoError(t, os.WriteFile(readOnlyTarget, []byte("bytes"), 0o644))
	for name, target := range map[string]string{
		"read-only non-Tier-3 target":   readOnlyTarget,
		"Tier-3 target not present yet": filepath.Join(archive, "not-copied-yet.jpg"),
	} {
		rec := aliasPostJSON(t, h, aliasTestAgentKey, "/api/v1/agent/rebase", map[string]any{
			"nodeUuid": agentUUID, "targetPath": target, "fullHash": fullHash,
		})
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
		var rb AgentRebaseOutput
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rb.Body))
		assert.Equal(t, "ALIASED", rb.Body.Status, name)
		assert.Equal(t, phoneUUID, rb.Body.NodeUUID, name)
		assert.Equal(t, phoneNode.ID, rb.Body.ID, name)
	}
	after, err := database.Reader.GetMediaNodeByUUID(ctx, phoneUUID)
	require.NoError(t, err)
	assert.Equal(t, phoneNode.FilePath, after.FilePath)
	assert.Equal(t, phoneNode.StorageLocationID, after.StorageLocationID)

	// Another device does not see through the workstation's alias, so its
	// rebase of the same UUID has its target validated and refused as before.
	rec := aliasPostJSON(t, h, aliasTestMobileKey, "/api/v1/agent/rebase", map[string]any{
		"nodeUuid": agentUUID, "targetPath": readOnlyTarget,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "READ_ONLY_TARGET")
}
