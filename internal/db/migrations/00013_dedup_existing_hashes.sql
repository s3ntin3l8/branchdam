-- +goose Up
-- Archive duplicate live nodes, keeping the lowest active ID as the survivor.
-- This prevents the unique index in 00014 from failing on existing data.
WITH survivors AS (
  SELECT full_hash, MIN(id) AS keep_id
  FROM media_nodes
  WHERE full_hash IS NOT NULL AND full_hash != '' AND lifecycle_state IN ('ACTIVE', 'HIDDEN')
  GROUP BY full_hash HAVING COUNT(*) > 1
)
UPDATE media_nodes
SET lifecycle_state = 'ARCHIVED', updated_at = unixepoch()
WHERE full_hash IN (SELECT full_hash FROM survivors)
  AND id NOT IN (SELECT keep_id FROM survivors)
  AND lifecycle_state IN ('ACTIVE', 'HIDDEN');

-- +goose Down
-- Cannot reliably un-archive previously deduplicated nodes.
