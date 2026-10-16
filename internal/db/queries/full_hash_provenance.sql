-- name: RecordFullHashProvenance :exec
-- Records who computed node ?1's full_hash ?2: 'SERVER' (scanner, upload,
-- or a scanner pass that recomputed the same value) or 'AGENT' (a paired
-- device asserted it in an event or a rebase). Called next to every write
-- of a new full_hash value; see 00041_full_hash_provenance.sql.
--
-- Never downgrades: recording 'AGENT' for the hash a 'SERVER' row already
-- vouches for keeps 'SERVER', since the server computed those exact bytes'
-- hash itself. A new hash value replaces the row outright and resets the
-- re-hash bookkeeping, which only ever described the old value. Recording
-- 'SERVER' also clears it: the server has just computed this value from the
-- bytes, which supersedes an earlier re-hash outcome.
INSERT INTO full_hash_provenance (node_id, full_hash, source, recorded_at)
VALUES (?1, ?2, ?3, unixepoch())
ON CONFLICT (node_id) DO UPDATE SET
    source = CASE
        WHEN excluded.source = 'SERVER' THEN 'SERVER'
        WHEN full_hash_provenance.full_hash = excluded.full_hash AND full_hash_provenance.source = 'SERVER' THEN 'SERVER'
        ELSE 'AGENT'
    END,
    recorded_at = CASE
        WHEN full_hash_provenance.full_hash = excluded.full_hash AND full_hash_provenance.source = excluded.source THEN full_hash_provenance.recorded_at
        ELSE excluded.recorded_at
    END,
    rehash_state = CASE
        WHEN excluded.source = 'AGENT' AND full_hash_provenance.full_hash = excluded.full_hash THEN full_hash_provenance.rehash_state
        ELSE NULL
    END,
    rehash_checked_at = CASE
        WHEN excluded.source = 'AGENT' AND full_hash_provenance.full_hash = excluded.full_hash THEN full_hash_provenance.rehash_checked_at
        ELSE NULL
    END,
    rehash_attempts = CASE
        WHEN excluded.source = 'AGENT' AND full_hash_provenance.full_hash = excluded.full_hash THEN full_hash_provenance.rehash_attempts
        ELSE 0
    END,
    rehash_computed = CASE
        WHEN excluded.source = 'AGENT' AND full_hash_provenance.full_hash = excluded.full_hash THEN full_hash_provenance.rehash_computed
        ELSE NULL
    END,
    rehash_mismatches = CASE
        WHEN excluded.source = 'AGENT' AND full_hash_provenance.full_hash = excluded.full_hash THEN full_hash_provenance.rehash_mismatches
        ELSE 0
    END,
    rehash_retry_at = CASE
        WHEN excluded.source = 'AGENT' AND full_hash_provenance.full_hash = excluded.full_hash THEN full_hash_provenance.rehash_retry_at
        ELSE NULL
    END,
    full_hash = excluded.full_hash;

-- name: GetFullHashProvenance :one
SELECT node_id, full_hash, source, recorded_at, rehash_state,
       rehash_checked_at, rehash_attempts, rehash_computed,
       rehash_mismatches, rehash_retry_at
FROM full_hash_provenance
WHERE node_id = ?1;

-- name: ListRehashCandidates :many
-- internal/rehash's claim query: up to batch_limit live Tier-3 nodes, in
-- node id order after after_id, whose current full_hash is only
-- agent-asserted and has not been re-hashed yet -- or whose last attempt
-- failed to read the file (ERROR) before retry_before, or read bytes that
-- did not match (MISMATCH) and whose rehash_retry_at has come (now_unix).
-- A MISMATCH is re-read on a doubling backoff the worker records with it
-- (00042), so that restoring a good copy of the file onto the archive
-- verifies it without anyone editing the database; the stored value is
-- never replaced by what was read. A row whose full_hash
-- no longer equals the node's vouches for nothing and is skipped. Driven from the AGENT partial index
-- (ix_full_hash_provenance_agent) so a fully verified archive costs an
-- empty index range per pass, not a media_nodes scan.
SELECT n.id, n.node_uuid, n.file_path, n.full_hash, n.size_bytes,
       p.rehash_mismatches
FROM full_hash_provenance p
JOIN media_nodes n ON n.id = p.node_id
JOIN storage_locations s ON s.id = n.storage_location_id
WHERE p.source = 'AGENT'
  AND p.node_id > sqlc.arg(after_id)
  AND p.full_hash = n.full_hash
  AND (p.rehash_state IS NULL
       OR (p.rehash_state = 'ERROR' AND p.rehash_checked_at < sqlc.arg(retry_before))
       OR (p.rehash_state = 'MISMATCH' AND p.rehash_retry_at <= sqlc.arg(now_unix)))
  AND s.tier = 'TIER3_MASTER_ARCHIVE'
  AND n.lifecycle_state IN ('ACTIVE', 'HIDDEN')
ORDER BY p.node_id
LIMIT sqlc.arg(batch_limit);

-- name: MarkRehashMatched :execrows
-- The server re-read node ?1's bytes and they hash to the asserted value
-- ?2: from now on that value is server-computed. Conditional on the row
-- still vouching for ?2, so a hash that changed while the file was being
-- read is never upgraded.
UPDATE full_hash_provenance
SET source = 'SERVER', recorded_at = unixepoch(),
    rehash_state = NULL, rehash_computed = NULL,
    rehash_mismatches = 0, rehash_retry_at = NULL,
    rehash_checked_at = unixepoch(), rehash_attempts = rehash_attempts + 1
WHERE node_id = ?1 AND full_hash = ?2 AND source = 'AGENT';

-- name: RecordRehashError :execrows
-- A re-hash of node ?1's asserted value ?2 could not read the file: ERROR,
-- retried after the worker's RetryAfter. An earlier MISMATCH count is
-- kept, so a share that flaps between reads doesn't restart the backoff.
-- The row stays 'AGENT', so the value keeps authorising nothing. Same ?2
-- condition as MarkRehashMatched.
UPDATE full_hash_provenance
SET rehash_state = 'ERROR', rehash_computed = NULL, rehash_retry_at = NULL,
    rehash_checked_at = unixepoch(), rehash_attempts = rehash_attempts + 1
WHERE node_id = ?1 AND full_hash = ?2 AND source = 'AGENT';

-- name: RecordRehashMismatch :execrows
-- A re-hash of node ?1's asserted value ?2 read bytes that hash to ?3
-- instead: MISMATCH, counted in rehash_mismatches and not read again
-- before ?4 (unix seconds; the worker's doubling backoff). The row stays
-- 'AGENT', so the value keeps authorising nothing. Same ?2 condition as
-- MarkRehashMatched.
UPDATE full_hash_provenance
SET rehash_state = 'MISMATCH', rehash_computed = ?3,
    rehash_mismatches = rehash_mismatches + 1, rehash_retry_at = ?4,
    rehash_checked_at = unixepoch(), rehash_attempts = rehash_attempts + 1
WHERE node_id = ?1 AND full_hash = ?2 AND source = 'AGENT';
