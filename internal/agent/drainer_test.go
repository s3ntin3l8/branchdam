package agent_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	db      *db.DB
	guard   *storage.Guard
	staging string
	exports string
	archive string
	locID1  int64
	locID2  int64
	locID3  int64
}

func setupTestDB(t *testing.T) *testEnv {
	t.Helper()
	database, err := db.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	exports := filepath.Join(root, "exports")
	archive := filepath.Join(root, "archive")
	for _, d := range []string{staging, exports, archive} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	resStaging, err := filepath.EvalSymlinks(staging)
	require.NoError(t, err)
	resExports, err := filepath.EvalSymlinks(exports)
	require.NoError(t, err)
	resArchive, err := filepath.EvalSymlinks(archive)
	require.NoError(t, err)

	ctx := context.Background()
	var loc1, loc2, loc3 sqlcgen.StorageLocation
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc1, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name:     "local_staging",
			RootPath: resStaging,
			Tier:     "TIER0_LOCAL_STAGING",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}
		loc2, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name:     "exports",
			RootPath: resExports,
			Tier:     "TIER2_EXPORTS",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}
		loc3, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name:     "archive",
			RootPath: resArchive,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 1,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	guard := storage.NewGuard([]storage.Location{
		{ID: loc1.ID, Name: "local_staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: false},
		{ID: loc2.ID, Name: "exports", RootPath: resExports, Tier: "TIER2_EXPORTS", ReadOnly: false},
		{ID: loc3.ID, Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})

	return &testEnv{
		db:      database,
		guard:   guard,
		staging: resStaging,
		exports: resExports,
		archive: resArchive,
		locID1:  loc1.ID,
		locID2:  loc2.ID,
		locID3:  loc3.ID,
	}
}

func enqueueEvent(t *testing.T, database *db.DB, eventType string, payload any) sqlcgen.EventQueue {
	t.Helper()
	ctx := context.Background()
	eventUUID := uuid.New().String()

	var payloadStr string
	switch p := payload.(type) {
	case string:
		payloadStr = p
	default:
		b, err := json.Marshal(p)
		require.NoError(t, err)
		payloadStr = string(b)
	}

	var row sqlcgen.EventQueue
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		row, err = q.EnqueueAgentEvent(ctx, sqlcgen.EnqueueAgentEventParams{
			EventUuid:   eventUUID,
			AgentID:     "agent-test",
			EventType:   eventType,
			PayloadJson: payloadStr,
		})
		return err
	})
	require.NoError(t, err)
	return row
}

func TestDrainer_NodeCreated_And_Idempotency(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, slog.Default())
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	fastHash := "0123456789abcdef"
	filePath := filepath.Join(env.staging, "raw_001.arw")
	payload := agent.NodeCreatedPayload{
		NodeUUID:          nodeUUID,
		StorageLocationID: env.locID1,
		FilePath:          filePath,
		FileName:          "raw_001.arw",
		FileExt:           ".arw",
		SizeBytes:         50000000,
		MtimeUnix:         time.Now().Unix(),
		FastHash:          &fastHash,
	}

	enqueueEvent(t, env.db, agent.EventNodeCreated, payload)

	// Process batch
	stats, err := drainer.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	// Verify node exists in database
	var node sqlcgen.MediaNode
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.GetMediaNodeByUUID(ctx, nodeUUID)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, filePath, node.FilePath)
	require.Equal(t, "ACTIVE", node.LifecycleState)

	// Enqueue duplicate event with same nodeUUID -> verify idempotency
	enqueueEvent(t, env.db, agent.EventNodeCreated, payload)
	stats, err = drainer.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 0, stats.Failed)
}

