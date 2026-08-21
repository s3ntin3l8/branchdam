package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

func TestWatchedFromConfig(t *testing.T) {
	cfg := []config.StorageLocation{
		{Name: "scratch", RootPath: "/s", Tier: "TIER1_LOCAL_SCRATCH", Watch: true},
		{Name: "archive", RootPath: "/a", Tier: "TIER3_MASTER_ARCHIVE", Watch: true}, // never watched
		{Name: "projects", RootPath: "/p", Tier: "PROJECTS", Watch: false},           // not opted in
	}
	got := watchedFromConfig(cfg)
	if len(got) != 1 || got[0].Name != "scratch" {
		t.Fatalf("watchedFromConfig = %+v, want only scratch", got)
	}
}

func TestResolveWatchedLocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	cfg := config.StorageLocation{Name: "scratch", RootPath: resolved, Tier: "TIER1_LOCAL_SCRATCH", Watch: true}
	// seedStorageLocations-equivalent upsert
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "scratch", RootPath: resolved, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	locs, err := resolveWatchedLocations(context.Background(), database, []config.StorageLocation{cfg})
	if err != nil {
		t.Fatalf("resolveWatchedLocations: %v", err)
	}
	if len(locs) != 1 || locs[0].Tier != "TIER1_LOCAL_SCRATCH" || locs[0].ID == 0 {
		t.Fatalf("locs = %+v, want one TIER1_LOCAL_SCRATCH with a real id", locs)
	}
}

// TestSeedStorageLocationsDeactivatesRemovedLocations backs M6: a location
// removed from config.yaml (not merely failing to resolve -- storage.
// LoadGuard handles that case) must self-heal storage-health/UI state to
// inactive without a manual DB edit.
func TestSeedStorageLocationsDeactivatesRemovedLocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed-deactivate.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	both := []config.StorageLocation{
		{Name: "keep", RootPath: "/keep", Tier: "TIER1_LOCAL_SCRATCH"},
		{Name: "drop", RootPath: "/drop", Tier: "TIER1_LOCAL_SCRATCH"},
	}
	if err := seedStorageLocations(ctx, database, both); err != nil {
		t.Fatalf("seed both: %v", err)
	}

	keepLoc, err := database.Reader.GetStorageLocationByPath(ctx, "/keep")
	if err != nil {
		t.Fatalf("get keep location: %v", err)
	}
	dropLoc, err := database.Reader.GetStorageLocationByPath(ctx, "/drop")
	if err != nil {
		t.Fatalf("get drop location: %v", err)
	}
	if keepLoc.IsActive != 1 || dropLoc.IsActive != 1 {
		t.Fatalf("both locations should start active: keep=%d drop=%d", keepLoc.IsActive, dropLoc.IsActive)
	}

	// Re-seed with only "keep" configured -- "drop" was removed from
	// config.yaml.
	if err := seedStorageLocations(ctx, database, []config.StorageLocation{both[0]}); err != nil {
		t.Fatalf("re-seed with drop removed: %v", err)
	}

	keepAfter, err := database.Reader.GetStorageLocationByPath(ctx, "/keep")
	if err != nil {
		t.Fatalf("get keep location after re-seed: %v", err)
	}
	dropAfter, err := database.Reader.GetStorageLocationByPath(ctx, "/drop")
	if err != nil {
		t.Fatalf("get drop location after re-seed: %v", err)
	}
	if keepAfter.IsActive != 1 {
		t.Errorf("keep.IsActive = %d, want 1 (still configured)", keepAfter.IsActive)
	}
	if dropAfter.IsActive != 0 {
		t.Errorf("drop.IsActive = %d, want 0 (removed from config)", dropAfter.IsActive)
	}
}

