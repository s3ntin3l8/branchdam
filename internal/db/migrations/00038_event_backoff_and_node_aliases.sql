-- +goose Up
-- Two agent-contract reliability fixes (see docs/schema.md):
--
-- 1. event_queue.next_attempt_at: the drainer used to retry a transiently
--    failing event on every pass (~2s apart) and mark it FAILED after three
--    attempts, so a transient condition that lasts longer than a few
--    seconds -- a Tier-3 rebase whose archive copy is still in flight --
--    failed permanently. A retryable failure now pushes next_attempt_at out
--    exponentially and ListPendingAgentEvents only claims rows whose
--    next_attempt_at has passed. Existing rows default to 0 (due now).
ALTER TABLE event_queue ADD COLUMN next_attempt_at INTEGER NOT NULL DEFAULT 0;

-- 2. node_uuid_aliases: an EVENT_NODE_CREATED whose full_hash already
--    belongs to a live node is content-deduplicated (no second media_nodes
--    row), which left the device holding a node_uuid the server had never
--    heard of: its follow-up POST /api/v1/agent/rebase fell through to the
--    insert branch and hit ux_media_nodes_live_full_hash (500, forever).
--    The dedup now records the device's UUID here as an alias of the
--    existing node, and the agent rebase / node-status / event paths
--    resolve it. agent_id is the device that recorded the alias (same free
--    text as event_queue.agent_id, deliberately no FK -- see
--    node_creators). Rows are never deleted; the FK is RESTRICT like every
--    other one in this schema.
CREATE TABLE node_uuid_aliases (
    alias_uuid TEXT    PRIMARY KEY,
    node_uuid  TEXT    NOT NULL REFERENCES media_nodes(node_uuid) ON DELETE RESTRICT,
    agent_id   TEXT    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    CHECK (alias_uuid <> node_uuid)
);
CREATE INDEX ix_node_uuid_aliases_node ON node_uuid_aliases(node_uuid);

-- +goose Down
DROP INDEX IF EXISTS ix_node_uuid_aliases_node;
DROP TABLE node_uuid_aliases;
ALTER TABLE event_queue DROP COLUMN next_attempt_at;
