-- +goose Up
-- Records which paired device (agent_id) created a node through the agent
-- API, so the device-authenticated rebase / move / delete paths can refuse
-- to act on nodes a device did not create (any paired device could
-- previously trash or re-point ANY node it could name by UUID).
--
-- A node with no row here was not created by a device: server-side scans,
-- web uploads, and legacy rows whose creating event is no longer in
-- event_queue. Devices have no authority over those. agent_id is the same
-- free-text id event_queue.agent_id carries and deliberately has no FK, so
-- the record survives the pairing being revoked or deleted. A separate
-- table (rather than a media_nodes column) keeps the many explicit-column
-- media_nodes queries untouched.
CREATE TABLE node_creators (
    node_uuid  TEXT    PRIMARY KEY REFERENCES media_nodes(node_uuid) ON DELETE RESTRICT,
    agent_id   TEXT    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- Backfill from creation-type events. A throwaway expression index turns
-- the per-node event lookup into an index seek. INSERT OR IGNORE plus
-- ORDER BY e.id means the earliest processed event per node wins.
CREATE INDEX tmp_ix_event_queue_node_uuid
    ON event_queue (json_extract(payload_json, '$.nodeUuid'), id)
    WHERE status = 'PROCESSED';

INSERT OR IGNORE INTO node_creators (node_uuid, agent_id)
SELECT n.node_uuid, e.agent_id
FROM media_nodes n
JOIN event_queue e
  ON e.status = 'PROCESSED'
 AND json_extract(e.payload_json, '$.nodeUuid') = n.node_uuid
 AND e.event_type IN ('EVENT_NODE_CREATED', 'EVENT_VIRTUAL_NODE_CREATED', 'EVENT_PATH_REBASED')
ORDER BY e.id;

DROP INDEX tmp_ix_event_queue_node_uuid;

-- +goose Down
DROP TABLE node_creators;
