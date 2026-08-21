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
	"github.com/s3ntin3l8/branchdam/internal/graph"
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
	root := t.TempDir()
	// A real temp-file DB, not ":memory:": a bare ":memory:" DSN gives every
	// SQLite connection its own isolated, empty database (no cache=shared),
	// so db.DB's reader pool -- a separate *sql.DB from the writer, up to
	// readerConns connections -- would never see anything the writer
	// commits. graph.Engine.ResolveAndCommit reads exclusively through the
	// reader pool (internal/graph/lookup.go), so any test exercising it
	// (WithEngine) needs a DB the reader pool can actually see. Matches
	// internal/httpapi's test harness (fullTestServer), which already uses
	// a temp file for the same reason.
	database, err := db.Open(context.Background(), filepath.Join(root, "agent_test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

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

// TestDrainer_NodeCreated_GPSMetadata proves a NodeCreatedPayload carrying
// gpsLatitude/gpsLongitude (#229) lands in node_metadata under
// source="exiftool" with exactly the key format
// internal/pipeline/commit.go's exifMetadata would have written for a
// scanned node -- Composite:GPSLatitude/Composite:GPSLongitude -- so
// downstream readers like httpapi's loadTagSet (which filters strictly on
// source=="exiftool") see an agent-ingested node's GPS point the same way
// they'd see a scanned one.
func TestDrainer_NodeCreated_GPSMetadata(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, slog.Default())
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	filePath := filepath.Join(env.staging, "DJI_0001.MP4")
	lat := 30.335120
	lon := -81.655480
	payload := agent.NodeCreatedPayload{
		NodeUUID:     nodeUUID,
		FilePath:     filePath,
		FileName:     "DJI_0001.MP4",
		FileExt:      ".MP4",
		MtimeUnix:    time.Now().Unix(),
		GPSLatitude:  &lat,
		GPSLongitude: &lon,
	}

	enqueueEvent(t, env.db, agent.EventNodeCreated, payload)

	stats, err := drainer.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	var node sqlcgen.MediaNode
	var rows []sqlcgen.NodeMetadatum
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.GetMediaNodeByUUID(ctx, nodeUUID)
		if err != nil {
			return err
		}
		rows, err = q.ListNodeMetadata(ctx, node.ID)
		return err
	})
	require.NoError(t, err)

	got := make(map[string]string, len(rows))
	for _, r := range rows {
		require.Equal(t, "exiftool", r.Source, "GPS rows must be written under source=exiftool to match a normal scan's exifMetadata convention")
		got[r.Key] = r.Value
	}
	require.Equal(t, "30.33512", got["Composite:GPSLatitude"])
	require.Equal(t, "-81.65548", got["Composite:GPSLongitude"])
}

// TestDrainer_NodeCreated_NoGPS_WritesNoMetadata proves a payload with no
// GPS fields set (the common case: most agent-ingested files aren't
// geotagged) writes zero node_metadata rows -- writeGPSMetadata must not
// write empty/placeholder rows when the payload simply omits GPS.
func TestDrainer_NodeCreated_NoGPS_WritesNoMetadata(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, slog.Default())
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	filePath := filepath.Join(env.staging, "raw_002.arw")
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID,
		FilePath: filePath,
	})

	stats, err := drainer.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)

	var node sqlcgen.MediaNode
	var rows []sqlcgen.NodeMetadatum
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.GetMediaNodeByUUID(ctx, nodeUUID)
		if err != nil {
			return err
		}
		rows, err = q.ListNodeMetadata(ctx, node.ID)
		return err
	})
	require.NoError(t, err)
	require.Empty(t, rows)
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
	// The file-not-yet-present case is deliberately transient (retries,
	// not an immediate fatal failure -- see ErrArchiveFileNotYetPresent's
	// doc comment), so pin maxRetries=1 to reach a terminal FAILED state
	// within this test's single DrainAll call.
	drainer.SetMaxRetries(1)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "master.raw"),
	})
	// Issue #167: a Tier 3 target is refused unless the file already exists
	// there. This target is never created on disk, so it must still be
	// refused -- see TestDrainer_PathRebased_Tier3WhenFileAlreadyArchived
	// for the success case.
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
	// See TestDrainer_RefuseTier3Rebase: file-not-yet-present is transient.
	drainer.SetMaxRetries(1)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "clip.mov"),
	})
	// Same as TestDrainer_RefuseTier3Rebase: refused because the file was
	// never created on disk at the Tier 3 target, not because Tier 3 is
	// categorically off-limits (issue #167).
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

