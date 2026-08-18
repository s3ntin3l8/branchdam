-- name: UpsertMediaEdge :one
-- A human decision outranks any resolver, permanently: the UPDATE branch is
-- gated by a WHERE that skips rows already CONFIRMED or REJECTED entirely.
-- IMPORTANT: when that WHERE evaluates false, SQLite's RETURNING emits NO
-- ROW at all -- it does NOT return the pre-existing row unmodified (verified
-- against SQLite directly; an earlier version of this comment claimed
-- otherwise and was wrong). Because this query is :one, a human-locked edge
-- therefore surfaces to the caller as sql.ErrNoRows, which internal/graph's
-- Engine.ResolveAndCommit must treat as "edge intentionally left untouched,
-- re-fetch it and continue" -- NOT as a failure to roll the whole batch
-- back on.
--
-- confidence only ever increases (MAX), never regresses if a later resolve
-- pass finds a weaker signal for the same edge. tier/resolver/evidence_json/
-- review_state are each keyed on WHICH SIDE actually supplied that MAX'd
-- confidence (excluded, i.e. this call's candidate, vs. the row as it stood
-- before this call) rather than being taken unconditionally from excluded or
-- re-derived from a hardcoded threshold -- both of those independently let a
-- later, weaker resolver pass silently overwrite a stronger earlier one:
-- unconditional excluded.tier/resolver/evidence_json could stamp a losing
-- pass's provenance next to a winning MAX'd confidence, and a hardcoded
-- ">= 0.90" ignored internal/graph's per-tier auto-accept threshold (0.85
-- for Tier 3), downgrading a Tier-3 AUTO_ACCEPTED edge back to NEEDS_REVIEW
-- on every subsequent scan pass. review_state is never independently
-- recomputed here at all: excluded.review_state (bind ?8) already carries
-- the tier-correct value Engine computed for this candidate, so keying it
-- the same way as confidence keeps the two permanently consistent. On an
-- exact confidence tie, `>=` deliberately favors the incoming candidate
-- (excluded) -- that's what refreshes evidence_json/resolver/timestamps on
-- an identical same-resolver re-fire, the common case. Do not tighten this
-- to `>`.
INSERT INTO media_edges (
    source_node_id, target_node_id, relationship_type, confidence, tier,
    resolver, evidence_json, review_state
) VALUES (
    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8
)
ON CONFLICT (source_node_id, target_node_id, relationship_type) DO UPDATE SET
    confidence    = MAX(excluded.confidence, media_edges.confidence),
    tier          = CASE WHEN excluded.confidence >= media_edges.confidence THEN excluded.tier ELSE media_edges.tier END,
    resolver      = CASE WHEN excluded.confidence >= media_edges.confidence THEN excluded.resolver ELSE media_edges.resolver END,
    evidence_json = CASE WHEN excluded.confidence >= media_edges.confidence THEN excluded.evidence_json ELSE media_edges.evidence_json END,
    review_state  = CASE WHEN excluded.confidence >= media_edges.confidence THEN excluded.review_state ELSE media_edges.review_state END,
    updated_at    = unixepoch()
WHERE media_edges.review_state NOT IN ('CONFIRMED', 'REJECTED')
RETURNING id, source_node_id, target_node_id, relationship_type, confidence,
          tier, resolver, evidence_json, review_state, reviewed_at, reviewed_by,
          created_at, updated_at;

-- name: GetMediaEdgeBySourceTargetRel :one
-- Used by internal/graph.Engine.ResolveAndCommit when UpsertMediaEdge
-- returns sql.ErrNoRows -- the edge is CONFIRMED/REJECTED and was
-- intentionally left untouched, so the caller re-fetches its current state
-- here rather than treating the no-row RETURNING as an error.
SELECT id, source_node_id, target_node_id, relationship_type, confidence,
       tier, resolver, evidence_json, review_state, reviewed_at, reviewed_by,
       created_at, updated_at
FROM media_edges
WHERE source_node_id = ?1 AND target_node_id = ?2 AND relationship_type = ?3;

-- name: MediaEdgeExists :one
-- Checked before UpsertMediaEdge so the caller can tell a genuinely new
-- edge apart from an existing one whose confidence/evidence was merely
-- refreshed -- UpsertMediaEdge's RETURNING row looks the same either way.
-- Backs scan_jobs.edges_created (fix(pipeline): #90).
SELECT EXISTS(
    SELECT 1 FROM media_edges
    WHERE source_node_id = ?1 AND target_node_id = ?2 AND relationship_type = ?3
) AS edge_exists;

-- name: ConfirmMediaEdge :exec
UPDATE media_edges
SET review_state = 'CONFIRMED', reviewed_at = unixepoch(), reviewed_by = ?2, updated_at = unixepoch()
WHERE id = ?1;

-- name: RejectMediaEdge :exec
UPDATE media_edges
SET review_state = 'REJECTED', reviewed_at = unixepoch(), reviewed_by = ?2, updated_at = unixepoch()
WHERE id = ?1;

-- name: ResolvedEdgeParentMissing :one
-- Backs T7's regression guard: v_media_edges_resolved.parent_missing must
-- be true for every relationship_type, not just DERIVED_FROM -- the thing
-- the spec's deleted trigger (docs/schema.md fix #4) never did.
SELECT parent_missing FROM v_media_edges_resolved WHERE id = ?1;
