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
-- auth_provider + pat_id record the credential. auth_provider is NULL
-- for machine/system/anonymous actors and for rows written before the
-- columns existed. pat_id is the user_pats row when a token acted.
-- pat_name is that token's display name (LEFT JOIN; NULL when pat_id is).
--
-- All positional params use bare ?1/?2 (not sqlc.arg) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: InsertActorAudit :exec
INSERT INTO actor_audit (
    actor_user_id, actor_kind, actor_name,
    event, resource_type, resource_id, details_json,
    auth_provider, pat_id
) VALUES (
    ?1, ?2, ?3,
    ?4, ?5, ?6, ?7,
    ?8, ?9
);

-- name: ListActorAudit :many
-- Admin/audit list view. Offset pagination -- audit volume is bounded
-- (one entry per admin action), so pagination drift isn't the unbounded-
-- table hazard it would be for a per-row event stream like audit_queue.
-- Filters are all sqlc.narg so a caller can filter on any subset
-- (a user-scoped "what did I do" view is actor_user_id + maybe event).
-- Columns are qualified: user_pats also has id and created_at.
SELECT actor_audit.id, actor_audit.actor_user_id, actor_audit.actor_kind, actor_audit.actor_name,
       actor_audit.event, actor_audit.resource_type, actor_audit.resource_id,
       actor_audit.details_json, actor_audit.created_at,
       actor_audit.auth_provider, actor_audit.pat_id,
       user_pats.name AS pat_name
FROM actor_audit
LEFT JOIN user_pats ON user_pats.id = actor_audit.pat_id
WHERE (sqlc.narg('actor_user_id') IS NULL OR actor_audit.actor_user_id = sqlc.narg('actor_user_id'))
  AND (sqlc.narg('actor_kind') IS NULL OR actor_audit.actor_kind = sqlc.narg('actor_kind'))
  AND (sqlc.narg('event') IS NULL OR actor_audit.event = sqlc.narg('event'))
  AND (sqlc.narg('resource_type') IS NULL OR actor_audit.resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('resource_id') IS NULL OR actor_audit.resource_id = sqlc.narg('resource_id'))
  AND (sqlc.narg('since_unix') IS NULL OR actor_audit.created_at >= sqlc.narg('since_unix'))
  AND (sqlc.narg('until_unix') IS NULL OR actor_audit.created_at < sqlc.narg('until_unix'))
ORDER BY actor_audit.created_at DESC, actor_audit.id DESC
LIMIT ?1 OFFSET ?2;

-- name: CountActorAudit :one
-- Counterpart to ListActorAudit -- must apply identical WHERE so
-- pagination totals match the visible rows. No join: the filters only
-- touch actor_audit columns, and the LEFT JOIN does not drop rows.
SELECT COUNT(*)
FROM actor_audit
WHERE (sqlc.narg('actor_user_id') IS NULL OR actor_user_id = sqlc.narg('actor_user_id'))
  AND (sqlc.narg('actor_kind') IS NULL OR actor_kind = sqlc.narg('actor_kind'))
  AND (sqlc.narg('event') IS NULL OR event = sqlc.narg('event'))
  AND (sqlc.narg('resource_type') IS NULL OR resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('resource_id') IS NULL OR resource_id = sqlc.narg('resource_id'))
  AND (sqlc.narg('since_unix') IS NULL OR created_at >= sqlc.narg('since_unix'))
  AND (sqlc.narg('until_unix') IS NULL OR created_at < sqlc.narg('until_unix'));
