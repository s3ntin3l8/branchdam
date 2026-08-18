package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// TestSweepUnchangedFilesAreTouchedNotHashed is #60's headline behavior,
// covering both directions in ONE test rather than two so a fail-closed
// differential check (one that wrongly skips a genuinely changed file)
// can't hide behind a separate "unchanged -> filesHashed==0" assertion:
// three unchanged files plus one modified file, swept once after an
// initial full scan establishes the baseline.
func TestSweepUnchangedFilesAreTouchedNotHashed(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	writeFile(t, filepath.Join(root, "b.txt"), "bravo content")
	writeFile(t, filepath.Join(root, "c.txt"), "charlie content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}

	before := map[string]sqlcgen.MediaNode{}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		before[name] = mustGetLiveNode(t, database, filepath.Join(resolvedRoot, name))
	}

	waitForNextSecond(t)

	if err := os.WriteFile(filepath.Join(resolvedRoot, "b.txt"), []byte("bravo content, edited"), 0o644); err != nil {
		t.Fatalf("rewrite b.txt: %v", err)
	}

	jobID2, err := RunSweep(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunSweep (pass 2): %v", err)
	}
	job2 := waitJobDone(t, database, jobID2)
	if job2.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job2.State, job2.LastError)
	}
	if job2.Kind != "INCREMENTAL" {
		t.Errorf("pass 2 kind = %q, want INCREMENTAL", job2.Kind)
	}
	if job2.FilesSeen != 3 {
		t.Errorf("pass 2 files_seen = %d, want 3", job2.FilesSeen)
	}
	if job2.FilesHashed != 1 {
		t.Errorf("pass 2 files_hashed = %d, want exactly 1 (only b.txt changed)", job2.FilesHashed)
	}

	for _, name := range []string{"a.txt", "c.txt"} {
		path := filepath.Join(resolvedRoot, name)
		after := mustGetLiveNode(t, database, path)
		want := before[name]
		if after.ID != want.ID {
			t.Errorf("%s: id changed from %d to %d, want unchanged (must not have been routed through Commit)", name, want.ID, after.ID)
		}
		if after.FastHash == nil || want.FastHash == nil || *after.FastHash != *want.FastHash {
			t.Errorf("%s: fast_hash changed, want unchanged", name)
		}
		if after.LastSeenAt <= want.LastSeenAt {
			t.Errorf("%s: last_seen_at did not advance (got %d, was %d)", name, after.LastSeenAt, want.LastSeenAt)
		}
		if after.UpdatedAt <= want.UpdatedAt {
			t.Errorf("%s: updated_at did not advance (got %d, was %d)", name, after.UpdatedAt, want.UpdatedAt)
		}
	}

	bPath := filepath.Join(resolvedRoot, "b.txt")
	bAfter := mustGetLiveNode(t, database, bPath)
	bBefore := before["b.txt"]
	if bAfter.ID == bBefore.ID {
		t.Errorf("b.txt: id unchanged (%d), want a version collision (new id) since content changed", bAfter.ID)
	}
	if bAfter.FastHash == nil || bBefore.FastHash == nil || *bAfter.FastHash == *bBefore.FastHash {
		t.Error("b.txt: fast_hash unchanged, want it to differ since content changed")
	}
}

// TestSweepSizeChangeRehashesEvenWithSameMtime guards against a differential
// check that only compares mtime: a file whose size changed but whose mtime
// (by construction here) did not must still be re-hashed. Both fields must
// match for a file to be skipped.
func TestSweepSizeChangeRehashesEvenWithSameMtime(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, "alpha")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}
	resolvedPath := filepath.Join(resolvedRoot, "a.txt")

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}
	before := mustGetLiveNode(t, database, resolvedPath)

	waitForNextSecond(t)

	// Overwrite with different content but pin mtime back to the original
	// value -- an mtime-only differential check would wrongly call this
	// unchanged.
	origMtime := time.Unix(before.MtimeUnix, 0)
	if err := os.WriteFile(resolvedPath, []byte("alpha content, much longer now"), 0o644); err != nil {
		t.Fatalf("rewrite a.txt: %v", err)
	}
	if err := os.Chtimes(resolvedPath, origMtime, origMtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	jobID2, err := RunSweep(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunSweep (pass 2): %v", err)
	}
	job2 := waitJobDone(t, database, jobID2)
	if job2.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job2.State, job2.LastError)
	}
	if job2.FilesHashed != 1 {
		t.Errorf("pass 2 files_hashed = %d, want 1 (size changed, must be re-hashed despite matching mtime)", job2.FilesHashed)
	}
	after := mustGetLiveNode(t, database, resolvedPath)
	if after.ID == before.ID {
		t.Error("a.txt: id unchanged, want a version collision since size/content changed")
	}
}

