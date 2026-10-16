-- login_audit: append-only log of authentication events (PR #407).
-- Read by /api/v1/audit?type=login alongside /api/v1/audit?type=activity
-- (which reads actor_audit). login_audit is owned by internal/auth/users;
-- these queries are kept here so the merged audit route can read both
-- tables through the same sqlcgen layer without a second db call site.
--
-- All positional params use bare ?1/?2 (not sqlc.arg) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: ListLoginAudit :many
-- Newest-first. Offset pagination -- volume is bounded by authentication
-- events (not per-asset), so the drift problem that justifies keyset
-- pagination on /api/v1/edges/audit doesn't apply here.
-- Filters are all sqlc.narg, same shape as ListActorAudit, so the audit
-- log page's filters apply to this stream too: user_id (actor),
-- outcome (shown as the event), source (shown as the resource id) and a
-- created_at window.
SELECT id, user_id, username_presented, source, outcome, ip, user_agent, created_at
FROM login_audit
WHERE (sqlc.narg('user_id') IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('outcome') IS NULL OR outcome = sqlc.narg('outcome'))
  AND (sqlc.narg('source') IS NULL OR source = sqlc.narg('source'))
  AND (sqlc.narg('since_unix') IS NULL OR created_at >= sqlc.narg('since_unix'))
  AND (sqlc.narg('until_unix') IS NULL OR created_at < sqlc.narg('until_unix'))
ORDER BY created_at DESC, id DESC
LIMIT ?1 OFFSET ?2;

-- name: CountLoginAudit :one
-- Counterpart to ListLoginAudit -- must apply identical WHERE so
-- pagination totals match the visible rows.
SELECT COUNT(*)
FROM login_audit
WHERE (sqlc.narg('user_id') IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('outcome') IS NULL OR outcome = sqlc.narg('outcome'))
  AND (sqlc.narg('source') IS NULL OR source = sqlc.narg('source'))
  AND (sqlc.narg('since_unix') IS NULL OR created_at >= sqlc.narg('since_unix'))
  AND (sqlc.narg('until_unix') IS NULL OR created_at < sqlc.narg('until_unix'));
