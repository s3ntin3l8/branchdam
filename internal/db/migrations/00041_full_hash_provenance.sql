-- +goose Up
-- Records who computed each media_nodes.full_hash (#505). Before this, any
-- 64-character full_hash counted as "verified" for Tier-1 pruning and for
-- POST /api/v1/agent/node-status, including a hash a paired device merely
-- asserted in an agent event or a rebase. Only a hash the server computed
-- from the bytes itself (scanner, upload) or re-hashed and matched
-- (internal/rehash) may now authorise a purge.
--
-- One row per node. full_hash is the hash this row vouches for: a node's
-- hash counts as server-verified only while source = 'SERVER' AND this
-- full_hash equals media_nodes.full_hash, so a later change to the node's
-- hash (cleared by an in-place write, set by a different path) silently
-- stops the old row from vouching for it -- no write path has to remember
-- to invalidate anything. Rows are updated in place, never deleted. The FK
-- is RESTRICT, matching every other FK in the schema. A separate table
-- (rather than a media_nodes column) keeps the many explicit-column
-- media_nodes queries untouched, the same choice 00035_node_creators made.
--
-- rehash_* is internal/rehash's bookkeeping for an AGENT row: the last
-- attempt time, how many attempts, and the outcome when it was not a match
-- (MISMATCH: the bytes hash to rehash_computed, not the asserted value --
-- terminal for this asserted value; ERROR: the file could not be read, so
-- the worker retries later). A match flips source to 'SERVER' instead.
CREATE TABLE full_hash_provenance (
    node_id           INTEGER PRIMARY KEY REFERENCES media_nodes(id) ON DELETE RESTRICT,
    full_hash         TEXT    NOT NULL CHECK (length(full_hash) = 64),
    source            TEXT    NOT NULL CHECK (source IN ('SERVER', 'AGENT')),
    recorded_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    rehash_state      TEXT    CHECK (rehash_state IN ('MISMATCH', 'ERROR')),
    rehash_checked_at INTEGER,
    rehash_attempts   INTEGER NOT NULL DEFAULT 0,
    rehash_computed   TEXT    CHECK (rehash_computed IS NULL OR length(rehash_computed) = 64)
);

-- Conservative backfill: a node a paired device created (it has a
-- node_creators row) holds a hash the device may only have asserted, so it
-- starts as AGENT and internal/rehash re-verifies it. Every other hash came
-- from the scanner or a server-side upload. Device uploads through
-- /api/v1/agent/upload were in fact hashed by the server too, but they also
-- carry a node_creators row and cannot be told apart here; re-hashing them
-- costs one read and changes nothing else. Agent pruning pauses for those
-- nodes until their re-hash completes.
INSERT INTO full_hash_provenance (node_id, full_hash, source)
SELECT n.id, n.full_hash,
       CASE WHEN EXISTS (SELECT 1 FROM node_creators c WHERE c.node_uuid = n.node_uuid)
            THEN 'AGENT' ELSE 'SERVER' END
FROM media_nodes n
WHERE n.full_hash IS NOT NULL AND length(n.full_hash) = 64;

-- internal/rehash's claim query scans AGENT rows only.
CREATE INDEX ix_full_hash_provenance_agent
    ON full_hash_provenance (node_id)
    WHERE source = 'AGENT';

-- +goose Down
DROP INDEX ix_full_hash_provenance_agent;
DROP TABLE full_hash_provenance;
