package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within", timeout)
}

func watchTestDeps(t *testing.T, database *db.DB, rootPath string, locationID int64) ScanDeps {
	t.Helper()
	d := scanTestDeps(t, database, rootPath, locationID)
	return d
}

func TestWatchSupervisorCreateThenMissingThenRebase(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := watchTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "watch-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	sctx, scancel := context.WithCancel(context.Background())
	super := NewWatcherSupervisor(deps, nil)
	super.Start(sctx, []storage.Location{loc}, 50*time.Millisecond)
	defer super.Wait()
	defer scancel()

	// Give the watcher a moment to install its fsnotify watch before the
	// first write, or the CREATE event could be missed entirely.
	time.Sleep(100 * time.Millisecond)

	first := filepath.Join(resolvedRoot, "made.txt")
	if err := writeFileToDisk(first, "watched content"); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		n, err := database.Reader.GetLiveNodeByPath(ctx, first)
		return err == nil && n.LifecycleState == "ACTIVE"
	})
	original := mustGetLiveNode(t, database, first)

	osRemove(t, first)
	waitFor(t, 5*time.Second, func() bool {
		n, err := database.Reader.GetLiveNodeByPath(ctx, first)
		return err == nil && n.LifecycleState == "MISSING"
	})

	moved := filepath.Join(resolvedRoot, "elsewhere")
	if err := os.MkdirAll(moved, 0o755); err != nil {
		t.Fatalf("mkdir moved: %v", err)
	}
	// Give addRecursive time to pick up and watch the new directory before a
	// file is created inside it, or that CREATE is lost (same race the
	// indexer package's TestWatchTracksNewSubdirectory documents).
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(moved, "made.txt"), []byte("watched content"), 0o644); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	newPath := filepath.Join(moved, "made.txt")
	waitFor(t, 5*time.Second, func() bool {
		n, err := database.Reader.GetLiveNodeByPath(ctx, newPath)
		return err == nil && n.LifecycleState == "ACTIVE" && n.ID == original.ID
	})
	rebase := mustGetLiveNode(t, database, newPath)
	if rebase.NodeUuid != original.NodeUuid {
		t.Errorf("node_uuid changed over a move: %q -> %q", original.NodeUuid, rebase.NodeUuid)
	}
}

// TestWatchSupervisorRenamePreservesNodeUUID covers the case that motivated
// the serialized per-location consumer: an os.Rename emits a Remove(old) and
// a Create(new) whose processing order must be deterministic, or the moved
// file gets a brand-new node_uuid instead of a rebase of the old node.
func TestWatchSupervisorRenamePreservesNodeUUID(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := watchTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "rename-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	sctx, scancel := context.WithCancel(context.Background())
	super := NewWatcherSupervisor(deps, nil)
	super.Start(sctx, []storage.Location{loc}, 50*time.Millisecond)
	defer super.Wait()
	defer scancel()

	// Give the watcher a moment to install its fsnotify watch before the
	// first write, or the CREATE event could be missed entirely.
	time.Sleep(100 * time.Millisecond)

	before := filepath.Join(resolvedRoot, "before.txt")
	if err := writeFileToDisk(before, "renamed content"); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		n, err := database.Reader.GetLiveNodeByPath(ctx, before)
		return err == nil && n.LifecycleState == "ACTIVE"
	})
	original := mustGetLiveNode(t, database, before)

	after := filepath.Join(resolvedRoot, "after.txt")
	if err := os.Rename(before, after); err != nil {
		t.Fatalf("rename: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		n, err := database.Reader.GetLiveNodeByPath(ctx, after)
		return err == nil && n.LifecycleState == "ACTIVE" && n.ID == original.ID
	})
	moved := mustGetLiveNode(t, database, after)
	if moved.NodeUuid != original.NodeUuid {
		t.Errorf("node_uuid changed over a rename: %q -> %q", original.NodeUuid, moved.NodeUuid)
	}
	if oldNode, err := database.Reader.GetLiveNodeByPath(ctx, before); err == nil && oldNode.LifecycleState == "ACTIVE" {
		t.Errorf("old path still ACTIVE after rename: %s", before)
	}
}

func TestWatchJobFailedOnDeath(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	resolvedGone, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	// location row must exist (FK), but the root path itself does not.
	rootForRow := filepath.Join(resolvedGone, "nope")
	if err := os.MkdirAll(filepath.Dir(rootForRow), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	_ = os.Remove(rootForRow) // ensure it does not exist for the watcher
	locationID := seedPipelineLocation(t, database, rootForRow)
	deps := watchTestDeps(t, database, rootForRow, locationID)
	loc := storage.Location{ID: locationID, Name: "dead", RootPath: rootForRow, Tier: "TIER2_EXPORTS", ReadOnly: false}

	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	super := NewWatcherSupervisor(deps, nil)
	super.Start(sctx, []storage.Location{loc}, 20*time.Millisecond)
	defer super.Wait()

	waitFor(t, 5*time.Second, func() bool {
		rows, err := database.Reader.ListRecentScanJobs(ctx, 10)
		if err != nil || len(rows) == 0 {
			return false
		}
		for _, j := range rows {
			if j.Kind == "WATCH" && j.State == "FAILED" && j.LastError.Valid && j.LastError.String != "" {
				return true
			}
		}
		return false
	})
}

func TestWatchJobCancelledOnShutdown(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := watchTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "cancel-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	sctx, scancel := context.WithCancel(context.Background())
	super := NewWatcherSupervisor(deps, nil)
	super.Start(sctx, []storage.Location{loc}, 20*time.Millisecond)

	// Wait for the job row to appear, then cancel + drain.
	waitFor(t, 5*time.Second, func() bool {
		rows, err := database.Reader.ListRecentScanJobs(ctx, 10)
		return err == nil && len(rows) > 0 && rows[0].Kind == "WATCH" && rows[0].State == "RUNNING"
	})
	scancel()
	super.Wait()

	rows, err := database.Reader.ListRecentScanJobs(ctx, 10)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ListRecentScanJobs: err=%v rows=%d", err, len(rows))
	}
	if rows[0].State != "CANCELLED" {
		t.Errorf("watch job state = %q, want CANCELLED", rows[0].State)
	}
}

func writeFileToDisk(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), 0o644)
}

func osRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}
