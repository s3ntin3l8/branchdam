package main

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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
