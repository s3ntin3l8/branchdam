-- name: UpsertMediaEdge :one
-- A human decision outranks any resolver, permanently: the UPDATE branch is
-- gated by a WHERE that skips rows already CONFIRMED or REJECTED entirely
-- (SQLite's upsert semantics: when the WHERE evaluates false, DO UPDATE is
-- skipped and the pre-existing row is returned unmodified). confidence only
-- ever increases (MAX), never regresses if a later resolve pass finds a
-- weaker signal for the same edge; review_state is recomputed from that
-- same MAX'd confidence rather than trusting the just-inserted value, so
-- the two can never disagree about which confidence they describe.
INSERT INTO media_edges (
    source_node_id, target_node_id, relationship_type, confidence, tier,
    resolver, evidence_json, review_state
) VALUES (
    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8
)
ON CONFLICT (source_node_id, target_node_id, relationship_type) DO UPDATE SET
    confidence    = MAX(excluded.confidence, media_edges.confidence),
    tier          = excluded.tier,
    resolver      = excluded.resolver,
    evidence_json = excluded.evidence_json,
    review_state  = CASE
                        WHEN MAX(excluded.confidence, media_edges.confidence) >= 0.90 THEN 'AUTO_ACCEPTED'
                        ELSE 'NEEDS_REVIEW'
                    END,
    updated_at    = unixepoch()
WHERE media_edges.review_state NOT IN ('CONFIRMED', 'REJECTED')
RETURNING id, source_node_id, target_node_id, relationship_type, confidence,
          tier, resolver, evidence_json, review_state, reviewed_at, reviewed_by,
          created_at, updated_at;

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
