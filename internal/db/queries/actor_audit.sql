-- actor_audit: append-only event log for admin actions that aren't
-- otherwise audited (scan start, restart, settings PUT, storage location
-- PUT, prune execute, pairing lifecycle). Mirrors companion_pairing_audit's
-- shape but cross-cutting: one table, not per-domain.
--
-- actor_kind + actor_name carry the display identity. actor_user_id
-- (nullable) carries the FK to users when one exists, so admin/audit
-- views can filter "everything by user X" or "everything the system did"
-- without parsing the name string.
--
-- All positional params use bare ?1/?2 (not sqlc.arg) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: InsertActorAudit :exec
INSERT INTO actor_audit (
    actor_user_id, actor_kind, actor_name,
    event, resource_type, resource_id, details_json
) VALUES (
    ?1, ?2, ?3,
    ?4, ?5, ?6, ?7
);

-- name: ListActorAudit :many
-- Admin/audit list view. Offset pagination -- audit volume is bounded
-- (one entry per admin action), so pagination drift isn't the unbounded-
-- table hazard it would be for a per-row event stream like audit_queue.
-- Filters are all sqlc.narg so a caller can filter on any subset
-- (a user-scoped "what did I do" view is actor_user_id + maybe event).
SELECT id, actor_user_id, actor_kind, actor_name,
       event, resource_type, resource_id, details_json, created_at
FROM actor_audit
WHERE (sqlc.narg('actor_user_id') IS NULL OR actor_user_id = sqlc.narg('actor_user_id'))
  AND (sqlc.narg('actor_kind') IS NULL OR actor_kind = sqlc.narg('actor_kind'))
  AND (sqlc.narg('event') IS NULL OR event = sqlc.narg('event'))
  AND (sqlc.narg('resource_type') IS NULL OR resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('resource_id') IS NULL OR resource_id = sqlc.narg('resource_id'))
  AND (sqlc.narg('since_unix') IS NULL OR created_at >= sqlc.narg('since_unix'))
  AND (sqlc.narg('until_unix') IS NULL OR created_at < sqlc.narg('until_unix'))
ORDER BY created_at DESC, id DESC
LIMIT ?1 OFFSET ?2;

-- name: CountActorAudit :one
-- Counterpart to ListActorAudit -- must apply identical WHERE so
-- pagination totals match the visible rows.
SELECT COUNT(*)
FROM actor_audit
WHERE (sqlc.narg('actor_user_id') IS NULL OR actor_user_id = sqlc.narg('actor_user_id'))
  AND (sqlc.narg('actor_kind') IS NULL OR actor_kind = sqlc.narg('actor_kind'))
  AND (sqlc.narg('event') IS NULL OR event = sqlc.narg('event'))
  AND (sqlc.narg('resource_type') IS NULL OR resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('resource_id') IS NULL OR resource_id = sqlc.narg('resource_id'))
  AND (sqlc.narg('since_unix') IS NULL OR created_at >= sqlc.narg('since_unix'))
  AND (sqlc.narg('until_unix') IS NULL OR created_at < sqlc.narg('until_unix'));
