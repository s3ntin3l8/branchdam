package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
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

// TestWaitWithinReturnsTrueWhenWaitCompletes is the trivial case: wait
// returns well inside ctx's deadline.
func TestWaitWithinReturnsTrueWhenWaitCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !waitWithin(ctx, func() {}) {
		t.Fatal("waitWithin = false, want true (wait returned immediately)")
	}
}

// TestWaitWithinReturnsFalseOnTimeout backs #98's headline bug: a wait that
// never returns must not hang waitWithin's caller forever -- ctx's deadline
// must win.
func TestWaitWithinReturnsFalseOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // unblock the leaked goroutine so it doesn't outlive the test

	start := time.Now()
	got := waitWithin(ctx, func() { <-block })
	if got {
		t.Fatal("waitWithin = true, want false (wait never returns)")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waitWithin took %s to return false, want well under ctx's 50ms deadline plus scheduling slack", elapsed)
	}
}

// TestShutdownSequenceTerminatesWithinBudgetDespiteSlowJob is #98's
// acceptance criterion: a deliberately slow in-flight job (standing in for
// a large-file full hash or a stalled network mount) must not delay
// shutdown past the budget, and must be reported via dbUnsafeToClose rather
// than silently hanging or silently proceeding.
func TestShutdownSequenceTerminatesWithinBudgetDespiteSlowJob(t *testing.T) {
	poolCtx, cancelPool := context.WithCancel(context.Background())
	pool := workers.New[string](1, 4)
	pool.Run(poolCtx)

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		cancelPool()
		close(release) // let the stuck worker goroutine finally exit -- it can't observe cancelPool while blocked inside Run
	})

	if !pool.Submit(poolCtx, workers.Job[string]{
		Key: "slow",
		Run: func(context.Context) error {
			close(started)
			<-release // simulates a hash/probe call that doesn't respect ctx cancellation quickly
			return nil
		},
	}) {
		t.Fatal("Submit for the slow job returned false")
	}
	<-started // the job is genuinely running, so pool.Drain() below is guaranteed to block on it

	httpServer := &http.Server{} // never ListenAndServe'd -- Shutdown returns immediately
	scanTracker := &pipeline.ScanTracker{}
	const budget = 200 * time.Millisecond
	shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var dbUnsafeToClose bool
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- runShutdownSequence(shutdownCtx, slog.New(slog.DiscardHandler), httpServer, nil, scanTracker, pool, &dbUnsafeToClose)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runShutdownSequence returned err = %v, want nil (httpServer was never served)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runShutdownSequence did not return within 2s of a 200ms budget -- shutdown is hanging on the slow job")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("runShutdownSequence took %s, want close to the %s budget", elapsed, budget)
	}
	if !dbUnsafeToClose {
		t.Error("dbUnsafeToClose = false, want true -- the pool never actually finished draining within budget")
	}
}

// TestShutdownSkipsDBCloseWhenAWaitTimesOut is the invariant #98 is really
// about: on a timed-out wait, the database must stay open rather than
// racing whatever goroutine might still be writing to it. closeDatabase is
// run()'s single close call site -- this drives it directly against a real
// *db.DB.
func TestShutdownSkipsDBCloseWhenAWaitTimesOut(t *testing.T) {
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