// TestDrainer_PathRebased_Tier3WhenFileAlreadyArchived is the spec
// §9-named scenario (issue #167, resolving the contradiction in #58): once
// the workstation agent has already copied a node's bytes into the Tier 3
// archive itself, a EVENT_PATH_REBASED for that node_uuid must succeed --
// branchDAM only records the new location in the database, it never writes
// to the archive.
func TestDrainer_PathRebased_Tier3WhenFileAlreadyArchived(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "master.raw"),
	})

	// The workstation agent has already placed the bytes in the archive.
	tier3Path := filepath.Join(env.archive, "master.raw")
	require.NoError(t, os.WriteFile(tier3Path, []byte("archived bytes"), 0o644))

	rebaseEvent := enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID:       nodeUUID,
		TargetFilePath: tier3Path,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed) // node create + rebase
	require.Equal(t, 0, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, rebaseEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "PROCESSED", ev.Status)

		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		require.NoError(t, err)
		require.Equal(t, tier3Path, node.FilePath)
		require.Equal(t, env.locID3, node.StorageLocationID)
		require.Equal(t, "ACTIVE", node.LifecycleState)
		return nil
	})
	require.NoError(t, err)
}

// TestDrainer_NodeMoved_Tier3WhenFileAlreadyArchived mirrors
// TestDrainer_PathRebased_Tier3WhenFileAlreadyArchived for EVENT_NODE_MOVED.
func TestDrainer_NodeMoved_Tier3WhenFileAlreadyArchived(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "clip.mov"),
	})

	tier3Path := filepath.Join(env.archive, "clip.mov")
	require.NoError(t, os.WriteFile(tier3Path, []byte("archived bytes"), 0o644))

	moveEvent := enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID:    nodeUUID,
		NewFilePath: tier3Path,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, moveEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "PROCESSED", ev.Status)

		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		require.NoError(t, err)
		require.Equal(t, tier3Path, node.FilePath)
		require.Equal(t, env.locID3, node.StorageLocationID)
		return nil
	})
	require.NoError(t, err)
}

// TestDrainer_NonTier3ReadOnlyStaysRefusedEvenWithFile is issue #167's
// scoping guarantee: the file-already-present exemption applies ONLY to
// TIER3_MASTER_ARCHIVE, never to any other read-only location, even when
// the target file genuinely exists there. Without this test, a future
// change accidentally widening resolveRebaseTarget's Tier-3 check (e.g.
// dropping the loc.Tier != "TIER3_MASTER_ARCHIVE" guard, or a typo in the
// tier string literal) would go uncaught -- every other read-only fixture
// in this file is Tier 3, so none of them can distinguish "refused because
// Tier 3 and file missing" from "refused because read-only, full stop".
func TestDrainer_NonTier3ReadOnlyStaysRefusedEvenWithFile(t *testing.T) {
	env := setupTestDB(t)
	ctx := context.Background()

	// A read-only location that is NOT Tier 3 -- the schema permits this
	// (only Tier 3 is required to be read-only, not the reverse).
	roDir := filepath.Join(t.TempDir(), "readonly-import")
	require.NoError(t, os.MkdirAll(roDir, 0o755))
	resRoDir, err := filepath.EvalSymlinks(roDir)
	require.NoError(t, err)

	var roLoc sqlcgen.StorageLocation
	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		roLoc, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: 1, Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	guard := storage.NewGuard([]storage.Location{
		{ID: env.locID1, Name: "local_staging", RootPath: env.staging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: false},
		{ID: roLoc.ID, Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: true},
	})
	drainer := agent.NewDrainer(env.db, guard, nil)
	drainer.SetMaxRetries(1) // this refusal is fatal, but pin it anyway for a deterministic single-pass assertion

	// The file genuinely exists -- this must still be refused, unlike the
	// Tier-3 case, which would allow it.
	targetPath := filepath.Join(resRoDir, "already_here.raw")
	require.NoError(t, os.WriteFile(targetPath, []byte("bytes"), 0o644))

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "orig.raw"),
	})
	rebaseEvent := enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID:       nodeUUID,
		TargetFilePath: targetPath,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 1, stats.Failed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.GetAgentEventByUUID(ctx, rebaseEvent.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "FAILED", ev.Status)
		require.True(t, ev.ErrorLog.Valid)
		require.Contains(t, ev.ErrorLog.String, "read-only")

		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		require.NoError(t, err)
		require.NotEqual(t, targetPath, node.FilePath, "the refused rebase must not have taken effect")
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