// TestSeedStorageLocationsReactivatesReturningLocation is the self-heal in
// the other direction: a location previously deactivated (e.g. by
// storage.LoadGuard after a mount vanished) must become active again once
// it's seeded from config successfully -- UpsertStorageLocation's ON
// CONFLICT branch unconditionally resets is_active to 1.
func TestSeedStorageLocationsReactivatesReturningLocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed-reactivate.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	cfg := []config.StorageLocation{{Name: "returning", RootPath: "/returning", Tier: "TIER1_LOCAL_SCRATCH"}}
	if err := seedStorageLocations(ctx, database, cfg); err != nil {
		t.Fatalf("initial seed: %v", err)
	}
	loc, err := database.Reader.GetStorageLocationByPath(ctx, "/returning")
	if err != nil {
		t.Fatalf("get location: %v", err)
	}
	if err := deactivateStorageLocations(ctx, database, []int64{loc.ID}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	deactivated, err := database.Reader.GetStorageLocationByID(ctx, loc.ID)
	if err != nil {
		t.Fatalf("get after deactivate: %v", err)
	}
	if deactivated.IsActive != 0 {
		t.Fatalf("IsActive after deactivate = %d, want 0", deactivated.IsActive)
	}

	if err := seedStorageLocations(ctx, database, cfg); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	reactivated, err := database.Reader.GetStorageLocationByID(ctx, loc.ID)
	if err != nil {
		t.Fatalf("get after re-seed: %v", err)
	}
	if reactivated.IsActive != 1 {
		t.Errorf("IsActive after re-seed = %d, want 1 (self-healed)", reactivated.IsActive)
	}
}