// TestSweepProgressCounterNeverRegresses guards the #140/fa5e452 bug class:
// UpdateScanJobProgress SETs an absolute value, so the sweep's touch path
// (which never increments filesHashed) must not cause a later flush to
// write a smaller files_hashed than an earlier flush already reported.
// Forces multiple drainAndCommit flushes by using a small batch via many
// files, then asserts the final files_hashed matches the actual number of
// changed files exactly (not more, not less, and never observed to shrink).
func TestSweepProgressCounterNeverRegresses(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	const unchangedCount = 5
	for i := 0; i < unchangedCount; i++ {
		writeFile(t, filepath.Join(root, "u"+string(rune('a'+i))+".txt"), "unchanged content")
	}
	changedPath := filepath.Join(root, "changed.txt")
	writeFile(t, changedPath, "original")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}

	waitForNextSecond(t)
	resolvedChanged := filepath.Join(resolvedRoot, "changed.txt")
	if err := os.WriteFile(resolvedChanged, []byte("edited"), 0o644); err != nil {
		t.Fatalf("rewrite changed.txt: %v", err)
	}

	jobID2, err := RunSweep(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunSweep (pass 2): %v", err)
	}

	// Poll files_hashed throughout the run and assert it is monotonically
	// non-decreasing, then assert its final value.
	var lastHashed int64
	deadline := time.Now().Add(10 * time.Second)
	var finalJob sqlcgen.ScanJob
	for time.Now().Before(deadline) {
		job, err := database.Reader.GetScanJob(ctx, jobID2)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.FilesHashed < lastHashed {
			t.Fatalf("files_hashed regressed: was %d, now %d", lastHashed, job.FilesHashed)
		}
		lastHashed = job.FilesHashed
		finalJob = job
		if job.State != "RUNNING" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if finalJob.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", finalJob.State, finalJob.LastError)
	}
	if finalJob.FilesHashed != 1 {
		t.Errorf("final files_hashed = %d, want 1", finalJob.FilesHashed)
	}
}

// TestTouchBatcherFlushFailureMarksUncertain proves a failed touch-batch
// flush adds every one of its entries' paths to uncertain -- matching
// drainAndCommit's own failed-batch handling -- rather than silently
// dropping them (which would let a live file's stale last_seen_at feed the
// MISSING sweep), and bumps filesFailed the same way a failed Commit batch
// does, so scan_jobs.files_failed doesn't undercount this failure class.
// Forces the failure deterministically by closing the database before
// flushing: sql.DB.Close is idempotent, so the test's own
// t.Cleanup-driven Close afterward is harmless.
func TestTouchBatcherFlushFailureMarksUncertain(t *testing.T) {
	database := openTestDB(t)
	uncertain := newUncertainPaths()
	var filesFailed atomic.Int32
	batch := newTouchBatcher(database, uncertain, &filesFailed, nil)

	ctx := context.Background()
	batch.add(ctx, 1, "/a", 100)
	batch.add(ctx, 2, "/b", 200)

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	batch.flush(ctx)

	if got := filesFailed.Load(); got != 2 {
		t.Errorf("filesFailed = %d, want 2", got)
	}

	got := uncertain.list()
	want := map[string]bool{"/a": true, "/b": true}
	if len(got) != len(want) {
		t.Fatalf("uncertain paths = %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected uncertain path %q", p)
		}
	}
	if batch.total != 0 {
		t.Errorf("touchBatcher.total = %d, want 0 (flush failed, nothing landed)", batch.total)
	}
}

