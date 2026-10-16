package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// createNodeAs creates a node through EVENT_NODE_CREATED as agentID (so the
// node_creators row records that device) and drains it.
func createNodeAs(t *testing.T, env *testEnv, drainer *agent.Drainer, agentID string) (string, int64) {
	t.Helper()
	nodeUUID := uuid.New().String()
	enqueueEventAs(t, env.db, agentID, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, nodeUUID+".raw"),
	})
	stats, err := drainer.DrainAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	return nodeUUID, nodeState(t, env.db, nodeUUID).ID
}

// createScannedNode inserts a node the way the server's own scan does: no
// node_creators row, so no device owns it.
func createScannedNode(t *testing.T, env *testEnv) (string, int64) {
	t.Helper()
	nodeUUID := uuid.New().String()
	var id int64
	require.NoError(t, env.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: nodeUUID, StorageLocationID: env.locID1,
			FilePath: filepath.Join(env.staging, nodeUUID+".arw"), FileName: nodeUUID + ".arw",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		id = n.ID
		return err
	}))
	return nodeUUID, id
}

// enqueueAsUUID is enqueueEventAs that returns the event UUID so the test
// can read the event's final status back.
func enqueueAsUUID(t *testing.T, database *db.DB, agentID, eventType string, payload any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	eventUUID := uuid.New().String()
	require.NoError(t, database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.EnqueueAgentEvent(context.Background(), sqlcgen.EnqueueAgentEventParams{
			EventUuid: eventUUID, AgentID: agentID, EventType: eventType, PayloadJson: string(b),
		})
		return err
	}))
	return eventUUID
}

func eventByUUID(t *testing.T, database *db.DB, eventUUID string) sqlcgen.GetAgentEventByUUIDRow {
	t.Helper()
	ev, err := database.Reader.GetAgentEventByUUID(context.Background(), eventUUID)
	require.NoError(t, err)
	return ev
}

func edgesInto(t *testing.T, database *db.DB, targetID int64) []sqlcgen.MediaEdge {
	t.Helper()
	var edges []sqlcgen.MediaEdge
	require.NoError(t, database.ReadTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		edges, err = q.ListEdgesByTarget(context.Background(), targetID)
		return err
	}))
	return edges
}

// attachAs enqueues one EVENT_EDGE_ATTACHED as agentID, drains, and returns
// the event's final row.
func attachAs(t *testing.T, env *testEnv, drainer *agent.Drainer, agentID string, payload any) sqlcgen.GetAgentEventByUUIDRow {
	t.Helper()
	eventUUID := enqueueAsUUID(t, env.db, agentID, agent.EventEdgeAttached, payload)
	_, err := drainer.DrainAll(context.Background())
	require.NoError(t, err)
	return eventByUUID(t, env.db, eventUUID)
}

