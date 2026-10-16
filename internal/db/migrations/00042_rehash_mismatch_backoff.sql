-- +goose Up
-- internal/rehash re-reads a MISMATCH row on a doubling backoff (1, 2, 4
-- ... days, capped at 30). rehash_attempts can't drive that: it counts
-- every attempt, and an archive share that was unmounted for a day piles
-- up hourly ERROR attempts, so the first real mismatch would already land
-- on the cap. rehash_mismatches counts only the mismatches recorded for
-- the row's current asserted value, and rehash_retry_at is when the worker
-- may read a MISMATCH row again (unix seconds, set by the worker when it
-- records the mismatch), so the claim query filters held-back rows itself
-- instead of handing them to the worker to skip. Both reset with the rest
-- of the re-hash bookkeeping when the asserted value changes or the server
-- computes the hash.
ALTER TABLE full_hash_provenance ADD COLUMN rehash_mismatches INTEGER NOT NULL DEFAULT 0;
ALTER TABLE full_hash_provenance ADD COLUMN rehash_retry_at INTEGER;

-- A MISMATCH recorded before this migration counts as the first one and
-- is due a day after it was checked, as 00041 promised.
UPDATE full_hash_provenance
SET rehash_mismatches = 1,
    rehash_retry_at = coalesce(rehash_checked_at, unixepoch()) + 86400
WHERE rehash_state = 'MISMATCH';

-- +goose Down
ALTER TABLE full_hash_provenance DROP COLUMN rehash_retry_at;
ALTER TABLE full_hash_provenance DROP COLUMN rehash_mismatches;
