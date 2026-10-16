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
