package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
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

func enqueueEvent(t *testing.T, database *db.DB, eventType string, payload any) sqlcgen.EnqueueAgentEventRow {
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

	var row sqlcgen.EnqueueAgentEventRow
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
		require.Equal(t, "AUTO_ACCEPTED", edges[0].ReviewState)

		tgt, err := q.GetMediaNodeByUUID(ctx, tgtUUID)
		if err != nil {
			return err
		}
		require.Equal(t, "LINKED", tgt.GraphStatus)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_EdgeAttached_ConfidenceGatesReviewState(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	makeNodes := func() (string, string) {
		src, tgt := uuid.New().String(), uuid.New().String()
		enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
			NodeUUID: src, FilePath: filepath.Join(env.staging, src+".raw"),
		})
		enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
			NodeUUID: tgt, FilePath: filepath.Join(env.staging, tgt+".jpg"),
		})
		return src, tgt
	}

	// Tier 2 at 0.80: below the 0.90 Tier 1/2 auto-accept threshold -> NEEDS_REVIEW, not AUTO_ACCEPTED.
	srcA, tgtA := makeNodes()
	enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: srcA, TargetNodeUUID: tgtA, RelationshipType: "DERIVED_FROM",
		Confidence: 0.80, Tier: 2,
	})

	// Same tier at 0.95 -> AUTO_ACCEPTED.
	srcB, tgtB := makeNodes()
	enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: srcB, TargetNodeUUID: tgtB, RelationshipType: "DERIVED_FROM",
		Confidence: 0.95, Tier: 2,
	})

	// Omitted confidence -> FAILED, not a free 1.0 auto-accept.
	srcC, tgtC := makeNodes()
	noConfEvent := enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: srcC, TargetNodeUUID: tgtC, RelationshipType: "DERIVED_FROM",
	})

	// Agent asserting a human review decision -> FAILED, naming the field.
	srcD, tgtD := makeNodes()
	humanStateEvent := enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: srcD, TargetNodeUUID: tgtD, RelationshipType: "DERIVED_FROM",
		Confidence: 0.95, Tier: 1, ReviewState: "CONFIRMED",
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 10, stats.Processed) // 8 node creates + 2 successful edges
	require.Equal(t, 2, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		srcNodeA, err := q.GetMediaNodeByUUID(ctx, srcA)
		require.NoError(t, err)
		edgesA, err := q.ListEdgesBySource(ctx, srcNodeA.ID)
		require.NoError(t, err)
		require.Len(t, edgesA, 1)
		require.Equal(t, "NEEDS_REVIEW", edgesA[0].ReviewState)

		srcNodeB, err := q.GetMediaNodeByUUID(ctx, srcB)
		require.NoError(t, err)
		edgesB, err := q.ListEdgesBySource(ctx, srcNodeB.ID)
		require.NoError(t, err)
		require.Len(t, edgesB, 1)
		require.Equal(t, "AUTO_ACCEPTED", edgesB[0].ReviewState)

		evC, err := q.GetAgentEventByUUID(ctx, noConfEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", evC.Status)
		require.True(t, evC.ErrorLog.Valid)
		require.Contains(t, evC.ErrorLog.String, "confidence")

		evD, err := q.GetAgentEventByUUID(ctx, humanStateEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", evD.Status)
		require.True(t, evD.ErrorLog.Valid)
		require.Contains(t, evD.ErrorLog.String, "reviewState")
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

func TestDrainer_RefuseTier3NodeMoved(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "clip.mov"),
	})
	// Attempt to move node directly into Tier 3 archive
	tier3Path := filepath.Join(env.archive, "clip.mov")
	moveEvent := enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID:    nodeUUID,
		NewFilePath: tier3Path,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 1, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, moveEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		require.True(t, ev.ErrorLog.Valid)
		require.Contains(t, ev.ErrorLog.String, "read-only")
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_EdgeAttached_SelfLoopFails(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "self.raw"),
	})
	// Attach self-loop edge (source == target). WouldCreateCycle catches
	// this before CreateMediaEdge is ever called -- a node is trivially its
	// own descendant in the recursive CTE's anchor row -- so this exercises
	// the cycle guard, not the schema's separate source<>target CHECK.
	selfLoopEvent := enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID:   nodeUUID,
		TargetNodeUUID:   nodeUUID,
		RelationshipType: "DERIVED_FROM",
		Confidence:       0.95,
		Tier:             1,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 1, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, selfLoopEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		require.True(t, ev.ErrorLog.Valid)
		require.Contains(t, ev.ErrorLog.String, "cycle")
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_RetryPersistenceAcrossInvocations(t *testing.T) {
	env := setupTestDB(t)
	drainer1 := agent.NewDrainer(env.db, env.guard, nil)
	drainer1.SetMaxRetries(3)
	ctx := context.Background()

	// Enqueue a node moved event with unknown node UUID -> transient error (lookup node for move)
	unknownUUID := uuid.New().String()
	event := enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID:    unknownUUID,
		NewFilePath: filepath.Join(env.staging, "nonexistent.mov"),
	})

	// Pass 1: drainer1 runs ProcessPending once -> failure is transient, increments retry_count in DB to 1
	stats1, err := drainer1.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 0, stats1.Processed)
	require.Equal(t, 0, stats1.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, event.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "PENDING", ev.Status)
		require.Equal(t, int64(1), ev.RetryCount)
		require.True(t, ev.ErrorLog.Valid)
		return nil
	})
	require.NoError(t, err)

	// Simulate restart: create a new Drainer instance with empty in-memory state
	drainer2 := agent.NewDrainer(env.db, env.guard, nil)
	drainer2.SetMaxRetries(3)

	// Pass 2: drainer2 runs ProcessPending -> retry_count becomes 2
	stats2, err := drainer2.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 0, stats2.Processed)
	require.Equal(t, 0, stats2.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, event.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "PENDING", ev.Status)
		require.Equal(t, int64(2), ev.RetryCount)
		return nil
	})
	require.NoError(t, err)

	// Pass 3: drainer2 runs ProcessPending -> attempts reaches 3 >= maxRetries -> marks FAILED
	stats3, err := drainer2.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 0, stats3.Processed)
	require.Equal(t, 1, stats3.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, event.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_Backoff(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	drainer.SetMaxRetries(10)
	drainer.SetRetryBackoff(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	// Enqueue a transient error event
	enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID:    uuid.New().String(),
		NewFilePath: filepath.Join(env.staging, "ghost.mov"),
	})

	// DrainAll should back off until ctx expires
	start := time.Now()
	_, err := drainer.DrainAll(ctx)
	duration := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, duration, 50*time.Millisecond)
}

