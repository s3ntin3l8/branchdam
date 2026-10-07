package agent_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// A NODE_DELETED event trashes the file (a real rename) inside ProcessPending's
// per-event tx. If a LATER step in that tx fails (here: marking the event
// processed), the rollback only reverts rows -- the file must be put back, and
// the Immich rescan must not fire for an event that did not commit.
func TestDrainer_NodeDeleted_TxRollbackRestoresFileAndSkipsScan(t *testing.T) {
	env := setupTestDB(t)
	scanner := &mockImmichScanner{}
	drainer := agent.NewDrainer(env.db, env.guard, slog.Default(), agent.WithImmichScanner(scanner))
	drainer.SetMaxRetries(1)

	file := filepath.Join(env.staging, "ROLLBACK.JPG")
	require.NoError(t, os.WriteFile(file, []byte("bytes"), 0o644))
	nodeUUID := uuid.New().String()
	require.NoError(t, env.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: nodeUUID, StorageLocationID: env.locID1, FilePath: file,
			FileName: "ROLLBACK.JPG", FileExt: ".JPG", SizeBytes: 5, MtimeUnix: time.Now().Unix(),
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	}))
	enqueueEvent(t, env.db, agent.EventNodeDeleted, agent.NodeDeletedPayload{NodeUUID: nodeUUID})

	_, err := env.db.ExecInTx(context.Background(), `CREATE TRIGGER fail_mark_processed
BEFORE UPDATE ON event_queue WHEN NEW.status = 'PROCESSED'
BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)

	stats, err := drainer.ProcessPending(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)

	_, statErr := os.Stat(file)
	require.NoError(t, statErr, "file must be back at its original path after the tx rolled back")
	trashed, _ := filepath.Glob(filepath.Join(env.staging, ".trash", "*"))
	require.Empty(t, trashed, ".trash must hold nothing for a rolled-back delete")
	require.False(t, scanner.scanned, "Immich rescan must not fire for a rolled-back event")
	require.Equal(t, "ACTIVE", nodeState(t, env.db, nodeUUID).LifecycleState)
}