// TestDeactivateStorageLocations is the direct unit test for the helper
// storage.LoadGuard's skippedLocationIDs feed into.
func TestDeactivateStorageLocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deactivate.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	if err := seedStorageLocations(ctx, database, []config.StorageLocation{
		{Name: "a", RootPath: "/a", Tier: "TIER1_LOCAL_SCRATCH"},
		{Name: "b", RootPath: "/b", Tier: "TIER1_LOCAL_SCRATCH"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	locA, err := database.Reader.GetStorageLocationByPath(ctx, "/a")
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	locB, err := database.Reader.GetStorageLocationByPath(ctx, "/b")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}

	if err := deactivateStorageLocations(ctx, database, []int64{locA.ID}); err != nil {
		t.Fatalf("deactivateStorageLocations: %v", err)
	}

	aAfter, err := database.Reader.GetStorageLocationByID(ctx, locA.ID)
	if err != nil {
		t.Fatalf("get a after: %v", err)
	}
	bAfter, err := database.Reader.GetStorageLocationByID(ctx, locB.ID)
	if err != nil {
		t.Fatalf("get b after: %v", err)
	}
	if aAfter.IsActive != 0 {
		t.Errorf("a.IsActive = %d, want 0", aAfter.IsActive)
	}
	if bAfter.IsActive != 1 {
		t.Errorf("b.IsActive = %d, want 1 (not in the deactivate list)", bAfter.IsActive)
	}
}

// TestReconcileOrphanedScanJobs backs #88: a RUNNING row left behind by a
// killed prior process (SIGKILL, OOM-kill, crash) must move to a terminal
// state on the next startup, before it can be confused with genuinely
// active work -- while a job that already reached a terminal state on its
// own must be left untouched.
func TestReconcileOrphanedScanJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconcile.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	const sentinelFailedError = "sentinel: a genuine processing failure, not a reconciliation"

	var orphanedID, completedID, failedID, cancelledID int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		orphaned, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{Kind: "WATCH"})
		if err != nil {
			return err
		}
		orphanedID = orphaned.ID

		completed, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{Kind: "FULL_SCAN"})
		if err != nil {
			return err
		}
		completedID = completed.ID
		if err := q.CompleteScanJob(ctx, completedID); err != nil {
			return err
		}

		// A pre-existing FAILED row with its own distinct last_error is
		// what actually discriminates an overly-broad WHERE clause (e.g.
		// WHERE state != 'COMPLETED') from the correct WHERE state =
		// 'RUNNING' -- a finished_at-unchanged assertion alone wouldn't
		// catch that, since both FailScanJob and ReconcileOrphanedScanJobs
		// set finished_at to unixepoch() and could land on the same
		// second in a fast test run.
		failed, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{Kind: "FULL_SCAN"})
		if err != nil {
			return err
		}
		failedID = failed.ID
		if err := q.FailScanJob(ctx, sqlcgen.FailScanJobParams{
			ID: failedID, LastError: sql.NullString{String: sentinelFailedError, Valid: true},
		}); err != nil {
			return err
		}

		cancelled, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{Kind: "WATCH"})
		if err != nil {
			return err
		}
		cancelledID = cancelled.ID
		return q.CancelScanJob(ctx, cancelledID)
	}); err != nil {
		t.Fatalf("seed scan jobs: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	n, err := reconcileOrphanedScanJobs(ctx, database, log)
	if err != nil {
		t.Fatalf("reconcileOrphanedScanJobs: %v", err)
	}
	if n != 1 {
		t.Errorf("reconcileOrphanedScanJobs returned %d, want 1 (only the orphaned RUNNING row)", n)
	}

	orphaned, err := database.Reader.GetScanJob(ctx, orphanedID)
	if err != nil {
		t.Fatalf("GetScanJob(orphaned): %v", err)
	}
	if orphaned.State != "FAILED" {
		t.Errorf("orphaned job state = %q, want FAILED", orphaned.State)
	}
	if !orphaned.LastError.Valid || orphaned.LastError.String == "" {
		t.Errorf("orphaned job last_error = %v, want a non-empty explanation", orphaned.LastError)
	}
	if !orphaned.FinishedAt.Valid {
		t.Error("orphaned job finished_at is still null after reconciliation")
	}

	completed, err := database.Reader.GetScanJob(ctx, completedID)
	if err != nil {
		t.Fatalf("GetScanJob(completed): %v", err)
	}
	if completed.State != "COMPLETED" {
		t.Errorf("already-completed job state = %q, want COMPLETED (must not be touched)", completed.State)
	}
	if completed.LastError.Valid {
		t.Errorf("already-completed job last_error = %v, want still null", completed.LastError)
	}

	failedJob, err := database.Reader.GetScanJob(ctx, failedID)
	if err != nil {
		t.Fatalf("GetScanJob(failed): %v", err)
	}
	if failedJob.State != "FAILED" {
		t.Errorf("already-failed job state = %q, want still FAILED", failedJob.State)
	}
	if !failedJob.LastError.Valid || failedJob.LastError.String != sentinelFailedError {
		t.Errorf("already-failed job last_error = %v, want unchanged sentinel %q -- a genuine failure must not be overwritten with a reconciliation message", failedJob.LastError, sentinelFailedError)
	}

	cancelledJob, err := database.Reader.GetScanJob(ctx, cancelledID)
	if err != nil {
		t.Fatalf("GetScanJob(cancelled): %v", err)
	}
	if cancelledJob.State != "CANCELLED" {
		t.Errorf("already-cancelled job state = %q, want still CANCELLED", cancelledJob.State)
	}

	// Idempotent: running it again with nothing left RUNNING must be a
	// no-op, not an error, and report zero reconciled.
	n2, err := reconcileOrphanedScanJobs(ctx, database, log)
	if err != nil {
		t.Fatalf("reconcileOrphanedScanJobs (second call, nothing to reconcile): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second call returned %d, want 0 (nothing left RUNNING)", n2)
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"info": slog.LevelInfo, "debug": slog.LevelDebug,
		"warn": slog.LevelWarn, "error": slog.LevelError,
		"INFO": slog.LevelInfo, "bogus": slog.LevelInfo, "": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLogLevel(in); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLogLevelDebugOverridesConfig(t *testing.T) {
	if got := logLevel("error", true); got != slog.LevelDebug {
		t.Errorf("logLevel(error, debug=true) = %v, want Debug", got)
	}
	if got := logLevel("debug", false); got != slog.LevelDebug {
		t.Errorf("logLevel(debug, debug=false) = %v, want Debug", got)
	}
	if got := logLevel("info", false); got != slog.LevelInfo {
		t.Errorf("logLevel(info, debug=false) = %v, want Info", got)
	}
}

func TestWaitBounded(t *testing.T) {
	t.Run("completes before context deadline", func(t *testing.T) {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(buf, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		completed := false
		ok := waitBounded(ctx, log, "quickComponent", func() {
			completed = true
		})

		if !completed {
			t.Errorf("expected waitBounded function to complete")
		}
		if !ok {
			t.Error("waitBounded returned false, want true (the func returned well within the deadline)")
		}
		if bytes.Contains(buf.Bytes(), []byte("shutdown wait timed out")) {
			t.Errorf("unexpected timeout warning in logs")
		}
	})

	t.Run("times out and logs warning when wait function hangs", func(t *testing.T) {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(buf, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		slowDone := make(chan struct{})
		defer close(slowDone)

		start := time.Now()
		ok := waitBounded(ctx, log, "slowComponent", func() {
			<-slowDone
		})
		elapsed := time.Since(start)

		if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
			t.Errorf("waitBounded did not respect context deadline: took %v", elapsed)
		}
		if ok {
			t.Error("waitBounded returned true, want false (the func never returned)")
		}
		if !bytes.Contains(buf.Bytes(), []byte("shutdown wait timed out")) {
			t.Errorf("expected timeout log warning, got: %s", buf.String())
		}
		if !bytes.Contains(buf.Bytes(), []byte("slowComponent")) {
			t.Errorf("expected component name in log output, got: %s", buf.String())
		}
	})

	// Backs the shutdown sequence's own join-phase context: httpServer.
	// Shutdown's realistic failure mode is its OWN context's deadline
	// firing, and reusing that same already-expired context for the joins
	// below it would give every waitBounded call effectively zero real
	// time before giving up -- this pins waitBounded's actual behavior on
	// an already-expired context (returns false near-instantly, still
	// running waitFunc in its own goroutine even though the caller never
	// waits for it) so that invariant can't silently regress if the join
	// phase's fresh context is ever accidentally swapped back for the
	// outer shutdownCtx.
	t.Run("returns false immediately when context is already expired", func(t *testing.T) {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(buf, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond) // guarantee ctx is expired before waitBounded runs

		slowDone := make(chan struct{})
		defer close(slowDone)

		start := time.Now()
		ok := waitBounded(ctx, log, "alreadyExpired", func() {
			<-slowDone
		})
		elapsed := time.Since(start)

		if ok {
			t.Error("waitBounded returned true against an already-expired context, want false")
		}
		if elapsed > 100*time.Millisecond {
			t.Errorf("waitBounded took %v against an already-expired context, want near-instant", elapsed)
		}
	})
}

// TestCloseDatabaseSkipsCloseWhenUnsafe is the invariant this fix is really
// about: when a shutdown wait times out, a background goroutine may still
// hold the writer connection, so the database must stay open rather than
// racing it -- closeDatabase is main()'s single database.Close() call site.
func TestCloseDatabaseSkipsCloseWhenUnsafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close-skip.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	log := slog.New(slog.DiscardHandler)

	closeDatabase(log, database, true) // unsafe=true: must NOT close
	if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error { return nil }); err != nil {
		t.Fatalf("database still unusable after a skipped close: %v", err)
	}

	closeDatabase(log, database, false) // unsafe=false: must close
	if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error { return nil }); err == nil {
		t.Fatal("database.InTx succeeded after closeDatabase(unsafe=false) -- want the database actually closed")
	}
}

func TestStartImmichWorkerDisabledWhenNotConfigured(t *testing.T) {
	for name, apiURL := range map[string]string{
		"empty":                  "",
		"unresolved placeholder": "${IMMICH_API_URL}", // unset env left literal by expandEnv
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			database := mainTestOpenDB(t)
			w := startImmichWorker(ctx, &config.Config{Immich: config.Immich{APIURL: apiURL}}, database, slog.New(slog.DiscardHandler))
			if w != nil {
				t.Fatalf("startImmichWorker with apiUrl %q = %v, want nil", apiURL, w)
			}
		})
	}
}

