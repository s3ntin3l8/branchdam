package agent_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// TestDrainer_ContentDedupRecordsAliasAndFollowUpEventsResolveIt: a
// device's EVENT_NODE_CREATED whose full_hash a live node already holds is
// not inserted; the device's UUID becomes an alias of that node. The
// device's follow-up events name its own UUID, so they must resolve the
// alias instead of failing as "node not found" -- and must never mutate the
// canonical node, which belongs to whoever created it.
func TestDrainer_ContentDedupRecordsAliasAndFollowUpEventsResolveIt(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	phoneUUID := uuid.New().String()
	phonePath := filepath.Join(env.staging, "PXL_0001.dng")
	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: phoneUUID, FilePath: phonePath, FullHash: &hash,
	})

	wsUUID := uuid.New().String()
	wsJPEG := uuid.New().String()
	enqueueEventAs(t, env.db, "workstation-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: wsUUID, FilePath: filepath.Join(env.staging, "DSC0001.dng"), FullHash: &hash,
	})
	enqueueEventAs(t, env.db, "workstation-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: wsJPEG, FilePath: filepath.Join(env.staging, "DSC0001.jpg"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	_, err = env.db.Reader.GetMediaNodeByUUID(ctx, wsUUID)
	require.ErrorIs(t, err, sql.ErrNoRows, "the duplicate must not get a node of its own")
	alias, err := env.db.Reader.GetNodeUUIDAlias(ctx, wsUUID)
	require.NoError(t, err)
	require.Equal(t, phoneUUID, alias.NodeUuid)
	require.Equal(t, "workstation-1", alias.AgentID)

	phone := nodeState(t, env.db, phoneUUID)

	// Edge from the deduped RAW to the workstation's JPEG lands on the
	// canonical node.
	enqueueEventAs(t, env.db, "workstation-1", agent.EventEdgeAttached, agent.EdgeAttachedPayload{
		SourceNodeUUID: wsUUID, TargetNodeUUID: wsJPEG, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
	})
	// Rebase / move / delete of the aliased UUID are acknowledged and leave
	// the phone's node alone.
	archived := filepath.Join(env.archive, "DSC0001.dng")
	require.NoError(t, os.WriteFile(archived, []byte("bytes"), 0o644))
	enqueueEventAs(t, env.db, "workstation-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: wsUUID, TargetFilePath: archived, FullHash: &hash,
	})
	enqueueEventAs(t, env.db, "workstation-1", agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID: wsUUID, NewFilePath: filepath.Join(env.staging, "renamed.dng"),
	})
	enqueueEventAs(t, env.db, "workstation-1", agent.EventNodeDeleted, agent.NodeDeletedPayload{NodeUUID: wsUUID})

	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	after := nodeState(t, env.db, phoneUUID)
	require.Equal(t, phone.FilePath, after.FilePath)
	require.Equal(t, phone.StorageLocationID, after.StorageLocationID)
	require.Equal(t, "ACTIVE", after.LifecycleState)

	jpeg := nodeState(t, env.db, wsJPEG)
	edges, err := env.db.Reader.ListEdgesByTarget(ctx, jpeg.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	require.Equal(t, phone.ID, edges[0].SourceNodeID)

	// Another device's events never see through workstation-1's alias.
	enqueueEventAs(t, env.db, "phone-2", agent.EventNodeDeleted, agent.NodeDeletedPayload{NodeUUID: wsUUID})
	stats, err = drainer.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)
	require.Equal(t, "ACTIVE", nodeState(t, env.db, phoneUUID).LifecycleState)
}

