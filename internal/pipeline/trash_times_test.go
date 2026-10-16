package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/prune"
)

const trashTimesNodeUUID = "018d3b2f-7630-7e50-9844-3d96e9592491"

// seedTrashTimesNode creates a storage location at root and one ACTIVE
// node with a real file under it.
func seedTrashTimesNode(t *testing.T, database *db.DB, root string) (nodeID int64, path string) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(root, "2026", "clip.mov")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("clip bytes"), 0o644))
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{Name: "staging", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH"})
		if err != nil {
			return err
		}
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: trashTimesNodeUUID, StorageLocationID: loc.ID, FilePath: path, FileName: "clip.mov", FileExt: ".mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		nodeID = n.ID
		return err
	}))
	return nodeID, path
}

// TestTrashPurge_UsesRecordedTrashTimeNotMtime pins the audit finding that
// the hourly purge aged .trash/ copies by mtime: when moveToTrash's
// Chtimes failed, a just-trashed file kept its original (old) mtime and
// was unlinked on the next pass. The trash time is now recorded in
// node_trash_times when the node is trashed and the purge reads it.
func TestTrashPurge_UsesRecordedTrashTimeNotMtime(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)
	nodeID, path := seedTrashTimesNode(t, database, root)

	_, err := TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.NoError(t, err)
	rel, err := filepath.Rel(root, path)
	require.NoError(t, err)
	trashCopy := trashPathFor(root, rel, trashTimesNodeUUID)
	require.FileExists(t, trashCopy)

	rec, err := database.Reader.GetNodeTrashTime(ctx, nodeID)
	require.NoError(t, err)
	require.True(t, rec.TrashedAt.Valid, "trashing must record trashed_at")
	assert.InDelta(t, time.Now().Unix(), rec.TrashedAt.Int64, 5)

	// Simulate the failed Chtimes: the copy carries the original file's
	// year-old mtime.
	old := time.Now().Add(-365 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(trashCopy, old, old))

	lookup := TrashTimeLookup(database)
	res, err := prune.PurgeTrash(ctx, guard, root, 30, time.Now().UTC(), lookup)
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.Zero(t, res.FilesPurged, "a copy trashed just now must survive regardless of its mtime")
	require.FileExists(t, trashCopy)

	// Once the recorded trash time is past retention it is purged, even
	// though nothing about the file changed.
	_, err = database.ExecInTx(ctx, `UPDATE node_trash_times SET trashed_at = ? WHERE node_id = ?`,
		time.Now().Add(-31*24*time.Hour).Unix(), nodeID)
	require.NoError(t, err)
	res, err = prune.PurgeTrash(ctx, guard, root, 30, time.Now().UTC(), lookup)
	require.NoError(t, err)
	assert.Equal(t, 1, res.FilesPurged)
	assert.NoFileExists(t, trashCopy)
}

// TestTrashPurge_FreshMtimeDoesNotOutliveRecordedTime is the converse: the
// recorded time is authoritative in both directions.
func TestTrashPurge_FreshMtimeDoesNotOutliveRecordedTime(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)
	nodeID, path := seedTrashTimesNode(t, database, root)
	_, err := TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.NoError(t, err)
	rel, _ := filepath.Rel(root, path)
	trashCopy := trashPathFor(root, rel, trashTimesNodeUUID)

	_, err = database.ExecInTx(ctx, `UPDATE node_trash_times SET trashed_at = ? WHERE node_id = ?`,
		time.Now().Add(-40*24*time.Hour).Unix(), nodeID)
	require.NoError(t, err)
	res, err := prune.PurgeTrash(ctx, guard, root, 30, time.Now().UTC(), TrashTimeLookup(database))
	require.NoError(t, err)
	assert.Equal(t, 1, res.FilesPurged)
	assert.NoFileExists(t, trashCopy)
}

// TestTrashTimes_RestoreClearsAndRetrashRestarts covers the lifecycle of
// the recorded time: restore clears it (so a restored node's stale copy
// name can't be aged by it) and a later re-trash starts a fresh window.
func TestTrashTimes_RestoreClearsAndRetrashRestarts(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)
	nodeID, _ := seedTrashTimesNode(t, database, root)

	_, err := TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.NoError(t, err)
	_, err = database.ExecInTx(ctx, `UPDATE node_trash_times SET trashed_at = 1000 WHERE node_id = ?`, nodeID)
	require.NoError(t, err)

	_, err = RestoreTrashedAsset(ctx, database, guard, testLogger(), nodeID)
	require.NoError(t, err)
	rec, err := database.Reader.GetNodeTrashTime(ctx, nodeID)
	require.NoError(t, err, "the row is kept, never deleted")
	assert.False(t, rec.TrashedAt.Valid, "restore must clear trashed_at")

	_, err = TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.NoError(t, err)
	rec, err = database.Reader.GetNodeTrashTime(ctx, nodeID)
	require.NoError(t, err)
	require.True(t, rec.TrashedAt.Valid)
	assert.InDelta(t, time.Now().Unix(), rec.TrashedAt.Int64, 5, "re-trash must restart the retention window")
}

// TestTrashTimeLookup_UnknownFilesFallBack checks every case the lookup
// must hand back to the mtime fallback rather than guess at.
func TestTrashTimeLookup_UnknownFilesFallBack(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)
	nodeID, path := seedTrashTimesNode(t, database, root)
	lookup := TrashTimeLookup(database)
	rel, _ := filepath.Rel(root, path)
	trashCopy := trashPathFor(root, rel, trashTimesNodeUUID)

	// Node not trashed yet: its would-be copy name is unknown.
	_, known, err := lookup(ctx, root, trashCopy)
	require.NoError(t, err)
	assert.False(t, known)

	_, err = TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.NoError(t, err)
	at, known, err := lookup(ctx, root, trashCopy)
	require.NoError(t, err)
	assert.True(t, known)
	assert.InDelta(t, time.Now().Unix(), at.Unix(), 5)

	for name, p := range map[string]string{
		"legacy 8-char suffix":   legacyTrashPathFor(root, rel, trashTimesNodeUUID),
		"no uuid in name":        filepath.Join(root, ".trash", "2026", "clip.mov"),
		"uuid of unknown node":   filepath.Join(root, ".trash", "2026", "clip.0190aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee.mov"),
		"right uuid, wrong path": filepath.Join(root, ".trash", "elsewhere", "clip."+trashTimesNodeUUID+".mov"),
	} {
		_, known, err := lookup(ctx, root, p)
		require.NoError(t, err, name)
		assert.False(t, known, name)
	}
}

func TestNodeUUIDFromTrashName(t *testing.T) {
	const id = trashTimesNodeUUID
	for in, want := range map[string]string{
		"/r/.trash/a/clip." + id + ".mov":      id,
		"/r/.trash/a/my.clip.v2." + id + ".jp": id,
		"/r/.trash/a/README." + id:             id,
		"/r/.trash/a/clip.018d3b2f.mov":        "",
		"/r/.trash/a/clip.mov":                 "",
		"/r/.trash/a/" + id + ".mov":           "", // no "<stem>." before the uuid
	} {
		got, ok := nodeUUIDFromTrashName(in)
		assert.Equal(t, want, got, in)
		assert.Equal(t, want != "", ok, in)
	}
}