func TestStartImmichWorkerDisabledWhenLibraryIDMissing(t *testing.T) {
	for name, libraryID := range map[string]string{
		"empty":                  "",
		"unresolved placeholder": "${IMMICH_LIBRARY_ID}",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			database := mainTestOpenDB(t)
			cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: libraryID, ExportPath: "/e"}}
			w := startImmichWorker(ctx, cfg, database, slog.New(slog.DiscardHandler))
			if w != nil {
				t.Fatalf("startImmichWorker with libraryId %q = %v, want nil (an empty libraryId would 404 every push forever, see #182)", libraryID, w)
			}
		})
	}
}

func TestStartImmichWorkerEnabledWhenConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database := mainTestOpenDB(t)
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	w := startImmichWorker(ctx, cfg, database, slog.New(slog.DiscardHandler))
	if w == nil {
		t.Fatal("startImmichWorker with APIURL set = nil, want a worker")
	}
}

func TestStartThumbWorkerDisabledWhenNotEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database := mainTestOpenDB(t)
	cfg := &config.Config{Thumbnails: config.Thumbnails{Enabled: false, CacheDir: t.TempDir()}}
	w := startThumbWorker(ctx, cfg, database, storage.NewGuard(nil), probe.New(), slog.New(slog.DiscardHandler), sse.New())
	if w != nil {
		t.Fatal("startThumbWorker with thumbnails.enabled=false = non-nil, want nil")
	}
}

func TestStartThumbWorkerEnabledWhenConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	database := mainTestOpenDB(t)
	cfg := &config.Config{Thumbnails: config.Thumbnails{Enabled: true, CacheDir: filepath.Join(t.TempDir(), "thumbs")}}
	w := startThumbWorker(ctx, cfg, database, storage.NewGuard(nil), probe.New(), slog.New(slog.DiscardHandler), sse.New())
	if w == nil {
		t.Fatal("startThumbWorker with thumbnails.enabled=true = nil, want a worker")
	}
	// Wait() blocks until ctx is cancelled (Start's background loop only
	// returns on <-ctx.Done()) -- cancel explicitly and confirm it returns
	// promptly, rather than deferring cancel() past this call and deadlocking.
	cancel()
	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5s of ctx cancellation")
	}
}

func TestStartThumbWorkerDisabledWhenCacheDirUncreatable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database := mainTestOpenDB(t)
	// A regular file where the cache dir should be: MkdirAll fails on it.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	cfg := &config.Config{Thumbnails: config.Thumbnails{Enabled: true, CacheDir: filepath.Join(blocker, "thumbs")}}
	w := startThumbWorker(ctx, cfg, database, storage.NewGuard(nil), probe.New(), slog.New(slog.DiscardHandler), sse.New())
	if w != nil {
		t.Fatal("startThumbWorker with an uncreatable cacheDir = non-nil, want nil (fail loudly, don't run a worker that can never write)")
	}
}

func TestValidatePruneConfig(t *testing.T) {
	t.Run("cacheTtlHours without prunable is rejected", func(t *testing.T) {
		cfg := []config.StorageLocation{
			{Name: "scratch", RootPath: "/s", Tier: "TIER1_LOCAL_SCRATCH", Prunable: false, CacheTTLHours: 24},
		}
		if err := validatePruneConfig(cfg); err == nil {
			t.Fatal("validatePruneConfig = nil, want an error")
		}
	})
	t.Run("cacheTtlHours with prunable is accepted", func(t *testing.T) {
		cfg := []config.StorageLocation{
			{Name: "scratch", RootPath: "/s", Tier: "TIER1_LOCAL_SCRATCH", Prunable: true, CacheTTLHours: 24},
		}
		if err := validatePruneConfig(cfg); err != nil {
			t.Errorf("validatePruneConfig = %v, want nil", err)
		}
	})
	t.Run("prunable without cacheTtlHours is accepted (never eligible, not an error)", func(t *testing.T) {
		cfg := []config.StorageLocation{
			{Name: "scratch", RootPath: "/s", Tier: "TIER1_LOCAL_SCRATCH", Prunable: true},
		}
		if err := validatePruneConfig(cfg); err != nil {
			t.Errorf("validatePruneConfig = %v, want nil", err)
		}
	})
	t.Run("negative cacheTtlHours is rejected even when prunable", func(t *testing.T) {
		cfg := []config.StorageLocation{
			{Name: "scratch", RootPath: "/s", Tier: "TIER1_LOCAL_SCRATCH", Prunable: true, CacheTTLHours: -1},
		}
		if err := validatePruneConfig(cfg); err == nil {
			t.Fatal("validatePruneConfig = nil, want an error (negative cacheTtlHours would otherwise silently no-op forever)")
		}
	})
}

func TestSweptFromConfig(t *testing.T) {
	cfg := []config.StorageLocation{
		{Name: "nas", RootPath: "/n", Tier: "TIER2_EXPORTS", Sweep: true},
		{Name: "archive", RootPath: "/a", Tier: "TIER3_MASTER_ARCHIVE", Sweep: true}, // never swept
		{Name: "projects", RootPath: "/p", Tier: "PROJECTS", Sweep: false},           // not opted in
	}
	got := sweptFromConfig(cfg)
	if len(got) != 1 || got[0].Name != "nas" {
		t.Fatalf("sweptFromConfig = %+v, want only nas", got)
	}
}