// TestDrainer_PathRebasedUnknownUUIDWithExistingHashAliases: the
// insert-on-unknown branch of EVENT_PATH_REBASED applies the same content
// dedup as EVENT_NODE_CREATED instead of failing on
// ux_media_nodes_live_full_hash.
func TestDrainer_PathRebasedUnknownUUIDWithExistingHashAliases(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	first := uuid.New().String()
	enqueueEventAs(t, env.db, "ws-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: first, FilePath: filepath.Join(env.staging, "a.jpg"), FullHash: &hash,
	})
	second := uuid.New().String()
	enqueueEventAs(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: second, TargetFilePath: filepath.Join(env.exports, "b.jpg"), FullHash: &hash,
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	alias, err := env.db.Reader.GetNodeUUIDAlias(ctx, second)
	require.NoError(t, err)
	require.Equal(t, first, alias.NodeUuid)

	var count int64
	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		count, err = q.CountMediaNodes(ctx)
		return err
	}))
	require.Equal(t, int64(1), count)
}

// TestDrainer_AliasedPathRebasedIsAcknowledgedBeforeTargetValidation: an
// EVENT_PATH_REBASED for a UUID this device had content-deduplicated onto
// an existing node changes nothing, so it is acknowledged whatever its
// target is. The alias is resolved before resolveRebaseTarget, so neither a
// read-only non-Tier-3 target nor a Tier-3 target whose file is not there
// yet fails the event (both used to be refused first).
func TestDrainer_AliasedPathRebasedIsAcknowledgedBeforeTargetValidation(t *testing.T) {
	env := setupTestDB(t)
	ctx := context.Background()

	roDir := filepath.Join(t.TempDir(), "readonly-import")
	require.NoError(t, os.MkdirAll(roDir, 0o755))
	resRoDir, err := filepath.EvalSymlinks(roDir)
	require.NoError(t, err)
	var roLoc sqlcgen.StorageLocation
	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		roLoc, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: 1,
		})
		return err
	}))
	guard := storage.NewGuard([]storage.Location{
		{ID: env.locID1, Name: "local_staging", RootPath: env.staging, Tier: "TIER0_LOCAL_STAGING"},
		{ID: env.locID3, Name: "archive", RootPath: env.archive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
		{ID: roLoc.ID, Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: true},
	})
	drainer := agent.NewDrainer(env.db, guard, nil)
	drainer.SetMaxRetries(1) // a refusal would show up as Failed on this one pass

	hash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	phoneUUID := uuid.New().String()
	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: phoneUUID, FilePath: filepath.Join(env.staging, "PXL_0002.dng"), FullHash: &hash,
	})
	wsUUID := uuid.New().String()
	enqueueEventAs(t, env.db, "ws-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: wsUUID, FilePath: filepath.Join(env.staging, "DSC0002.dng"), FullHash: &hash,
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)
	require.Equal(t, 0, stats.Failed)
	before := nodeState(t, env.db, phoneUUID)

	readOnlyTarget := filepath.Join(resRoDir, "DSC0002.dng")
	require.NoError(t, os.WriteFile(readOnlyTarget, []byte("bytes"), 0o644))
	enqueueEventAs(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: wsUUID, TargetFilePath: readOnlyTarget, FullHash: &hash,
	})
	enqueueEventAs(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: wsUUID, TargetFilePath: filepath.Join(env.archive, "not-copied-yet.dng"), FullHash: &hash,
	})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)
	require.Equal(t, 0, stats.Failed, "an aliased rebase must be acknowledged, not refused on its target")

	after := nodeState(t, env.db, phoneUUID)
	require.Equal(t, before.FilePath, after.FilePath)
	require.Equal(t, before.StorageLocationID, after.StorageLocationID)

	// The same read-only target is still refused for a node the device
	// created itself, so the ordering change only exempts aliases.
	ownUUID := uuid.New().String()
	enqueueEventAs(t, env.db, "ws-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: ownUUID, FilePath: filepath.Join(env.staging, "own.dng"),
	})
	enqueueEventAs(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: ownUUID, TargetFilePath: readOnlyTarget,
	})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed, "the node create")
	require.Equal(t, 1, stats.Failed, "the rebase onto the read-only target")
}
