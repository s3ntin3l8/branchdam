-- name: CreateScanJob :one
INSERT INTO scan_jobs (storage_location_id, kind, state, started_at, updated_at)
VALUES (?1, ?2, 'RUNNING', unixepoch(), unixepoch())
RETURNING id, storage_location_id, kind, state, files_seen, files_hashed,
          files_failed, edges_created, started_at, finished_at, last_error, updated_at;

-- name: UpdateScanJobProgress :exec
UPDATE scan_jobs
SET files_seen = ?2, files_hashed = ?3, files_failed = ?4, edges_created = ?5, updated_at = unixepoch()
WHERE id = ?1;

-- name: CompleteScanJob :exec
UPDATE scan_jobs SET state = 'COMPLETED', finished_at = unixepoch(), updated_at = unixepoch() WHERE id = ?1;

-- name: FailScanJob :exec
UPDATE scan_jobs SET state = 'FAILED', last_error = ?2, finished_at = unixepoch(), updated_at = unixepoch() WHERE id = ?1;

-- name: CancelScanJob :exec
-- Phase 1 (#32): a WATCH job torn down by a clean shutdown ends CANCELLED,
-- not FAILED -- only a watcher that died on its own is a failure.
UPDATE scan_jobs SET state = 'CANCELLED', finished_at = unixepoch(), updated_at = unixepoch() WHERE id = ?1;

-- name: ReconcileOrphanedScanJobs :execrows
-- Every row still 'RUNNING' at process startup, before this process has
-- created any scan_jobs row of its own, was left behind by a previous
-- process that never reached a terminal state -- SIGKILL, OOM-kill,
-- container hard-stop, power loss. A WATCH row is RUNNING for its entire
-- process lifetime by design, so this is the only place its state ever
-- gets cleaned up after a crash. Reuses FAILED rather than adding a new
-- enum state (see issue #88's scope note): last_error distinguishes this
-- from a genuine processing failure. Must run before
-- WatcherSupervisor.Start creates any fresh WATCH row for the same
-- location, or a reconciled row and a fresh row could momentarily both
-- claim to represent "the" watch state for it. ix_scan_jobs_active
-- (state, started_at DESC) backs this WHERE clause.
UPDATE scan_jobs
SET state = 'FAILED', last_error = ?1, finished_at = unixepoch(), updated_at = unixepoch()
WHERE state = 'RUNNING';

-- name: GetScanJob :one
SELECT id, storage_location_id, kind, state, files_seen, files_hashed,
       files_failed, edges_created, started_at, finished_at, last_error, updated_at
FROM scan_jobs
WHERE id = ?1;

-- name: ListRecentScanJobs :many
SELECT id, storage_location_id, kind, state, files_seen, files_hashed,
       files_failed, edges_created, started_at, finished_at, last_error, updated_at
FROM scan_jobs
ORDER BY started_at DESC
LIMIT ?1;

-- name: CountRunningScanJobs :one
SELECT COUNT(*) FROM scan_jobs WHERE state = 'RUNNING' AND kind != 'WATCH';

-- name: ListScanJobsFiltered :many
SELECT id, storage_location_id, kind, state, files_seen, files_hashed,
       files_failed, edges_created, started_at, finished_at, last_error, updated_at
FROM scan_jobs
WHERE (sqlc.narg('kind') IS NULL OR kind = sqlc.narg('kind'))
  AND (sqlc.narg('state') IS NULL OR state = sqlc.narg('state'))
ORDER BY started_at DESC
LIMIT ?1 OFFSET ?2;

-- name: CountScanJobsFiltered :one
SELECT COUNT(*)
FROM scan_jobs
WHERE (sqlc.narg('kind') IS NULL OR kind = sqlc.narg('kind'))
  AND (sqlc.narg('state') IS NULL OR state = sqlc.narg('state'));
