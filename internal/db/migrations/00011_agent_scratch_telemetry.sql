-- +goose Up
-- agent_scratch_telemetry stores the latest reported workstation scratch capacity
-- breakdown (render cache, mirrors, proxies, free/used space) and prune run statistics
-- (lastPruneTimestampUnix, lastReclaimedBytes, lastPruneDurationMs, prunedItemCounts).
-- It is keyed by agent_id so querying connected workstation health is an O(1) lookup
-- and unbounded historical table growth is avoided.
-- No triggers, no FKs -- updated_at is set explicitly in every query (docs/schema.md fix #5).
CREATE TABLE agent_scratch_telemetry (
    agent_id                    TEXT    PRIMARY KEY,
    client_version              TEXT    NOT NULL DEFAULT '',
    timestamp_unix              INTEGER NOT NULL,
    mount_path                  TEXT    NOT NULL DEFAULT '',
    total_bytes                 INTEGER NOT NULL DEFAULT 0,
    free_bytes                  INTEGER NOT NULL DEFAULT 0,
    used_bytes                  INTEGER NOT NULL DEFAULT 0,
    mirrors_size_bytes          INTEGER NOT NULL DEFAULT 0,
    render_cache_size_bytes      INTEGER NOT NULL DEFAULT 0,
    proxies_size_bytes          INTEGER NOT NULL DEFAULT 0,
    prunable_bytes              INTEGER NOT NULL DEFAULT 0,
    last_prune_timestamp_unix   INTEGER NOT NULL DEFAULT 0,
    last_reclaimed_bytes        INTEGER NOT NULL DEFAULT 0,
    last_prune_duration_ms       INTEGER NOT NULL DEFAULT 0,
    pruned_item_counts          TEXT    NOT NULL DEFAULT '{}',
    updated_at                  INTEGER NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS agent_scratch_telemetry;
