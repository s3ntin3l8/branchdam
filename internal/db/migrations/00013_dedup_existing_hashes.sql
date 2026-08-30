-- +goose Up
-- Archive duplicate live nodes, keeping MIN(id) as the survivor.
-- This prevents the unique index in 00014 from failing on existing data.
WITH survivors AS (
  SELECT full_hash, MIN(id) AS keep_id
  FROM media_nodes
  WHERE full_hash IS NOT NULL AND full_hash != '' AND lifecycle_state != 'ARCHIVED'
  GROUP BY full_hash HAVING COUNT(*) > 1
)
UPDATE media_nodes
SET lifecycle_state = 'ARCHIVED', updated_at = unixepoch()
WHERE full_hash IN (SELECT full_hash FROM survivors)
  AND id NOT IN (SELECT keep_id FROM survivors)
  AND lifecycle_state != 'ARCHIVED';

-- +goose Down
-- Cannot reliably un-archive previously deduplicated nodes.