// TestDrainer_EdgeAttached_DeviceOwnership covers issue #504: a paired
// device could attach AUTO_ACCEPTED edges between arbitrary nodes, which
// drive graph_status and the Tier-3 ancestor walk that authorises prune.
// Now only an edge into a child the device created can be auto-accepted;
// any other edge is stored NEEDS_REVIEW, which never authorises prune.
func TestDrainer_EdgeAttached_DeviceOwnership(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)

	t.Run("owning both ends keeps the confidence-derived review state", func(t *testing.T) {
		src, _ := createNodeAs(t, env, drainer, "ws-1")
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 2,
		})
		require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "AUTO_ACCEPTED", edges[0].ReviewState)
		require.Equal(t, int64(2), edges[0].Tier)
	})

	t.Run("owned target with a server-scanned source auto-accepts normally", func(t *testing.T) {
		// The legitimate flow named in #504: the device's own derived file
		// pointing at a server-scanned master.
		src, _ := createScannedNode(t, env)
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "DERIVED_FROM", Confidence: 1.0, Tier: 1,
		})
		require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "AUTO_ACCEPTED", edges[0].ReviewState)
		require.Equal(t, "LINKED", nodeState(t, env.db, tgt).GraphStatus)
	})

	t.Run("owned target with another device's source auto-accepts normally", func(t *testing.T) {
		src, _ := createNodeAs(t, env, drainer, "phone-2")
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 2,
		})
		require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "AUTO_ACCEPTED", edges[0].ReviewState)
	})

	t.Run("target owned by nobody is stored NEEDS_REVIEW even at confidence 1.0", func(t *testing.T) {
		// Luminar/Resolve-style: both files were scanned by the server.
		// The edge still arrives, but only as a suggestion that can't
		// authorise prune until a human confirms it.
		src, _ := createScannedNode(t, env)
		tgt, tgtID := createScannedNode(t, env)
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "DERIVED_FROM", Confidence: 1.0, Tier: 1,
		})
		require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "NEEDS_REVIEW", edges[0].ReviewState)
		require.Equal(t, "NEEDS_REVIEW", nodeState(t, env.db, tgt).GraphStatus)
	})

	t.Run("target owned by another device is stored NEEDS_REVIEW", func(t *testing.T) {
		src, _ := createNodeAs(t, env, drainer, "ws-1")
		tgt, tgtID := createNodeAs(t, env, drainer, "phone-2")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "PROJECT_SIDECAR", Confidence: 1.0, Tier: 1,
		})
		require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "NEEDS_REVIEW", edges[0].ReviewState)
	})

	t.Run("an existing edge's review state is never overwritten", func(t *testing.T) {
		src, _ := createScannedNode(t, env)
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
		payload := agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		}
		require.Equal(t, "PROCESSED", attachAs(t, env, drainer, "ws-1", payload).Status)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "AUTO_ACCEPTED", edges[0].ReviewState)

		// A human rejects it; neither the owner re-sending it nor another
		// device sending it may revive or change it (Invariant 6).
		require.NoError(t, env.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			_, err := q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{
				ID: edges[0].ID, ReviewedBy: sql.NullString{String: "user:reviewer", Valid: true},
			})
			return err
		}))
		require.Equal(t, "PROCESSED", attachAs(t, env, drainer, "ws-1", payload).Status)
		require.Equal(t, "PROCESSED", attachAs(t, env, drainer, "phone-2", payload).Status)
		edges = edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, "REJECTED", edges[0].ReviewState)
	})
}