func TestDrainer_EdgeAttached(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	srcUUID := uuid.New().String()
	tgtUUID := uuid.New().String()

	// Enqueue parent and child nodes
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: srcUUID, FilePath: filepath.Join(env.staging, "parent.raw"),
	})
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: tgtUUID, FilePath: filepath.Join(env.staging, "child.jpg"),
	})

	// Enqueue edge
	enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID:   srcUUID,
		TargetNodeUUID:   tgtUUID,
		RelationshipType: "DERIVED_FROM",
		Confidence:       0.95,
		Tier:             2,
		Resolver:         "xmp",
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	// Verify edge exists
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		src, err := q.GetMediaNodeByUUID(ctx, srcUUID)
		if err != nil {
			return err
		}
		edges, err := q.ListEdgesBySource(ctx, src.ID)
		if err != nil {
			return err
		}
		require.Len(t, edges, 1)
		require.Equal(t, "DERIVED_FROM", edges[0].RelationshipType)
		require.Equal(t, 0.95, edges[0].Confidence)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_NodeMoved_And_Deleted(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "initial.mov"),
	})

	// Move node
	newPath := filepath.Join(env.staging, "renamed.mov")
	enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID:    nodeUUID,
		NewFilePath: newPath,
		NewFileName: "renamed.mov",
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)

	// Verify path updated
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		require.NoError(t, err)
		require.Equal(t, newPath, node.FilePath)
		require.Equal(t, "ACTIVE", node.LifecycleState)
		return nil
	})
	require.NoError(t, err)

	// Delete node
	enqueueEvent(t, env.db, agent.EventNodeDeleted, agent.NodeDeletedPayload{
		NodeUUID: nodeUUID,
	})

	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)

	// Verify lifecycle_state is MISSING (not removed from DB)
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		require.NoError(t, err)
		require.Equal(t, "MISSING", node.LifecycleState)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_PathRebased_Known_And_Unknown_Node(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	// Scenario 1: Known node rebased from staging to exports
	knownUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: knownUUID, FilePath: filepath.Join(env.staging, "clip1.mov"),
	})
	rebasedPath1 := filepath.Join(env.exports, "clip1.mov")
	enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID:       knownUUID,
		TargetFilePath: rebasedPath1,
	})

	// Scenario 2: Unknown node rebased (agent source of truth)
	unknownUUID := uuid.New().String()
	fastHash := "1122334455667788"
	rebasedPath2 := filepath.Join(env.exports, "clip2_direct.mov")
	enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID:       unknownUUID,
		TargetFilePath: rebasedPath2,
		FastHash:       &fastHash,
		SizeBytes:      1024,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		node1, err := q.GetMediaNodeByUUID(ctx, knownUUID)
		require.NoError(t, err)
		require.Equal(t, rebasedPath1, node1.FilePath)
		require.Equal(t, env.locID2, node1.StorageLocationID)

		node2, err := q.GetMediaNodeByUUID(ctx, unknownUUID)
		require.NoError(t, err)
		require.Equal(t, rebasedPath2, node2.FilePath)
		require.Equal(t, "ACTIVE", node2.LifecycleState)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_PoisonPill_And_MalformedPayload(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	// Enqueue malformed JSON
	badEvent := enqueueEvent(t, env.db, agent.EventNodeCreated, "{invalid-json")

	// Enqueue good event behind it
	goodUUID := uuid.New().String()
	goodPath := filepath.Join(env.staging, "good.jpg")
	goodEvent := enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: goodUUID, FilePath: goodPath,
	})

	// Process queue
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 1, stats.Failed)

	// Verify bad event is marked FAILED with error_log
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, badEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		require.True(t, ev.ErrorLog.Valid)
		require.Contains(t, ev.ErrorLog.String, "malformed")

		// Verify good event was processed
		evGood, err := q.GetAgentEventByUUID(ctx, goodEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "PROCESSED", evGood.Status)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_RefuseTier3Rebase(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "master.raw"),
	})
	// Try rebasing into read-only Tier 3 archive
	tier3Path := filepath.Join(env.archive, "master.raw")
	tier3Event := enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID:       nodeUUID,
		TargetFilePath: tier3Path,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 1, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, tier3Event.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		require.True(t, ev.ErrorLog.Valid)
		require.Contains(t, ev.ErrorLog.String, "read-only")
		return nil
	})
	require.NoError(t, err)
}
