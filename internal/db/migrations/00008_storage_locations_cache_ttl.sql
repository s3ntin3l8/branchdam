-- +goose Up
-- cache_ttl_hours persists what was previously a config-only,
-- restart-only value (config.yaml's storageLocations[].cacheTtlHours).
-- Before this migration, internal/httpapi's handlePrune re-joined the live
-- config back to this table by root_path string match to recover a
-- location's TTL at prune time -- editing a rootPath in config therefore
-- silently orphaned that location's TTL: the old row deactivates (M6's
-- self-heal), a new row appears with prunable carried over correctly (it
-- IS a persisted column), but nothing re-joins cacheTtlHours to it until
-- the rootPath strings line up again, with no error or warning (#238).
-- Every existing row defaults to 0 ("never eligible", the same value
-- handlePrune already treats as "no TTL configured"), so this is additive
-- and needs no data-correction migration, matching thumb_state's (00007)
-- pattern rather than #132's 00006.
ALTER TABLE storage_locations ADD COLUMN cache_ttl_hours INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE storage_locations DROP COLUMN cache_ttl_hours;
