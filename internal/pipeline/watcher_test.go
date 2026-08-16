package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/indexer"
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

// TestRebaseIfMovedCreateFirstRebases is the direct unit test for the
// kernel-Create-first ordering rebaseIfMoved exists to handle: inotify on
// Linux delivers a rename's IN_MOVED_TO (create) before its IN_MOVED_FROM
// (removal), so the old node is still ACTIVE -- not MISSING -- when the new
// file is processed. rebaseIfMoved must recognize the move from the
// filesystem (old file gone from disk, same fast_hash) and rebase the old
// node in place, rather than letting a duplicate node be inserted.
func TestRebaseIfMovedCreateFirstRebases(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	oldPath := filepath.Join(root, "old.txt")
	writeFile(t, oldPath, "renamed content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "rebase-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("seed scan state = %q (last_error=%v)", job.State, job.LastError)
	}
	original := mustGetLiveNode(t, database, oldPath)
	if original.LifecycleState != "ACTIVE" {
		t.Fatalf("precondition broken: old node = %q, want ACTIVE", original.LifecycleState)
	}

	// The Create-first precondition: the old file is gone from disk, but the
	// node is still ACTIVE -- its removal event has not been processed yet.
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}

	newPath := filepath.Join(resolvedRoot, "new.txt")
	writeFile(t, newPath, "renamed content") // the moved file: identical content
	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("stat new: %v", err)
	}
	result, err := processFile(ctx, deps, loc, indexer.Record{Path: newPath, Size: info.Size(), ModTime: info.ModTime()})
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if original.FastHash == nil || *original.FastHash != result.FastHash {
		t.Fatalf("precondition broken: old fast_hash %v != new fast_hash %q", original.FastHash, result.FastHash)
	}

	moved, err := NewWatcherSupervisor(deps, nil).rebaseIfMoved(ctx, loc, result)
	if err != nil {
		t.Fatalf("rebaseIfMoved: %v", err)
	}
	if !moved {
		t.Fatal("rebaseIfMoved = false, want true (old file gone, same fast_hash)")
	}

	rebased := mustGetLiveNode(t, database, newPath)
	if rebased.ID != original.ID {
		t.Errorf("rebased id = %d, want %d (same row, no fresh insert)", rebased.ID, original.ID)
	}
	if rebased.NodeUuid != original.NodeUuid {
		t.Errorf("rebased node_uuid = %q, want %q", rebased.NodeUuid, original.NodeUuid)
	}
	if rebased.LifecycleState != "ACTIVE" {
		t.Errorf("rebased lifecycle_state = %q, want ACTIVE", rebased.LifecycleState)
	}

	// The old path must no longer resolve to a live node -- the row was
	// rebound to newPath, not left behind.
	if _, err := database.Reader.GetLiveNodeByPath(ctx, oldPath); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("old path still resolves after rebase (err=%v)", err)
	}

	// And the move inserted nothing: exactly one live node carries this
	// fast_hash now.
	nodes, err := database.Reader.ListLiveNodesByFastHash(ctx, &result.FastHash)
	if err != nil {
		t.Fatalf("ListLiveNodesByFastHash: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("live nodes with fast_hash = %d, want 1 (no duplicate inserted)", len(nodes))
	}
}

// TestRebaseIfMovedLeavesDuplicateAlone is the negative counterpart: the old
// file is still on disk, so a same-content file at a new path is a genuine
// duplicate -- not a move -- and rebaseIfMoved must decline (returning
// false), leaving the old node untouched for Commit's normal duplicate /
// collision handling.
func TestRebaseIfMovedLeavesDuplicateAlone(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	oldPath := filepath.Join(root, "old.txt")
	writeFile(t, oldPath, "renamed content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "rebase-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("seed scan state = %q (last_error=%v)", job.State, job.LastError)
	}
	original := mustGetLiveNode(t, database, oldPath)

	// Old file deliberately still present: identical content at a new path is
	// a duplicate, not a move -- rebaseIfMoved must leave it alone.
	newPath := filepath.Join(resolvedRoot, "dup.txt")
	writeFile(t, newPath, "renamed content")
	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("stat new: %v", err)
	}
	result, err := processFile(ctx, deps, loc, indexer.Record{Path: newPath, Size: info.Size(), ModTime: info.ModTime()})
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}

	moved, err := NewWatcherSupervisor(deps, nil).rebaseIfMoved(ctx, loc, result)
	if err != nil {
		t.Fatalf("rebaseIfMoved: %v", err)
	}
	if moved {
		t.Fatal("rebaseIfMoved = true, want false (old file still on disk)")
	}

	// Old node untouched, still ACTIVE at its path.
	if got := mustGetLiveNode(t, database, oldPath); got.ID != original.ID || got.LifecycleState != "ACTIVE" {
		t.Errorf("old node changed: id=%d state=%q, want id=%d ACTIVE", got.ID, got.LifecycleState, original.ID)
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
