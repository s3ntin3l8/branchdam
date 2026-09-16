-- +goose Up

-- Resolve removal is a graph transition, not a row deletion. Retain the
-- original edge and its review history for audit while excluding it from
-- lineage and graph-status reads.
ALTER TABLE media_edges ADD COLUMN is_active INTEGER NOT NULL DEFAULT 1
    CHECK (is_active IN (0, 1));

CREATE TABLE resolve_timeline_scopes (
    agent_id TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    timeline_node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    PRIMARY KEY (agent_id, scope_id, timeline_node_id)
);
CREATE INDEX ix_resolve_timeline_scopes_node ON resolve_timeline_scopes(timeline_node_id);

DROP VIEW v_media_edges_resolved;
CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING') AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id
WHERE e.is_active = 1;

-- +goose Down

DROP TABLE resolve_timeline_scopes;
DROP VIEW v_media_edges_resolved;

ALTER TABLE media_edges RENAME TO media_edges_with_active;
CREATE TABLE media_edges (
    id                INTEGER PRIMARY KEY,
    source_node_id    INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    target_node_id    INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    relationship_type TEXT NOT NULL
        CHECK (relationship_type IN ('DERIVED_FROM','FINAL_EXPORT','PROXY_OF','PROJECT_SIDECAR','DUPLICATE_OF')),
    confidence    REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    tier          INTEGER NOT NULL CHECK (tier IN (1,2,3)),
    resolver      TEXT NOT NULL,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    review_state TEXT NOT NULL DEFAULT 'NEEDS_REVIEW'
        CHECK (review_state IN ('AUTO_ACCEPTED','NEEDS_REVIEW','CONFIRMED','REJECTED')),
    reviewed_at INTEGER,
    reviewed_by TEXT,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
    CHECK (source_node_id <> target_node_id),
    CHECK (review_state NOT IN ('CONFIRMED','REJECTED') OR reviewed_at IS NOT NULL),
    UNIQUE (source_node_id, target_node_id, relationship_type)
);
INSERT INTO media_edges (
    id, source_node_id, target_node_id, relationship_type, confidence, tier,
    resolver, evidence_json, review_state, reviewed_at, reviewed_by,
    created_at, updated_at
)
SELECT
    id, source_node_id, target_node_id, relationship_type, confidence, tier,
    resolver, evidence_json, review_state, reviewed_at, reviewed_by,
    created_at, updated_at
FROM media_edges_with_active;
DROP TABLE media_edges_with_active;
CREATE INDEX ix_media_edges_target ON media_edges(target_node_id);
CREATE INDEX ix_media_edges_source ON media_edges(source_node_id);
CREATE INDEX ix_media_edges_audit ON media_edges(review_state, confidence DESC)
    WHERE review_state = 'NEEDS_REVIEW';

CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING') AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id;