// TestSweepMarksDeletedFileMissing proves the differential sweep reuses the
// exact same clean-completion-gated MISSING sweep a full scan uses (#60's
// stated acceptance criterion), not an approximation of it: a file removed
// between an initial full scan and a differential sweep pass must still be
// marked MISSING. Mirrors TestScanSweepMarksDeletedFileMissing but with
// pass 2 run via RunSweep instead of RunScan.
func TestSweepMarksDeletedFileMissing(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	writeFile(t, filepath.Join(root, "b.txt"), "bravo content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}

	if err := os.Remove(filepath.Join(resolvedRoot, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	waitForNextSecond(t)

	jobID2, err := RunSweep(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunSweep (pass 2): %v", err)
	}
	job2 := waitJobDone(t, database, jobID2)
	if job2.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job2.State, job2.LastError)
	}
	if job2.Kind != "INCREMENTAL" {
		t.Errorf("pass 2 kind = %q, want INCREMENTAL", job2.Kind)
	}

	a := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt"))
	if a.LifecycleState != "MISSING" {
		t.Errorf("a.txt lifecycle_state = %q, want MISSING", a.LifecycleState)
	}
	b := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "b.txt"))
	if b.LifecycleState != "ACTIVE" {
		t.Errorf("b.txt lifecycle_state = %q, want ACTIVE", b.LifecycleState)
	}
}

// TestSweepMissingNodeAlwaysRehashes is the Hermes-flagged correctness
// guard: a MISSING node whose path gets a DIFFERENT file with a
// coincidentally matching mtime+size must never be reactivated by the
// differential fast path on that weak signal alone. sweepUnchanged requires
// LifecycleState=="ACTIVE"; a MISSING node always falls through to the
// ordinary hash+Commit path, which verifies via fast_hash and -- since the
// content genuinely differs here -- archives the stale node and inserts a
// fresh one, rather than silently reattaching the old node's identity
// (full_hash, phash) to unrelated content.
func TestSweepMissingNodeAlwaysRehashes(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	const originalContent = "original-content"
	writeFile(t, path, originalContent)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	resolvedPath := filepath.Join(resolvedRoot, "a.txt")
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}
	original := mustGetLiveNode(t, database, resolvedPath)

	if err := os.Remove(resolvedPath); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	waitForNextSecond(t)

	jobID2, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 2): %v", err)
	}
	if job := waitJobDone(t, database, jobID2); job.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job.State, job.LastError)
	}
	missing := mustGetLiveNode(t, database, resolvedPath)
	if missing.LifecycleState != "MISSING" {
		t.Fatalf("pre-conditions broken: a.txt = %q, want MISSING", missing.LifecycleState)
	}

	waitForNextSecond(t)

	// A DIFFERENT file, same byte length as the original, pinned back to the
	// missing node's stored mtime -- coincidentally matching (mtime, size)
	// without being the same content.
	const differentContent = "different-conten" // same length as originalContent
	if len(differentContent) != len(originalContent) {
		t.Fatalf("test fixture bug: content lengths differ (%d vs %d)", len(differentContent), len(originalContent))
	}
	if err := os.WriteFile(resolvedPath, []byte(differentContent), 0o644); err != nil {
		t.Fatalf("write replacement a.txt: %v", err)
	}
	origMtime := time.Unix(missing.MtimeUnix, 0)
	if err := os.Chtimes(resolvedPath, origMtime, origMtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	jobID3, err := RunSweep(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunSweep (pass 3): %v", err)
	}
	job3 := waitJobDone(t, database, jobID3)
	if job3.State != "COMPLETED" {
		t.Fatalf("pass 3 state = %q (last_error=%v)", job3.State, job3.LastError)
	}
	if job3.FilesHashed != 1 {
		t.Errorf("pass 3 files_hashed = %d, want 1 (a MISSING node must always be re-hashed, never blindly reactivated)", job3.FilesHashed)
	}

	after := mustGetLiveNode(t, database, resolvedPath)
	if after.ID == original.ID {
		t.Error("a.txt: id unchanged, want a fresh node (version collision) since the reappeared content genuinely differs")
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("a.txt lifecycle_state = %q, want ACTIVE", after.LifecycleState)
	}
	if after.FastHash == nil || original.FastHash == nil || *after.FastHash == *original.FastHash {
		t.Error("a.txt: fast_hash unchanged from the original MISSING node, want it to reflect the new content")
	}

	archived, err := database.Reader.GetMediaNodeByID(ctx, original.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID(original): %v", err)
	}
	if archived.LifecycleState != "ARCHIVED" {
		t.Errorf("original node lifecycle_state = %q, want ARCHIVED (must not have been silently reactivated in place)", archived.LifecycleState)
	}
}