func TestDrainer_RefuseArchivedNodeRebase(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	// Create a node, then archive it directly (as the pipeline's
	// version-collision path would on a real re-scan) without a
	// superseding successor -- ArchiveMediaNode alone is enough to
	// reproduce an ARCHIVED node with a stale node_uuid an agent might
	// still be holding.
	archivedUUID := uuid.New().String()
	var archivedID int64
	err := env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          archivedUUID,
			StorageLocationID: env.locID1,
			FilePath:          filepath.Join(env.staging, "old_version.mov"),
			FileName:          "old_version.mov",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		if err != nil {
			return err
		}
		archivedID = n.ID
		return q.ArchiveMediaNode(ctx, n.ID)
	})
	require.NoError(t, err)

	moveEvent := enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID:    archivedUUID,
		NewFilePath: filepath.Join(env.exports, "resurrected_via_move.mov"),
	})
	rebaseEvent := enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID:       archivedUUID,
		TargetFilePath: filepath.Join(env.exports, "resurrected_via_rebase.mov"),
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)
	require.Equal(t, 2, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, evUUID := range []string{moveEvent.EventUuid, rebaseEvent.EventUuid} {
			ev, err := q.GetAgentEventByUUID(ctx, evUUID)
			require.NoError(t, err)
			require.Equal(t, "FAILED", ev.Status)
			require.True(t, ev.ErrorLog.Valid)
			require.Contains(t, ev.ErrorLog.String, "archived")
		}

		// The row itself must be untouched: still ARCHIVED, still at its
		// original path -- not resurrected to ACTIVE at either target.
		node, err := q.GetMediaNodeByUUID(ctx, archivedUUID)
		require.NoError(t, err)
		require.Equal(t, archivedID, node.ID)
		require.Equal(t, "ARCHIVED", node.LifecycleState)
		require.Equal(t, filepath.Join(env.staging, "old_version.mov"), node.FilePath)
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_EdgeAttached_MultiHopCycleRefused(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	a, b, c, d := uuid.New().String(), uuid.New().String(), uuid.New().String(), uuid.New().String()
	for _, u := range []string{a, b, c, d} {
		enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
			NodeUUID: u, FilePath: filepath.Join(env.staging, u+".raw"),
		})
	}
	// A -> B -> C, both legitimate.
	enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: a, TargetNodeUUID: b, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
	})
	enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: b, TargetNodeUUID: c, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
	})
	// C -> A would close the cycle A->B->C->A -- must be refused.
	cycleEvent := enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: c, TargetNodeUUID: a, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
	})
	// Positive control in the same fixture: A -> D is unrelated to the
	// cycle and must still succeed. Without this, a fully-inverted
	// parent/child mapping in the cycle check would also make this test
	// pass (every edge refused), masking the bug.
	enqueueEvent(t, env.db, agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: a, TargetNodeUUID: d, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 7, stats.Processed) // 4 node creates + A->B, B->C, A->D
	require.Equal(t, 1, stats.Failed)    // C->A

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, cycleEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		require.True(t, ev.ErrorLog.Valid)
		require.Contains(t, ev.ErrorLog.String, "cycle")

		nodeC, err := q.GetMediaNodeByUUID(ctx, c)
		require.NoError(t, err)
		edgesFromC, err := q.ListEdgesBySource(ctx, nodeC.ID)
		require.NoError(t, err)
		require.Empty(t, edgesFromC, "the refused C->A edge must not have been created")

		nodeA, err := q.GetMediaNodeByUUID(ctx, a)
		require.NoError(t, err)
		edgesFromA, err := q.ListEdgesBySource(ctx, nodeA.ID)
		require.NoError(t, err)
		require.Len(t, edgesFromA, 2, "A->B and A->D (the positive control) must both have succeeded")
		return nil
	})
	require.NoError(t, err)
}

