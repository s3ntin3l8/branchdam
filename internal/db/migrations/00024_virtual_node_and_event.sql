-- +goose Up

-- Expand event_type CHECK to include EVENT_VIRTUAL_NODE_CREATED.
-- SQLite cannot ALTER CHECK constraints; recreate the table.
CREATE TABLE event_queue_new (
    id           INTEGER PRIMARY KEY,
    event_uuid   TEXT NOT NULL UNIQUE,
    agent_id     TEXT NOT NULL,
    event_type   TEXT NOT NULL
        CHECK (event_type IN ('EVENT_NODE_CREATED','EVENT_EDGE_ATTACHED',
               'EVENT_NODE_MOVED','EVENT_NODE_DELETED','EVENT_PATH_REBASED',
               'EVENT_VIRTUAL_NODE_CREATED')),
    payload_json TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    error_log    TEXT,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    processed_at INTEGER,
    retry_count  INTEGER NOT NULL DEFAULT 0
);
INSERT INTO event_queue_new
    (id, event_uuid, agent_id, event_type, payload_json,
     status, error_log, created_at, processed_at, retry_count)
SELECT
    id, event_uuid, agent_id, event_type, payload_json,
    status, error_log, created_at, processed_at,
    COALESCE(retry_count, 0)
FROM event_queue;
DROP TABLE event_queue;
ALTER TABLE event_queue_new RENAME TO event_queue;
CREATE INDEX ix_event_queue_status ON event_queue(status, id);

-- +goose Down

-- Restore event_queue without EVENT_VIRTUAL_NODE_CREATED in the CHECK.
-- Virtual events are silently dropped on downgrade (one-way migration,
-- same precedent as 00021). Rows with the new type cannot be inserted
-- into event_queue_old's narrower CHECK, so filter them out.
CREATE TABLE event_queue_old (
    id           INTEGER PRIMARY KEY,
    event_uuid   TEXT NOT NULL UNIQUE,
    agent_id     TEXT NOT NULL,
    event_type   TEXT NOT NULL
        CHECK (event_type IN ('EVENT_NODE_CREATED','EVENT_EDGE_ATTACHED',
               'EVENT_NODE_MOVED','EVENT_NODE_DELETED','EVENT_PATH_REBASED')),
    payload_json TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    error_log    TEXT,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    processed_at INTEGER,
    retry_count  INTEGER NOT NULL DEFAULT 0
);
INSERT INTO event_queue_old
    (id, event_uuid, agent_id, event_type, payload_json,
     status, error_log, created_at, processed_at, retry_count)
SELECT
    id, event_uuid, agent_id, event_type, payload_json,
    status, error_log, created_at, processed_at,
    COALESCE(retry_count, 0)
FROM event_queue
WHERE event_type <> 'EVENT_VIRTUAL_NODE_CREATED';
DROP TABLE event_queue;
ALTER TABLE event_queue_old RENAME TO event_queue;
CREATE INDEX ix_event_queue_status ON event_queue(status, id);
