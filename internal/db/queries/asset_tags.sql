-- name: ListAssetTags :many
SELECT id, node_id, tag, source, created_by_user_id, pat_id, created_at
FROM asset_tags
WHERE node_id = ?1
ORDER BY tag ASC;

-- name: GetAssetTag :one
SELECT id, node_id, tag, source, created_by_user_id, pat_id, created_at
FROM asset_tags
WHERE node_id = ?1 AND tag = ?2;

-- name: InsertAssetTag :one
INSERT INTO asset_tags (node_id, tag, source, created_by_user_id, pat_id)
VALUES (?1, ?2, ?3, ?4, ?5)
RETURNING id, node_id, tag, source, created_by_user_id, pat_id, created_at;

-- name: DeleteAssetTag :execrows
DELETE FROM asset_tags WHERE node_id = ?1 AND tag = ?2;

-- name: ListTagCounts :many
SELECT tag, COUNT(*) AS count
FROM asset_tags
GROUP BY tag
ORDER BY tag ASC;

-- name: DeleteAssistantTags :execrows
DELETE FROM asset_tags
WHERE source = 'assistant'
  AND (pat_id = sqlc.narg('pat_id') OR sqlc.narg('pat_id') IS NULL)
  AND (created_at >= sqlc.narg('since_unix') OR sqlc.narg('since_unix') IS NULL);

-- name: ListAssetAnnotations :many
SELECT id, node_id, kind, text, source, model, created_by_user_id, pat_id, created_at, updated_at
FROM asset_annotations
WHERE node_id = ?1
ORDER BY kind ASC, source ASC, id ASC;

-- name: UpsertAssetDescription :one
INSERT INTO asset_annotations (node_id, kind, text, source, model, created_by_user_id, pat_id)
VALUES (?1, 'description', ?2, ?3, ?4, ?5, ?6)
ON CONFLICT (node_id, source) WHERE kind = 'description' DO UPDATE SET
    text = excluded.text,
    model = excluded.model,
    created_by_user_id = excluded.created_by_user_id,
    pat_id = excluded.pat_id,
    updated_at = unixepoch()
RETURNING id, node_id, kind, text, source, model, created_by_user_id, pat_id, created_at, updated_at;

-- name: DeleteAssistantAnnotations :execrows
DELETE FROM asset_annotations
WHERE source = 'assistant'
  AND (pat_id = sqlc.narg('pat_id') OR sqlc.narg('pat_id') IS NULL)
  AND (created_at >= sqlc.narg('since_unix') OR sqlc.narg('since_unix') IS NULL);
