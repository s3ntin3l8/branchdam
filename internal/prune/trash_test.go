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

// trashGuard returns a Guard with locRoot as a single writable location.
func trashGuard(t *testing.T, locRoot string) *storage.Guard {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(locRoot)
	require.NoError(t, err)
	return storage.NewGuard([]storage.Location{{ID: 1, Name: "loc", RootPath: resolved, Tier: "TIER1_LOCAL_SCRATCH"}})
}

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

	res, err := PurgeTrash(context.Background(), trashGuard(t, locRoot), locRoot, 30, now)
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
	res, err := PurgeTrash(context.Background(), trashGuard(t, locRoot), locRoot, 0, now)
	require.NoError(t, err)
	assert.Equal(t, 0, res.FilesPurged)
	_, err = os.Stat(oldFile)
	assert.NoError(t, err, "file should not be purged when retentionDays is 0")

	// RetentionDays = -1
	res, err = PurgeTrash(context.Background(), trashGuard(t, locRoot), locRoot, -1, now)
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

	res, err := PurgeTrash(context.Background(), trashGuard(t, locRoot), locRoot, 30, now)
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

// A read-only location's .trash is never purged, even when physically
// writable; a writable one is.
func TestPurgeAllTrash_SkipsReadOnlyLocation(t *testing.T) {
	tmpDir := t.TempDir()
	ro := filepath.Join(tmpDir, "ro")
	rw := filepath.Join(tmpDir, "rw")
	require.NoError(t, os.MkdirAll(filepath.Join(ro, ".trash"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(rw, ".trash"), 0o755))
	roFile := filepath.Join(ro, ".trash", "keep.jpg")
	rwFile := filepath.Join(rw, ".trash", "purge.jpg")
	now := time.Now().UTC()
	old := now.Add(-45 * 24 * time.Hour)
	for _, f := range []string{roFile, rwFile} {
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
		require.NoError(t, os.Chtimes(f, old, old))
	}
	guard := storage.NewGuard([]storage.Location{
		{ID: 1, Name: "ro", RootPath: ro, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
		{ID: 2, Name: "rw", RootPath: rw, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: false},
	})

	res, err := PurgeAllTrash(context.Background(), guard, 30, now)
	require.NoError(t, err)
	assert.Equal(t, 1, res.FilesPurged)
	assert.Empty(t, res.Errors)
	_, err = os.Stat(roFile)
	assert.NoError(t, err, "read-only location's trash must be left alone")
	_, err = os.Stat(rwFile)
	assert.True(t, os.IsNotExist(err))

	// Calling PurgeTrash directly on the read-only root is refused by the
	// Guard per file rather than unlinking.
	res, err = PurgeTrash(context.Background(), guard, ro, 30, now)
	require.NoError(t, err)
	assert.Equal(t, 0, res.FilesPurged)
	assert.NotEmpty(t, res.Errors)
	_, err = os.Stat(roFile)
	assert.NoError(t, err)
}