func TestDrainer_UnresolvableStorageLocationRefused(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	outsidePath := filepath.Join(t.TempDir(), "not_under_any_root.mov")

	// No payload storageLocationId at all.
	noIDUUID := uuid.New().String()
	noIDEvent := enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: noIDUUID, FilePath: outsidePath,
	})

	// A payload storageLocationId pointing at a real (Tier 3, read-only)
	// location must NOT let the node in via that ID -- Resolve(FilePath) is
	// the only source of truth, and Resolve fails for an out-of-root path
	// regardless of what storageLocationId claims.
	withIDUUID := uuid.New().String()
	withIDEvent := enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: withIDUUID, FilePath: outsidePath, StorageLocationID: env.locID3,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)
	require.Equal(t, 2, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, evUUID := range []string{noIDEvent.EventUuid, withIDEvent.EventUuid} {
			ev, err := q.GetAgentEventByUUID(ctx, evUUID)
			require.NoError(t, err)
			require.Equal(t, "FAILED", ev.Status)
		}
		for _, nodeUUID := range []string{noIDUUID, withIDUUID} {
			_, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
			require.ErrorIs(t, err, sql.ErrNoRows, "neither node should have been created, and certainly not at location id 1")
		}
		return nil
	})
	require.NoError(t, err)
}
