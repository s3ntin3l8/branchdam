package pipeline

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// TestRebaseIfMovedBackfillsMetadata backs #86: a node indexed before
// exiftool/ffprobe were on PATH, then moved before its removal event was
// processed, must gain its metadata on rebaseIfMoved's own rebase --
// RebaseMissingNodePath alone (unlike insertNewNode) never called
// persistMetadata before this fix.
func TestRebaseIfMovedBackfillsMetadata(t *testing.T) {
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
	loc := storage.Location{ID: locationID, Name: "rebase-metadata-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	// Seed the "old" node directly, with no probe data -- simulating a scan
	// before exiftool/ffprobe were on PATH.
	if _, err := Commit(ctx, database, locationID, []Result{
		{Path: oldPath, FileName: "old.txt", FileExt: "txt", Size: 15, ModTime: time.Now(), FastHash: "abababababababab"},
	}); err != nil {
		t.Fatalf("Commit (seed): %v", err)
	}
	original := mustGetLiveNode(t, database, oldPath)
	if rows, err := database.Reader.ListNodeMetadata(ctx, original.ID); err != nil {
		t.Fatalf("ListNodeMetadata (initial): %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("initial metadata rows = %d, want 0 (simulating a probe-less first scan)", len(rows))
	}

	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}

	newPath := filepath.Join(resolvedRoot, "new.txt")
	writeFile(t, newPath, "renamed content")
	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("stat new: %v", err)
	}
	// Same fast_hash as the seeded node, now with probe data -- the tools
	// were installed in between, or this is the first pass to reach this
	// file after they were.
	result := &Result{
		Path: newPath, FileName: "new.txt", FileExt: "txt", Size: info.Size(), ModTime: info.ModTime(),
		FastHash: "abababababababab",
		Make:     "CANON", ExifRaw: map[string]string{"EXIF:ISO": "100"},
	}

	moved, err := NewWatcherSupervisor(deps, nil).rebaseIfMoved(ctx, loc, result)
	if err != nil {
		t.Fatalf("rebaseIfMoved: %v", err)
	}
	if !moved {
		t.Fatal("rebaseIfMoved = false, want true (old file gone, same fast_hash)")
	}

	rows, err := database.Reader.ListNodeMetadata(ctx, original.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata (after rebase): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("metadata rows after rebase = %d, want 2 (EXIF:Make, EXIF:ISO)", len(rows))
	}

	// #105: rebaseIfMoved's own backfill uses reconcileAllMetadata, same as
	// Commit's touched branch. A second call can't re-exercise rebaseIfMoved
	// itself (the node is now live at newPath, so its own pre-check declines
	// -- see rebaseIfMoved's doc comment), but a Commit pass over the
	// now-rebased node with identical metadata takes the touched branch and
	// exercises the exact reconcileAllMetadata call rebaseIfMoved made, with
	// Stats available to observe it. It must write nothing and leave values
	// unchanged.
	stats, err := Commit(ctx, database, locationID, []Result{*result})
	if err != nil {
		t.Fatalf("Commit (touched, after rebase): %v", err)
	}
	if stats.Touched != 1 {
		t.Fatalf("stats = %+v, want Touched=1", stats)
	}
	if stats.MetadataWritten != 0 {
		t.Fatalf("stats = %+v, want MetadataWritten=0 (identical metadata, nothing changed)", stats)
	}
	if rows, err := database.Reader.ListNodeMetadata(ctx, original.ID); err != nil {
		t.Fatalf("ListNodeMetadata (after touched no-op): %v", err)
	} else if len(rows) != 2 {
		t.Fatalf("metadata rows after touched no-op = %d, want 2 (unchanged)", len(rows))
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

// TestRebaseIfMovedDeclinesFullHashMismatch pins the full_hash guard -- the
// property that makes rebaseIfMoved strictly safer than Commit's scan-path
// move detection (which matches fast_hash alone). With a stored full_hash set
// and a result carrying a DIFFERENT full_hash, the move must be declined even
// though the old file is gone from disk and the fast_hash matches: the
// content can't be verified as identical, so a fast_hash-only rebase could
// steal a genuinely different file.
func TestRebaseIfMovedDeclinesFullHashMismatch(t *testing.T) {
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
	deps.FullHashPolicy = "always" // so the seeded node stores a full_hash
	loc := storage.Location{ID: locationID, Name: "rebase-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("seed scan state = %q (last_error=%v)", job.State, job.LastError)
	}
	original := mustGetLiveNode(t, database, oldPath)
	if original.FullHash == nil {
		t.Fatal("precondition broken: seeded node has no stored full_hash (FullHashPolicy=always should set one)")
	}

	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}

	// Hand-crafted result: the SAME fast_hash as the stored node (so the move
	// would otherwise match) but a DIFFERENT full_hash -- the guard must
	// decline rather than rebase a file whose content it can't verify.
	result := &Result{
		Path:     filepath.Join(resolvedRoot, "new.txt"),
		FileName: "new.txt",
		FileExt:  "txt",
		Size:     16,
		ModTime:  time.Now(),
		FastHash: *original.FastHash,
		FullHash: strings.Repeat("0", 64),
	}

	moved, err := NewWatcherSupervisor(deps, nil).rebaseIfMoved(ctx, loc, result)
	if err != nil {
		t.Fatalf("rebaseIfMoved: %v", err)
	}
	if moved {
		t.Fatal("rebaseIfMoved = true, want false (stored full_hash != result full_hash)")
	}

	// Node untouched, still ACTIVE at its original path with its full_hash.
	got := mustGetLiveNode(t, database, oldPath)
	if got.ID != original.ID || got.LifecycleState != "ACTIVE" {
		t.Errorf("old node changed: id=%d state=%q, want id=%d ACTIVE", got.ID, got.LifecycleState, original.ID)
	}
	if got.FullHash == nil || *got.FullHash != *original.FullHash {
		t.Errorf("old node full_hash changed: %v -> %v", original.FullHash, got.FullHash)
	}
}

// TestRebaseIfMovedFullHashMatchRebases is the full_hash guard's pass side:
// when the stored node's full_hash EQUALS the new file's (content verified
// beyond the 64-bit fast_hash), the guard must not decline -- the move
// rebases in place exactly as in the never-policy case.
func TestRebaseIfMovedFullHashMatchRebases(t *testing.T) {
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
	deps.FullHashPolicy = "always" // so the stored node AND the processed result both carry a full_hash
	loc := storage.Location{ID: locationID, Name: "rebase-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("seed scan state = %q (last_error=%v)", job.State, job.LastError)
	}
	original := mustGetLiveNode(t, database, oldPath)
	if original.FullHash == nil {
		t.Fatal("precondition broken: seeded node has no stored full_hash (FullHashPolicy=always should set one)")
	}

	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}

	newPath := filepath.Join(resolvedRoot, "new.txt")
	writeFile(t, newPath, "renamed content") // identical content: same full_hash as stored
	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("stat new: %v", err)
	}
	result, err := processFile(ctx, deps, loc, indexer.Record{Path: newPath, Size: info.Size(), ModTime: info.ModTime()})
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if result.FullHash == "" {
		t.Fatal("precondition broken: processFile produced no full_hash under FullHashPolicy=always")
	}
	if *original.FullHash != result.FullHash {
		t.Fatalf("precondition broken: stored full_hash %q != processed full_hash %q", *original.FullHash, result.FullHash)
	}

	moved, err := NewWatcherSupervisor(deps, nil).rebaseIfMoved(ctx, loc, result)
	if err != nil {
		t.Fatalf("rebaseIfMoved: %v", err)
	}
	if !moved {
		t.Fatal("rebaseIfMoved = false, want true (stored full_hash == result full_hash)")
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
	if _, err := database.Reader.GetLiveNodeByPath(ctx, oldPath); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("old path still resolves after rebase (err=%v)", err)
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

// TestConsumeOneAbandonsWhenContextAlreadyDone backs #87 (per hermes
// review): a fast shutdown drain must never run a backlogged item through
// the real handleWatchItem pipeline once ctx is already done -- only
// abandon it. target is a file that WOULD be committed successfully if
// consumeOne fell through to handleWatchItem despite the cancelled
// context, so the absence of a node at that path is a genuine
// discriminating signal, not just an absence of an error.
func TestConsumeOneAbandonsWhenContextAlreadyDone(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	target := filepath.Join(resolvedRoot, "would-be-processed.txt")
	if err := writeFileToDisk(target, "content"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := watchTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "consume-one-abandon-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}
	w := NewWatcherSupervisor(deps, nil)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	item := watchItem{rec: indexer.Record{Path: target, Size: info.Size(), ModTime: info.ModTime()}}

	var seen, hashed, failed atomic.Int32
	bumped, abandoned := w.consumeOne(cancelledCtx, loc, item, &seen, &hashed, &failed)

	if !abandoned {
		t.Fatal("abandoned = false, want true (ctx already done)")
	}
	if bumped {
		t.Error("bumped = true, want false for an abandoned item")
	}
	// failed stays 0, not 1: an abandoned item was never attempted, so
	// counting it as a failure would make files_failed>0 on every ordinary
	// shutdown that catches a backlog -- indistinguishable from a real
	// processing failure to an operator or alert.
	if seen.Load() != 1 || failed.Load() != 0 || hashed.Load() != 0 {
		t.Errorf("counts = seen:%d failed:%d hashed:%d, want 1,0,0", seen.Load(), failed.Load(), hashed.Load())
	}
	if _, err := database.Reader.GetLiveNodeByPath(context.Background(), target); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a node was committed for an abandoned item (err=%v) -- consumeOne ran handleWatchItem despite the cancelled context", err)
	}
}

// TestConsumeOneProcessesNormallyWhenContextLive is the positive
// counterpart: with a live context, consumeOne must delegate to
// handleWatchItem exactly as before, not abandon.
func TestConsumeOneProcessesNormallyWhenContextLive(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	target := filepath.Join(resolvedRoot, "processed.txt")
	if err := writeFileToDisk(target, "content"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := watchTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "consume-one-live-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}
	w := NewWatcherSupervisor(deps, nil)

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	item := watchItem{rec: indexer.Record{Path: target, Size: info.Size(), ModTime: info.ModTime()}}

	var seen, hashed, failed atomic.Int32
	bumped, abandoned := w.consumeOne(context.Background(), loc, item, &seen, &hashed, &failed)

	if abandoned {
		t.Fatal("abandoned = true, want false (ctx is live)")
	}
	if !bumped {
		t.Error("bumped = false, want true for a successfully committed item")
	}
	if hashed.Load() != 1 || failed.Load() != 0 {
		t.Errorf("counts = hashed:%d failed:%d, want 1,0", hashed.Load(), failed.Load())
	}
	if _, err := database.Reader.GetLiveNodeByPath(context.Background(), target); err != nil {
		t.Errorf("GetLiveNodeByPath: %v (item should have been committed normally)", err)
	}
}

// TestWatchWorkEnqueueNeverBlocks backs #87: a burst far larger than
// watchQueueCapacity must not make enqueue block or park the caller's
// goroutine -- the actual bug being fixed (every fired debounce timer held
// its own goroutine parked on watchWork's mutex for the full duration of a
// blocking channel send). Calling enqueue this many times directly from the
// test's own goroutine, rather than spawning one per call, proves the
// property without needing runtime.NumGoroutine(): if enqueue blocked even
// once, this loop would hang and the test would time out.
func TestWatchWorkEnqueueNeverBlocks(t *testing.T) {
	work := newWatchWork(nil, "/test/location")
	const burst = watchQueueCapacity * 5
	done := make(chan struct{})
	go func() {
		for i := 0; i < burst; i++ {
			work.enqueue(watchItem{rec: indexer.Record{Path: fmt.Sprintf("/burst/%d", i)}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue blocked under a burst far exceeding watchQueueCapacity")
	}
}

// TestWatchWorkDropsOldestUnderPressure backs #87's chosen backpressure
// policy: once the backlog reaches watchQueueCapacity, a further enqueue
// evicts the OLDEST queued item (not the newest, not an unbounded grow),
// logs the eviction visibly, and counts it in droppedCount.
func TestWatchWorkDropsOldestUnderPressure(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	work := newWatchWork(log, "/test/location")

	const burst = watchQueueCapacity * 3
	for i := 0; i < burst; i++ {
		work.enqueue(watchItem{rec: indexer.Record{Path: fmt.Sprintf("/burst/%d", i)}})
	}

	backlog := work.backlogLen()
	work.mu.Lock()
	first := work.items[0].rec.Path
	last := work.items[len(work.items)-1].rec.Path
	work.mu.Unlock()

	if backlog != watchQueueCapacity {
		t.Errorf("backlog = %d, want exactly %d (bounded, not growing with burst size)", backlog, watchQueueCapacity)
	}
	wantDropped := int64(burst - watchQueueCapacity)
	if got := work.droppedCount(); got != wantDropped {
		t.Errorf("droppedCount() = %d, want %d", got, wantDropped)
	}
	// Drop-oldest, not drop-newest: the surviving backlog must be exactly
	// the last watchQueueCapacity items enqueued, in order.
	if wantFirst := fmt.Sprintf("/burst/%d", burst-watchQueueCapacity); first != wantFirst {
		t.Errorf("oldest surviving item = %q, want %q", first, wantFirst)
	}
	if wantLast := fmt.Sprintf("/burst/%d", burst-1); last != wantLast {
		t.Errorf("newest item = %q, want %q", last, wantLast)
	}
	if !strings.Contains(buf.String(), "dropping oldest") {
		t.Errorf("log output missing a visible drop notice: %q", buf.String())
	}
}

// TestWatchWorkDequeueDrainsThenCloses proves dequeue's contract directly:
// every enqueued item is eventually returned (nothing lost below capacity),
// and dequeue only reports ok=false once the queue is both closed and
// fully drained -- never while items remain, and never blocking forever
// after close() with nothing left.
func TestWatchWorkDequeueDrainsThenCloses(t *testing.T) {
	work := newWatchWork(nil, "/test/location")
	for i := 0; i < 5; i++ {
		work.enqueue(watchItem{rec: indexer.Record{Path: fmt.Sprintf("/item/%d", i)}})
	}

	var got []string
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			item, ok := work.dequeue()
			if !ok {
				close(done)
				return
			}
			got = append(got, item.rec.Path)
		}
	}()

	// Close after a moment, once the 5 items are likely already queued --
	// dequeue must still drain them all before reporting closed.
	time.Sleep(20 * time.Millisecond)
	work.close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dequeue never reported closed after close()")
	}
	wg.Wait()

	if len(got) != 5 {
		t.Fatalf("drained %d items, want 5 (nothing lost below capacity)", len(got))
	}
	for i, path := range got {
		if want := fmt.Sprintf("/item/%d", i); path != want {
			t.Errorf("item %d = %q, want %q", i, path, want)
		}
	}
}

// TestWatchWorkDequeueWakesFromBlockedWait exercises the subtlest part of
// this queue's design, which TestWatchWorkDequeueDrainsThenCloses doesn't
// reach: the consumer must already be parked in dequeue's blocking
// <-q.notify wait -- not merely started before enqueue happens to run --
// or this is only proving the "items already queued" path, not the actual
// wakeup. waitFor polls a "the consumer is now waiting" signal (set right
// before the blocking receive) to guarantee the enqueue below genuinely
// races a parked receiver, not a coincidentally-fast one.
func TestWatchWorkDequeueWakesFromBlockedWait(t *testing.T) {
	work := newWatchWork(nil, "/test/location")

	var waiting atomic.Bool
	got := make(chan string, 1)
	go func() {
		waiting.Store(true)
		item, ok := work.dequeue()
		if !ok {
			return
		}
		got <- item.rec.Path
	}()

	waitFor(t, 2*time.Second, func() bool { return waiting.Load() })
	// waiting only proves the goroutine reached dequeue, not that it's
	// actually parked in <-q.notify yet -- give it a moment to get there.
	time.Sleep(20 * time.Millisecond)

	work.enqueue(watchItem{rec: indexer.Record{Path: "/woken/item"}})

	select {
	case path := <-got:
		if path != "/woken/item" {
			t.Errorf("dequeued %q, want /woken/item", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dequeue never woke from its blocked wait after enqueue -- missed wakeup")
	}
}

// TestWatchWorkEnqueueRaceWithClose is the regression guard for a real
// panic an independent hermes review reproduced against an earlier version
// of this fix: enqueue's non-blocking notify send happened AFTER releasing
// q.mu, so a concurrent close() could close q.notify in the gap between
// enqueue's unlock and its send -- a send racing a channel close panics
// ("send on closed channel"), taking the whole process down (a panic in
// any goroutine is fatal). This is reachable in production any time
// indexer.Watch's debounce timers are still firing (they are not joined
// before watchLocation calls work.close()) right as shutdown begins.
//
// The fix serializes the notify send (in enqueue) and the notify close (in
// close()) through the same q.mu, so they can never interleave -- see the
// watchWork type doc comment. This test doesn't assert an outcome beyond
// "no panic, no hang": running many concurrent enqueue and close attempts
// under -race is what would have caught the original bug (the race
// detector flags the racing chansend/closechan access, and the run panics
// without the fix), and a clean, race-free run under -race across many
// iterations is the regression guard against it recurring.
func TestWatchWorkEnqueueRaceWithClose(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		work := newWatchWork(nil, "/test/location")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				work.enqueue(watchItem{rec: indexer.Record{Path: fmt.Sprintf("/race/%d", i)}})
			}
		}()
		go func() {
			defer wg.Done()
			work.close()
		}()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: enqueue/close did not both return within 5s (deadlock?)", iter)
		}
	}
}