func TestDrainer_EdgeAttached_PayloadRules(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)

	t.Run("integer node IDs are refused", func(t *testing.T) {
		src, srcID := createNodeAs(t, env, drainer, "ws-1")
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")

		// IDs alone (the pre-fix path that skipped the UUID lookup).
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeID: srcID, TargetNodeID: tgtID, RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		})
		require.Equal(t, "FAILED", ev.Status)
		require.Contains(t, ev.ErrorLog.String, "sourceNodeId/targetNodeId are not accepted")

		// IDs alongside valid UUIDs are refused too, not silently ignored.
		ev = attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, TargetNodeID: tgtID,
			RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		})
		require.Equal(t, "FAILED", ev.Status)
		require.Contains(t, ev.ErrorLog.String, "sourceNodeId/targetNodeId are not accepted")
		require.Empty(t, edgesInto(t, env.db, tgtID))
	})

	t.Run("malformed UUID is refused", func(t *testing.T) {
		_, tgtID := createNodeAs(t, env, drainer, "ws-1")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: "../../etc/passwd", TargetNodeUUID: "not-a-uuid",
			RelationshipType: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		})
		require.Equal(t, "FAILED", ev.Status)
		require.Contains(t, ev.ErrorLog.String, agent.ErrInvalidNodeUUID.Error())
		require.Empty(t, edgesInto(t, env.db, tgtID))
	})

	t.Run("unknown relationshipType is refused", func(t *testing.T) {
		src, _ := createNodeAs(t, env, drainer, "ws-1")
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "OWNS_EVERYTHING", Confidence: 0.95, Tier: 1,
		})
		require.Equal(t, "FAILED", ev.Status)
		require.Contains(t, ev.ErrorLog.String, "unknown relationshipType")
		require.Empty(t, edgesInto(t, env.db, tgtID))
	})

	t.Run("tier 3 is clamped to 2", func(t *testing.T) {
		// 0.87 clears Tier 3's 0.85 auto-accept threshold but not Tier 2's
		// 0.90: a device claiming tier 3 must not get the lower bar.
		src, _ := createNodeAs(t, env, drainer, "ws-1")
		tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
		ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
			SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: "DERIVED_FROM", Confidence: 0.87, Tier: 3,
		})
		require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
		edges := edgesInto(t, env.db, tgtID)
		require.Len(t, edges, 1)
		require.Equal(t, int64(2), edges[0].Tier)
		require.Equal(t, "NEEDS_REVIEW", edges[0].ReviewState)
	})

	t.Run("tiers the workstation agent sends are kept", func(t *testing.T) {
		// Luminar sync sends tier 2 at 0.89 (NEEDS_REVIEW); Resolve sends
		// PROJECT_SIDECAR tier 1 at 1.00 (AUTO_ACCEPTED).
		for _, tc := range []struct {
			rel        string
			tier       int64
			confidence float64
			want       string
		}{
			{"DERIVED_FROM", 2, 0.89, "NEEDS_REVIEW"},
			{"PROJECT_SIDECAR", 1, 1.00, "AUTO_ACCEPTED"},
		} {
			src, _ := createNodeAs(t, env, drainer, "ws-1")
			tgt, tgtID := createNodeAs(t, env, drainer, "ws-1")
			ev := attachAs(t, env, drainer, "ws-1", agent.EdgeAttachedPayload{
				SourceNodeUUID: src, TargetNodeUUID: tgt, RelationshipType: tc.rel, Confidence: tc.confidence, Tier: tc.tier,
			})
			require.Equal(t, "PROCESSED", ev.Status, ev.ErrorLog.String)
			edges := edgesInto(t, env.db, tgtID)
			require.Len(t, edges, 1)
			require.Equal(t, tc.tier, edges[0].Tier)
			require.Equal(t, tc.want, edges[0].ReviewState)
		}
	})
}

// TestDrainer_RefuseTrashedNodeRebase: RebaseNodePathByUUID flips
// lifecycle_state back to ACTIVE, so EVENT_NODE_MOVED / EVENT_PATH_REBASED
// on a TRASHED node used to restore it out of the trash. The node is owned
// by the sending device, so the refusal is the TRASHED rule, not ownership.
func TestDrainer_RefuseTrashedNodeRebase(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	origPath := filepath.Join(env.staging, "deleted.mov")
	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: nodeUUID, StorageLocationID: env.locID1, FilePath: origPath, FileName: "deleted.mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "TRASHED",
		}); err != nil {
			return err
		}
		return q.SetNodeCreator(ctx, sqlcgen.SetNodeCreatorParams{NodeUuid: nodeUUID, AgentID: "ws-1"})
	}))

	moveUUID := enqueueAsUUID(t, env.db, "ws-1", agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID: nodeUUID, NewFilePath: filepath.Join(env.exports, "via_move.mov"),
	})
	rebaseUUID := enqueueAsUUID(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: nodeUUID, TargetFilePath: filepath.Join(env.exports, "via_rebase.mov"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)
	require.Equal(t, 2, stats.Failed, "both rebases of a TRASHED node must fail permanently")

	for _, evUUID := range []string{moveUUID, rebaseUUID} {
		ev := eventByUUID(t, env.db, evUUID)
		require.Equal(t, "FAILED", ev.Status)
		require.Contains(t, ev.ErrorLog.String, "trashed")
	}

	node := nodeState(t, env.db, nodeUUID)
	require.Equal(t, "TRASHED", node.LifecycleState, "a refused rebase must not restore the node")
	require.Equal(t, origPath, node.FilePath)
}
