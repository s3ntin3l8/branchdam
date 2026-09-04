package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
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

// TestConcurrentFullScanArchiveDoesNotResurrectViaDifferentialTouch is the
// regression test #226's widened createScanJob guard depends on but doesn't
// itself provide: the guard is deliberately same-kind-only (see its doc
// comment), so a FULL_SCAN and a manually-triggered differential
// INCREMENTAL can now run concurrently against the same location. Most of a
// mostly-unchanged Tier-3 archive's files never reach workers.Pool.Submit's
// per-path dedup at all -- sweepUnchanged routes them straight to
// touchBatcher, which has no dedup/refusal handling of its own. What
// actually makes that combination safe is TouchMediaNode's own
// MISSING-only CASE (internal/db/queries/media_nodes.sql): reproduces, by
// deliberately sequencing the interleaving rather than racing goroutines
// (the SQL-level safety property being tested doesn't depend on timing,
// only on operation order), the case where a concurrent FULL_SCAN's version
// collision archives a node id AFTER the differential sweep's
// sweepUnchanged check already decided to touch it, but BEFORE that
// deferred touchBatcher flush actually runs.
func TestConcurrentFullScanArchiveDoesNotResurrectViaDifferentialTouch(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", true)
	const path = "/archive/master.raw"

	// Baseline: the node a differential sweep's sweepUnchanged check will
	// decide is unchanged and queue for a touch.
	stats, err := Commit(ctx, database, locationID, []Result{
		{Path: path, FileName: "master.raw", FileExt: "raw", Size: 100, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa"},
	})
	if err != nil || stats.Inserted != 1 {
		t.Fatalf("Commit (baseline insert): stats=%+v err=%v", stats, err)
	}
	node1 := mustGetLiveNode(t, database, path)

	// The differential sweep's own machinery: sweepUnchanged already ran
	// (elsewhere, conceptually) and decided node1 is unchanged, queuing a
	// touch for it -- but the batch hasn't flushed yet.
	uncertain := newUncertainPaths()
	var filesFailed atomic.Int32
	batch := newTouchBatcher(database, uncertain, &filesFailed, nil)
	batch.add(ctx, node1.ID, path, node1.MtimeUnix)

	// Before that deferred flush runs, a concurrent FULL_SCAN observes
	// different content at the same path and commits a version collision:
	// archive-then-insert (docs/schema.md fix #3) means node1 is archived
	// and a fresh node2 takes over the live path.
	collisionStats, err := Commit(ctx, database, locationID, []Result{
		{Path: path, FileName: "master.raw", FileExt: "raw", Size: 200, ModTime: time.Now(), FastHash: "bbbbbbbbbbbbbbbb"},
	})
	if err != nil || collisionStats.VersionCollisions != 1 {
		t.Fatalf("Commit (concurrent version collision): stats=%+v err=%v", collisionStats, err)
	}
	archivedBeforeTouch, err := database.Reader.GetMediaNodeByID(ctx, node1.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID(node1) after collision: %v", err)
	}
	if archivedBeforeTouch.LifecycleState != "ARCHIVED" {
		t.Fatalf("node1 lifecycle_state = %q after concurrent collision, want ARCHIVED", archivedBeforeTouch.LifecycleState)
	}

	// Now the differential sweep's deferred touch finally lands, against the
	// id of what is now an ARCHIVED row.
	batch.flush(ctx)
	if batch.total != 1 {
		t.Errorf("touchBatcher.total = %d, want 1 (the touch itself is a valid UPDATE, not a failure)", batch.total)
	}

	// The core assertion: TouchMediaNode's MISSING-only CASE must not have
	// resurrected node1 back to ACTIVE.
	node1AfterTouch, err := database.Reader.GetMediaNodeByID(ctx, node1.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID(node1) after touch: %v", err)
	}
	if node1AfterTouch.LifecycleState != "ARCHIVED" {
		t.Errorf("node1 lifecycle_state = %q after the deferred touch, want still ARCHIVED (must not be resurrected)", node1AfterTouch.LifecycleState)
	}
	if node1AfterTouch.MtimeUnix != archivedBeforeTouch.MtimeUnix {
		t.Errorf("node1 mtime_unix = %d, want unchanged %d", node1AfterTouch.MtimeUnix, archivedBeforeTouch.MtimeUnix)
	}
	if node1AfterTouch.LastSeenAt != archivedBeforeTouch.LastSeenAt {
		t.Errorf("node1 last_seen_at = %d, want unchanged %d", node1AfterTouch.LastSeenAt, archivedBeforeTouch.LastSeenAt)
	}
	if node1AfterTouch.UpdatedAt != archivedBeforeTouch.UpdatedAt {
		t.Errorf("node1 updated_at = %d, want unchanged %d", node1AfterTouch.UpdatedAt, archivedBeforeTouch.UpdatedAt)
	}

	// And no live duplicate exists at the path: the live row must still be
	// node2, the FULL_SCAN's successor, exactly one of it.
	liveNow := mustGetLiveNode(t, database, path)
	if liveNow.ID == node1.ID {
		t.Fatalf("live node at %q is node1 (%d), want the FULL_SCAN's successor node", path, node1.ID)
	}
	if liveNow.LifecycleState != "ACTIVE" {
		t.Errorf("live node lifecycle_state = %q, want ACTIVE", liveNow.LifecycleState)
	}
	if liveNow.FastHash == nil || *liveNow.FastHash != "bbbbbbbbbbbbbbbb" {
		t.Errorf("live node fast_hash = %v, want the collision's fast_hash (bbbb...)", liveNow.FastHash)
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
	// keepalive.txt stays on disk through every pass -- #225's zero-only
	// sweep guard skips MarkUnseenNodesMissing entirely when a pass sees
	// zero files, so a.txt being the ONLY file on disk and getting removed
	// would otherwise never reach MISSING at all. This test is about
	// re-hashing a MISSING node on reappearance, not the zero-files guard
	// itself, so keep filesSeen > 0 throughout.
	writeFile(t, filepath.Join(root, "keepalive.txt"), "keepalive content")
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

// seedTier3Location mirrors seedPipelineLocation but for a
// TIER3_MASTER_ARCHIVE/read-only location -- #226's differential-Tier-3
// scenario needs a location whose full_hash policy actually forces a
// BLAKE3 computation (needsFullHash's tierReadOnly branch), which
// seedPipelineLocation's hardcoded TIER2_EXPORTS/ReadOnly:0 row never does.
func seedTier3Location(t *testing.T, database *db.DB, rootPath string) int64 {
	t.Helper()
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "scan-test-tier3-" + t.Name(), RootPath: rootPath,
			Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		})
		if err != nil {
			return err
		}
		id = loc.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed tier-3 location: %v", err)
	}
	return id
}

