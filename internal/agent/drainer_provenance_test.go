package agent_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// TestDrainer_RecordsAgentAssertedHashProvenance backs #505: every full_hash
// the drainer writes from an agent event -- EVENT_NODE_CREATED, and
// EVENT_PATH_REBASED for both an unknown node and a known node without a
// hash -- is recorded as agent-asserted, so it authorises nothing until
// internal/rehash re-reads the file.
func TestDrainer_RecordsAgentAssertedHashProvenance(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()
	h1, h2, h3 := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)

	created := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: created, FilePath: filepath.Join(env.staging, "a.mov"), FullHash: &h1,
	})
	rebasedUnknown := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: rebasedUnknown, TargetFilePath: filepath.Join(env.exports, "b.mov"), FullHash: &h2,
	})
	rebasedKnown := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: rebasedKnown, FilePath: filepath.Join(env.staging, "c.mov"),
	})
	enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: rebasedKnown, TargetFilePath: filepath.Join(env.exports, "c.mov"), FullHash: &h3,
	})

	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	for nodeUUID, want := range map[string]string{created: h1, rebasedUnknown: h2, rebasedKnown: h3} {
		n, err := env.db.Reader.GetMediaNodeByUUID(ctx, nodeUUID)
		require.NoError(t, err)
		p, err := env.db.Reader.GetFullHashProvenance(ctx, n.ID)
		require.NoError(t, err, "node %s has no provenance row", nodeUUID)
		require.Equal(t, db.HashSourceAgent, p.Source, "node %s", nodeUUID)
		require.Equal(t, want, p.FullHash, "node %s", nodeUUID)
	}
}

// TestDrainer_ContentDedupAliasLeavesCanonicalProvenanceAlone: when a
// device's EVENT_NODE_CREATED (or the insert-on-unknown branch of
// EVENT_PATH_REBASED) is content-deduplicated onto an existing node, only an
// alias is recorded. The existing node's hash came from whoever created it,
// so the device's assertion must not touch that node's full_hash_provenance:
// neither re-stamp a SERVER row nor invent an AGENT row where there is none.
// A follow-up EVENT_PATH_REBASED of the aliased UUID is acknowledged without
// touching it either.
func TestDrainer_ContentDedupAliasLeavesCanonicalProvenanceAlone(t *testing.T) {
	env := setupTestDB(t)
	drainer := agent.NewDrainer(env.db, env.guard, nil)
	ctx := context.Background()
	scanned, legacy := strings.Repeat("4", 64), strings.Repeat("5", 64)
	scannedUUID, legacyUUID := uuid.New().String(), uuid.New().String()

	// scannedUUID: a node the server hashed itself (SERVER row, backdated so
	// any re-record would be visible). legacyUUID: a node with a hash but no
	// provenance row at all.
	var scannedID, legacyID int64
	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, n := range []struct {
			uuid, name string
			hash       *string
			id         *int64
		}{
			{scannedUUID, "scanned.dng", &scanned, &scannedID},
			{legacyUUID, "legacy.dng", &legacy, &legacyID},
		} {
			row, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          n.uuid,
				StorageLocationID: env.locID1,
				FilePath:          filepath.Join(env.staging, n.name),
				FileName:          n.name,
				FullHash:          n.hash,
				IndexingStatus:    "INDEXED_FULL",
				GraphStatus:       "UNLINKED",
				LifecycleState:    "ACTIVE",
			})
			if err != nil {
				return err
			}
			*n.id = row.ID
		}
		return db.RecordFullHashSource(ctx, q, scannedID, &scanned, db.HashSourceServer)
	}))
	_, err := env.db.ExecInTx(ctx, `UPDATE full_hash_provenance SET recorded_at = 1000 WHERE node_id = ?`, scannedID)
	require.NoError(t, err)

	createdDup, rebasedDup := uuid.New().String(), uuid.New().String()
	enqueueEventAs(t, env.db, "ws-1", agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: createdDup, FilePath: filepath.Join(env.staging, "DSC0001.dng"), FullHash: &scanned,
	})
	enqueueEventAs(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: rebasedDup, TargetFilePath: filepath.Join(env.exports, "DSC0002.dng"), FullHash: &legacy,
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	for dup, canonical := range map[string]string{createdDup: scannedUUID, rebasedDup: legacyUUID} {
		alias, err := env.db.Reader.GetNodeUUIDAlias(ctx, dup)
		require.NoError(t, err)
		require.Equal(t, canonical, alias.NodeUuid)
	}

	// The aliased UUID's own rebase is acknowledged and changes nothing.
	enqueueEventAs(t, env.db, "ws-1", agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: createdDup, TargetFilePath: filepath.Join(env.exports, "DSC0001.dng"), FullHash: &scanned,
	})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, 0, stats.Failed)

	p, err := env.db.Reader.GetFullHashProvenance(ctx, scannedID)
	require.NoError(t, err)
	require.Equal(t, db.HashSourceServer, p.Source)
	require.Equal(t, scanned, p.FullHash)
	require.Equal(t, int64(1000), p.RecordedAt, "the alias must not re-record the canonical node's provenance")

	_, err = env.db.Reader.GetFullHashProvenance(ctx, legacyID)
	require.ErrorIs(t, err, sql.ErrNoRows, "the alias must not record AGENT provenance on the canonical node")
}
