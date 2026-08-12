-- name: CreateScanJob :one
INSERT INTO scan_jobs (storage_location_id, kind, state, started_at, updated_at)
VALUES (?1, ?2, 'RUNNING', unixepoch(), unixepoch())
RETURNING id, storage_location_id, kind, state, files_seen, files_hashed,
          files_failed, edges_created, started_at, finished_at, last_error, updated_at;

-- name: UpdateScanJobProgress :exec
UPDATE scan_jobs
SET files_seen = ?2, files_hashed = ?3, files_failed = ?4, updated_at = unixepoch()
WHERE id = ?1;

-- name: CompleteScanJob :exec
UPDATE scan_jobs SET state = 'COMPLETED', finished_at = unixepoch(), updated_at = unixepoch() WHERE id = ?1;

-- name: FailScanJob :exec
UPDATE scan_jobs SET state = 'FAILED', last_error = ?2, finished_at = unixepoch(), updated_at = unixepoch() WHERE id = ?1;

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
