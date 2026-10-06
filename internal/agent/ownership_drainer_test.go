package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// enqueueEventAs is enqueueEvent with an explicit agent id, so two paired
// devices can act on the same library.
func enqueueEventAs(t *testing.T, database *db.DB, agentID, eventType string, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.EnqueueAgentEvent(context.Background(), sqlcgen.EnqueueAgentEventParams{
			EventUuid: uuid.New().String(), AgentID: agentID, EventType: eventType, PayloadJson: string(b),
		})
		return err
	}))
}

func nodeState(t *testing.T, database *db.DB, nodeUUID string) sqlcgen.MediaNode {
	t.Helper()
	var node sqlcgen.MediaNode
	require.NoError(t, database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.GetMediaNodeByUUID(context.Background(), nodeUUID)
		return err
	}))
	return node
}

func TestDrainer_CreatorIsRecordedAndOtherDeviceCannotMoveOrDelete(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	orig := filepath.Join(env.staging, "owned.mov")
	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeCreated, agent.NodeCreatedPayload{NodeUUID: nodeUUID, FilePath: orig})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)

	// phone-2 knows the UUID but didn't create the node.
	enqueueEventAs(t, env.db, "phone-2", agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID: nodeUUID, NewFilePath: filepath.Join(env.staging, "hijacked.mov"), NewFileName: "hijacked.mov",
	})
	enqueueEventAs(t, env.db, "phone-2", agent.EventNodeDeleted, agent.NodeDeletedPayload{NodeUUID: nodeUUID})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)
	require.Equal(t, 2, stats.Failed, "both foreign events must fail permanently")

	node := nodeState(t, env.db, nodeUUID)
	require.Equal(t, orig, node.FilePath, "foreign move must not re-point the node")
	require.Equal(t, "ACTIVE", node.LifecycleState, "foreign delete must not trash the node")

	// The creator can still manage it.
	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeDeleted, agent.NodeDeletedPayload{NodeUUID: nodeUUID})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, "TRASHED", nodeState(t, env.db, nodeUUID).LifecycleState)
}

func TestDrainer_ServerCreatedNodeCannotBeTouchedByDevice(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	// A node the server's own scan indexed: no node_creators row.
	nodeUUID := uuid.New().String()
	path := filepath.Join(env.staging, "scanned.mov")
	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: nodeUUID, StorageLocationID: env.locID1, FilePath: path, FileName: "scanned.mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	}))

	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeDeleted, agent.NodeDeletedPayload{NodeUUID: nodeUUID})
	enqueueEventAs(t, env.db, "phone-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: nodeUUID, TargetFilePath: filepath.Join(env.staging, "elsewhere.mov"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Failed)
	node := nodeState(t, env.db, nodeUUID)
	require.Equal(t, "ACTIVE", node.LifecycleState)
	require.Equal(t, path, node.FilePath)
}

func TestDrainer_RebaseCannotOverwriteExistingFullHash(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	real := strings.Repeat("a", 64)
	forged := strings.Repeat("b", 64)
	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "h.mov"), FullHash: &real,
	})
	enqueueEventAs(t, env.db, "phone-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: nodeUUID, TargetFilePath: filepath.Join(env.staging, "h2.mov"), FullHash: &forged,
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 1, stats.Failed, "re-asserting a different hash must fail")
	node := nodeState(t, env.db, nodeUUID)
	require.NotNil(t, node.FullHash)
	require.Equal(t, real, *node.FullHash)
}

func TestDrainer_RejectsNonUUIDNodeIdentifiers(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	enqueueEventAs(t, env.db, "phone-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: "../../etc/cron.d/x", FilePath: filepath.Join(env.staging, "evil.mov"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Failed)
	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.GetMediaNodeByUUID(ctx, "../../etc/cron.d/x")
		require.ErrorIs(t, err, sql.ErrNoRows)
		return nil
	}))
}
