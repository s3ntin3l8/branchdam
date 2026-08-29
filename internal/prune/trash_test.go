package prune

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func TestPurgeTrash_Basic(t *testing.T) {
	locRoot := t.TempDir()
	trashDir := filepath.Join(locRoot, ".trash", "2026", "08")
	require.NoError(t, os.MkdirAll(trashDir, 0o755))

	oldFile := filepath.Join(trashDir, "old.jpg")
	newFile := filepath.Join(trashDir, "new.jpg")

	require.NoError(t, os.WriteFile(oldFile, []byte("old content"), 0o644))
	require.NoError(t, os.WriteFile(newFile, []byte("new content"), 0o644))

	now := time.Now().UTC()
	oldTime := now.Add(-35 * 24 * time.Hour)
	newTime := now.Add(-5 * 24 * time.Hour)

	require.NoError(t, os.Chtimes(oldFile, oldTime, oldTime))
	require.NoError(t, os.Chtimes(newFile, newTime, newTime))

	res, err := PurgeTrash(context.Background(), locRoot, 30, now)
	require.NoError(t, err)
	assert.Equal(t, 1, res.FilesPurged)
	assert.Equal(t, int64(len("old content")), res.BytesFreed)
	assert.Empty(t, res.Errors)

	// old.jpg should be removed
	_, err = os.Stat(oldFile)
	assert.True(t, os.IsNotExist(err), "old file should be unlinked")

	// new.jpg should still exist
	_, err = os.Stat(newFile)
	assert.NoError(t, err, "new file should be kept")
}

func TestPurgeTrash_DisabledWhenZeroOrNegative(t *testing.T) {
	locRoot := t.TempDir()
	trashDir := filepath.Join(locRoot, ".trash")
	require.NoError(t, os.MkdirAll(trashDir, 0o755))

	oldFile := filepath.Join(trashDir, "old.jpg")
	require.NoError(t, os.WriteFile(oldFile, []byte("content"), 0o644))

	now := time.Now().UTC()
	oldTime := now.Add(-60 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(oldFile, oldTime, oldTime))

	// RetentionDays = 0 (disabled)
	res, err := PurgeTrash(context.Background(), locRoot, 0, now)
	require.NoError(t, err)
	assert.Equal(t, 0, res.FilesPurged)
	_, err = os.Stat(oldFile)
	assert.NoError(t, err, "file should not be purged when retentionDays is 0")

	// RetentionDays = -1
	res, err = PurgeTrash(context.Background(), locRoot, -1, now)
	require.NoError(t, err)
	assert.Equal(t, 0, res.FilesPurged)
	_, err = os.Stat(oldFile)
	assert.NoError(t, err, "file should not be purged when retentionDays is negative")
}

func TestPurgeTrash_CleansEmptySubdirectories(t *testing.T) {
	locRoot := t.TempDir()
	nestedDir := filepath.Join(locRoot, ".trash", "nested", "deep", "dir")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))

	file := filepath.Join(nestedDir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o644))

	now := time.Now().UTC()
	oldTime := now.Add(-40 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(file, oldTime, oldTime))

	res, err := PurgeTrash(context.Background(), locRoot, 30, now)
	require.NoError(t, err)
	assert.Equal(t, 1, res.FilesPurged)

	// The nested empty directory should have been removed
	_, err = os.Stat(nestedDir)
	assert.True(t, os.IsNotExist(err), "empty nested directories should be removed")
}

func TestPurgeAllTrash_MultipleLocations(t *testing.T) {
	tmpDir := t.TempDir()
	loc1 := filepath.Join(tmpDir, "loc1")
	loc2 := filepath.Join(tmpDir, "loc2")
	require.NoError(t, os.MkdirAll(filepath.Join(loc1, ".trash"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(loc2, ".trash"), 0o755))

	f1 := filepath.Join(loc1, ".trash", "f1.jpg")
	f2 := filepath.Join(loc2, ".trash", "f2.jpg")
	require.NoError(t, os.WriteFile(f1, []byte("data1"), 0o644))
	require.NoError(t, os.WriteFile(f2, []byte("data2"), 0o644))

	now := time.Now().UTC()
	oldTime := now.Add(-45 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(f1, oldTime, oldTime))
	require.NoError(t, os.Chtimes(f2, oldTime, oldTime))

	guard := storage.NewGuard([]storage.Location{
		{ID: 1, Name: "Loc1", RootPath: loc1, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: false},
		{ID: 2, Name: "Loc2", RootPath: loc2, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false},
	})

	res, err := PurgeAllTrash(context.Background(), guard, 30, now)
	require.NoError(t, err)
	assert.Equal(t, 2, res.FilesPurged)
	assert.Equal(t, int64(10), res.BytesFreed)

	// Verify worker
	worker := NewTrashWorker(guard, func() int { return 30 }, nil)
	workerRes, err := worker.PurgeOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, workerRes.FilesPurged, "already purged files should not be re-purged")
}