// tier3ScanTestDeps builds ScanDeps for a TIER3_MASTER_ARCHIVE location.
// Unlike scanTestDepsN (hardcoded to TIER2_EXPORTS/FullHashPolicy:"never"
// for every other sweep test in this file), this uses the default
// "tier3_and_collision" policy so needsFullHash actually escalates to a
// full BLAKE3 hash on the read-only tier -- the behavior #226's differential
// path must avoid re-running for files sweepUnchanged finds unchanged.
func tier3ScanTestDeps(t *testing.T, database *db.DB, rootPath string, locationID int64) ScanDeps {
	t.Helper()
	pool := workers.New[string](2, 16)
	poolCtx, cancelPool := context.WithCancel(context.Background())
	t.Cleanup(cancelPool)
	pool.Run(poolCtx)
	return ScanDeps{
		DB:     database,
		Guard:  storage.NewGuard([]storage.Location{{ID: locationID, Name: "test-tier3", RootPath: rootPath, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true}}),
		Prober: probe.New(),
		Pool:   pool,
		// FullHashPolicy left unset (zero value ""), so needsFullHash falls
		// through to its documented default -- the same "tier3_and_collision"
		// behavior handleStartScan wires up in production when config leaves
		// workers.fullHashPolicy unset.
	}
}