// TestDrainer_WithEngineResolvesAgentCreatedNode is issue #166's third
// acceptance criterion: an agent-created node must not sit graph_status
// UNLINKED forever. FilenameStemResolver (a real, already-shipped Tier-2
// resolver) is enough to prove the hook actually runs: two nodes sharing a
// filename stem produce a NEEDS_REVIEW candidate at base confidence 0.60,
// below the 0.90 auto-accept threshold, so the resulting graph_status is
// deterministic without needing to control capture time/camera/directory
// boosts.
func TestDrainer_WithEngineResolvesAgentCreatedNode(t *testing.T) {
	env := setupTestDB(t)
	engine := graph.NewEngine(env.db, nil, graph.FilenameStemResolver{})
	drainer := agent.NewDrainer(env.db, env.guard, nil, agent.WithEngine(engine))
	ctx := context.Background()

	parentUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: parentUUID, FilePath: filepath.Join(env.staging, "IMG_0001.raw"),
		FilenameStem: strPtr("IMG_0001"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)

	childUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: childUUID, FilePath: filepath.Join(env.exports, "IMG_0001.jpg"),
		FilenameStem: strPtr("IMG_0001"),
	})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		child, err := q.GetMediaNodeByUUID(ctx, childUUID)
		require.NoError(t, err)
		require.Equal(t, "NEEDS_REVIEW", child.GraphStatus, "the FilenameStemResolver candidate must have been resolved and committed, not left UNLINKED")

		parent, err := q.GetMediaNodeByUUID(ctx, parentUUID)
		require.NoError(t, err)
		edges, err := q.ListEdgesBySource(ctx, parent.ID)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		require.Equal(t, "filename_stem", edges[0].Resolver)
		require.Equal(t, child.ID, edges[0].TargetNodeID)
		return nil
	})
	require.NoError(t, err)
}

// TestDrainer_WithoutEngineLeavesNodeUnlinked is the regression check for
// the above: a Drainer built without WithEngine (every pre-existing
// NewDrainer call site, including every other test in this file) must
// behave exactly as before -- no resolution attempted, node stays UNLINKED.
func TestDrainer_WithoutEngineLeavesNodeUnlinked(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	uuidA := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: uuidA, FilePath: filepath.Join(env.staging, "IMG_0002.raw"),
		FilenameStem: strPtr("IMG_0002"),
	})
	uuidB := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: uuidB, FilePath: filepath.Join(env.exports, "IMG_0002.jpg"),
		FilenameStem: strPtr("IMG_0002"),
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)

	err = env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		node, err := q.GetMediaNodeByUUID(ctx, uuidB)
		require.NoError(t, err)
		require.Equal(t, "UNLINKED", node.GraphStatus)
		return nil
	})
	require.NoError(t, err)
}

// TestDrainer_StartWaitProcessesInBackground is issue #166's first two
// acceptance criteria together: Start runs a background loop that picks up
// a newly enqueued event without an explicit DrainAll/ProcessPending call,
// and nudges after doing so; Wait joins cleanly once ctx is cancelled.
func TestDrainer_StartWaitProcessesInBackground(t *testing.T) {
	env := setupTestDB(t)
	nudged := make(chan struct{}, 1)
	drainer := agent.NewDrainer(env.db, env.guard, nil, agent.WithNudge(func() {
		select {
		case nudged <- struct{}{}:
		default:
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	ev := enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: uuid.New().String(), FilePath: filepath.Join(env.staging, "background.raw"),
	})

	drainer.Start(ctx, 10*time.Millisecond)

	select {
	case <-nudged:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the background drain loop to process the event and nudge")
	}

	cancel()
	waited := make(chan struct{})
	go func() {
		drainer.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait() did not return after ctx was cancelled")
	}

	err := env.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		got, err := q.GetAgentEventByUUID(context.Background(), ev.EventUuid)
		require.NoError(t, err)
		require.Equal(t, "PROCESSED", got.Status)
		return nil
	})
	require.NoError(t, err)
}

func strPtr(s string) *string { return &s }
