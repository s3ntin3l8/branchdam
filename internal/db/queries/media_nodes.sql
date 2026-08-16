-- name: ListMediaNodes :many
-- Backs GET /api/v1/assets. Excludes archived rows by default -- an
-- archived node is reachable via its successor's superseded history, not
-- the main asset list.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE lifecycle_state != 'ARCHIVED'
ORDER BY id DESC
LIMIT ?1 OFFSET ?2;

-- name: GetMediaNodeByID :one
-- Includes archived rows, unlike GetLiveNodeByPath -- used to verify a
-- superseded node's post-archive state (superseded_by, lifecycle_state).
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE id = ?1;

-- name: GetLiveNodeByPath :one
-- The live-path lookup a scan does for every file: is there already a
-- non-archived node at this exact path? Backed by ux_media_nodes_live_path
-- (docs/schema.md fix #3).
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE file_path = ?1 AND lifecycle_state != 'ARCHIVED';

-- name: GetMissingNodeByFastHash :one
-- Pillar 5 move detection: a file vanished (lifecycle_state='MISSING') and
-- a new file elsewhere hashes the same -- likely the same file, moved.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE fast_hash = ?1 AND lifecycle_state = 'MISSING'
LIMIT 1;

-- name: ListLiveNodesByFastHash :many
-- T1 (spec 9.5): before assuming two same-fast_hash files at DIFFERENT live
-- paths are the same content, the pipeline needs every other live node
-- sharing that fast_hash so it can escalate to full_hash and compare.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE fast_hash = ?1 AND lifecycle_state != 'ARCHIVED';

-- name: ListLiveNodesByDocumentID :many
-- Tier-2 xmpOriginalDocumentID resolver: a child's XMP:OriginalDocumentID
-- matching a candidate parent's document_id is a near-certain lineage
-- signal (confidence 0.95).
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE document_id = ?1 AND lifecycle_state != 'ARCHIVED';

-- name: ListLiveNodesByFilenameStem :many
-- Tier-2 filenameStem resolver: candidate parents sharing a normalized
-- filename stem with the child, scored further by capture day / camera /
-- directory match in internal/graph.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at
FROM media_nodes
WHERE filename_stem = ?1 AND lifecycle_state != 'ARCHIVED';

-- name: UpdateMediaNodeGraphStatus :exec
UPDATE media_nodes SET graph_status = ?2, updated_at = unixepoch() WHERE id = ?1;

-- name: InsertMediaNode :one
INSERT INTO media_nodes (
    node_uuid, storage_location_id, file_path, file_name, file_ext,
    size_bytes, mtime_unix, fast_hash, full_hash, phash,
    indexing_status, graph_status, lifecycle_state,
    original_document_id, document_id, derived_from_id,
    captured_at_unix, camera_model, filename_stem,
    first_seen_at, last_seen_at, created_at, updated_at
) VALUES (
    ?1, ?2, ?3, ?4, ?5,
    ?6, ?7, ?8, ?9, ?10,
    ?11, ?12, ?13,
    ?14, ?15, ?16,
    ?17, ?18, ?19,
    unixepoch(), unixepoch(), unixepoch(), unixepoch()
)
RETURNING id, node_uuid, storage_location_id, file_path, file_name, file_ext,
          size_bytes, mtime_unix, fast_hash, full_hash, phash,
          indexing_status, graph_status, lifecycle_state, superseded_by,
          original_document_id, document_id, derived_from_id,
          captured_at_unix, camera_model, filename_stem,
          first_seen_at, last_seen_at, created_at, updated_at;

-- name: ArchiveMediaNode :exec
-- Step 1 of a version collision (docs/schema.md fix #3): archive the OLD
-- row FIRST, before inserting the new one. The partial unique index
-- (WHERE lifecycle_state != 'ARCHIVED') means a live row and a new live
-- row can never share file_path even for an instant within the
-- transaction -- archiving first, not after, is what keeps that true.
UPDATE media_nodes SET lifecycle_state = 'ARCHIVED', updated_at = unixepoch() WHERE id = ?1;

-- name: SetSupersededBy :exec
-- Step 3 of a version collision: link the archived row to its successor,
-- once the successor's id is known (i.e. after InsertMediaNode).
UPDATE media_nodes SET superseded_by = ?2, updated_at = unixepoch() WHERE id = ?1;

-- name: RebaseMissingNodePath :exec
-- Pillar 5 move detection, applied: the id and node_uuid never change, so
-- every edge referencing this node (as parent or child) survives the move
-- untouched -- no CASCADE, no rewrite needed.
UPDATE media_nodes
SET file_path = ?2, file_name = ?3, storage_location_id = ?4,
    lifecycle_state = 'ACTIVE', mtime_unix = ?5,
    last_seen_at = unixepoch(), updated_at = unixepoch()
WHERE id = ?1;

-- name: TouchMediaNode :exec
-- Same content at the same path, seen again on a later scan -- just record
-- that, no new row.
UPDATE media_nodes SET mtime_unix = ?2, last_seen_at = unixepoch(), updated_at = unixepoch() WHERE id = ?1;

-- name: UpdateMediaNodeFullHash :exec
-- Escalation path for T1: computed lazily, only when fast_hash collides
-- with another live node or the file lives on a TIER3_MASTER_ARCHIVE
-- location (docs/schema.md fix #8's full_hash policy).
UPDATE media_nodes SET full_hash = ?2, indexing_status = 'INDEXED_FULL', updated_at = unixepoch() WHERE id = ?1;

-- name: MarkNodeMissing :exec
UPDATE media_nodes SET lifecycle_state = 'MISSING', updated_at = unixepoch() WHERE id = ?1;

-- name: MarkUnseenNodesMissing :execrows
-- Phase 1 (#31): at the end of a successful full scan, every ACTIVE node under
-- the scanned storage location whose last_seen_at predates the scan's start is
-- gone. TouchMediaNode/InsertMediaNode/RebaseMissingNodePath all bump
-- last_seen_at on every node the walk actually saw, so anything still old here
-- was genuinely unseen this scan. Scoped by storage_location_id so a scan of
-- one mount never touches another. unixepoch() is 1s granularity, so a node
-- last seen in a scan that happened to end in the SAME wall-clock second as
-- this scan's start may survive one extra scan -- it is swept the next round,
-- which is delayed-not-wrong.
UPDATE media_nodes
SET lifecycle_state = 'MISSING', updated_at = unixepoch()
WHERE storage_location_id = sqlc.arg('storage_location_id')
  AND lifecycle_state = 'ACTIVE'
  AND last_seen_at < sqlc.arg('before_unix');