// TestDifferentialTier3ScanSkipsFullHashForUnchangedFiles is #226's headline
// acceptance criterion: a TIER3_MASTER_ARCHIVE location swept with
// RunSweep (the same call handleStartScan's differential:true branch makes)
// must not reopen or re-BLAKE3 a file whose (mtime, size) still match its
// stored node -- it goes through sweepUnchanged/touchBatcher exactly like
// any other tier's differential pass, never through processFile/Commit.
// Proven two ways: the unchanged node's id and full_hash are byte-identical
// before and after (an id change would mean it took the version-collision
// path; a recomputed-but-coincidentally-equal full_hash can't be ruled out
// by value alone, so the id/FilesHashed checks are what actually establish
// "never re-hashed", not the full_hash comparison by itself), and
// scan_jobs.files_hashed only counts the one file that actually changed.
// The changed file, by contrast, must still get a fresh full_hash --
// existing full-hash-on-collision/change behavior is unaffected.
func TestDifferentialTier3ScanSkipsFullHashForUnchangedFiles(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "master-a.dng"), "master alpha content")
	writeFile(t, filepath.Join(root, "master-b.dng"), "master bravo content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedTier3Location(t, database, resolvedRoot)
	deps := tier3ScanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-tier3", RootPath: resolvedRoot, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1, baseline full scan): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}

	beforeA := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "master-a.dng"))
	beforeB := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "master-b.dng"))
	if beforeA.FullHash == nil || len(*beforeA.FullHash) != 64 {
		t.Fatalf("master-a.dng full_hash not computed on baseline full scan (tier3_and_collision policy should force it): %v", beforeA.FullHash)
	}
	if beforeB.FullHash == nil || len(*beforeB.FullHash) != 64 {
		t.Fatalf("master-b.dng full_hash not computed on baseline full scan: %v", beforeB.FullHash)
	}

	waitForNextSecond(t)
	if err := os.WriteFile(filepath.Join(resolvedRoot, "master-b.dng"), []byte("master bravo content, edited"), 0o644); err != nil {
		t.Fatalf("rewrite master-b.dng: %v", err)
	}

	jobID2, err := RunSweep(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunSweep (pass 2, differential): %v", err)
	}
	job2 := waitJobDone(t, database, jobID2)
	if job2.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job2.State, job2.LastError)
	}
	if job2.Kind != "INCREMENTAL" {
		t.Errorf("pass 2 kind = %q, want INCREMENTAL", job2.Kind)
	}
	if job2.FilesSeen != 2 {
		t.Errorf("pass 2 files_seen = %d, want 2", job2.FilesSeen)
	}
	if job2.FilesHashed != 1 {
		t.Errorf("pass 2 files_hashed = %d, want exactly 1 (only master-b.dng changed; master-a.dng must not be re-hashed)", job2.FilesHashed)
	}

	afterA := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "master-a.dng"))
	if afterA.ID != beforeA.ID {
		t.Errorf("master-a.dng: id changed from %d to %d, want unchanged (must have taken the touch-only path, not Commit)", beforeA.ID, afterA.ID)
	}
	if afterA.FullHash == nil || beforeA.FullHash == nil || *afterA.FullHash != *beforeA.FullHash {
		t.Error("master-a.dng: full_hash changed, want byte-identical (never reopened or re-hashed)")
	}
	if afterA.LastSeenAt <= beforeA.LastSeenAt {
		t.Errorf("master-a.dng: last_seen_at did not advance (got %d, was %d)", afterA.LastSeenAt, beforeA.LastSeenAt)
	}

	afterB := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "master-b.dng"))
	if afterB.ID == beforeB.ID {
		t.Errorf("master-b.dng: id unchanged (%d), want a version collision (new id) since content changed", afterB.ID)
	}
	if afterB.FullHash == nil || len(*afterB.FullHash) != 64 {
		t.Fatalf("master-b.dng full_hash not recomputed after content change: %v", afterB.FullHash)
	}
	if beforeB.FullHash != nil && *afterB.FullHash == *beforeB.FullHash {
		t.Error("master-b.dng: full_hash unchanged despite content change, want a fresh hash")
	}
}