func TestResolveSweptLocations(t *testing.T) {
	database := mainTestOpenDB(t)
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	cfg := config.StorageLocation{Name: "nas", RootPath: resolved, Tier: "TIER2_EXPORTS", Sweep: true, SweepIntervalSecs: 42}
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "nas", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	locs, err := resolveSweptLocations(context.Background(), database, []config.StorageLocation{cfg})
	if err != nil {
		t.Fatalf("resolveSweptLocations: %v", err)
	}
	if len(locs) != 1 || locs[0].Location.Tier != "TIER2_EXPORTS" || locs[0].Location.ID == 0 {
		t.Fatalf("locs = %+v, want one TIER2_EXPORTS with a real id", locs)
	}
	if locs[0].Interval != 42*time.Second {
		t.Errorf("locs[0].Interval = %v, want 42s", locs[0].Interval)
	}
}

func TestResolveSweptLocationsZeroIntervalMeansDefault(t *testing.T) {
	database := mainTestOpenDB(t)
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	cfg := config.StorageLocation{Name: "nas", RootPath: resolved, Tier: "TIER2_EXPORTS", Sweep: true} // SweepIntervalSecs unset
	if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "nas", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	locs, err := resolveSweptLocations(context.Background(), database, []config.StorageLocation{cfg})
	if err != nil {
		t.Fatalf("resolveSweptLocations: %v", err)
	}
	if len(locs) != 1 || locs[0].Interval != 0 {
		t.Fatalf("locs = %+v, want Interval=0 (SweeperSupervisor.Start resolves 0 to its own default)", locs)
	}
}

// TestWarnOverlappingWatchAndSweep proves a location with both watch and
// sweep enabled logs a WARN naming that location, rather than being
// silently accepted or rejected as a config error -- #60's AC #3 is not
// literally true (the worker pool's per-key dedup never sees watcher work,
// since the watcher calls Commit directly), so this is the honest
// alternative: surface the combination, don't pretend it's unreachable.
func TestWarnOverlappingWatchAndSweep(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))

	watched := []config.StorageLocation{
		{Name: "both", RootPath: "/both", Tier: "TIER2_EXPORTS", Watch: true, Sweep: true},
		{Name: "watch-only", RootPath: "/w", Tier: "TIER2_EXPORTS", Watch: true},
	}
	swept := []config.StorageLocation{
		{Name: "both", RootPath: "/both", Tier: "TIER2_EXPORTS", Watch: true, Sweep: true},
		{Name: "sweep-only", RootPath: "/s", Tier: "TIER2_EXPORTS", Sweep: true},
	}

	warnOverlappingWatchAndSweep(log, watched, swept)

	out := buf.String()
	if !strings.Contains(out, "rootPath=/both") {
		t.Errorf("log output = %q, want a WARN naming /both", out)
	}
	if strings.Contains(out, "rootPath=/w") || strings.Contains(out, "rootPath=/s") {
		t.Errorf("log output = %q, want no warning for locations with only one of watch/sweep", out)
	}
}

// TestRunReturnsErrorOnBindFailure backs #123: a genuine ListenAndServe
// failure (e.g. the configured listenAddr is already in use) must surface
// through run()'s return value instead of falling through the same
// shutdown path a normal SIGTERM takes and returning nil -- which
// previously made the process exit 0 even though it never started serving.
// Occupying the address with our own listener first, rather than a fixed
// port number, makes the bind conflict deterministic without risking a
// collision with something else on the test machine.
func TestRunReturnsErrorOnBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	httpServer := &http.Server{Addr: occupied.Addr().String()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.DiscardHandler)
	database := mainTestOpenDB(t)
	scanTracker := &pipeline.ScanTracker{}
	drainer := agent.NewDrainer(database, nil, log)
	pool := workers.New[string](1, 1)
	pool.Run(ctx)

	var dbUnsafeToClose bool
	runErr := run(ctx, cancel, log, httpServer, nil, nil, scanTracker, nil, drainer, nil, pool, &dbUnsafeToClose)

	if runErr == nil {
		t.Fatal("run() = nil, want a non-nil error surfacing the bind failure")
	}
	if !errors.Is(runErr, syscall.EADDRINUSE) {
		t.Errorf("run() err = %v, want it to wrap syscall.EADDRINUSE", runErr)
	}
}

func mainTestOpenDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
