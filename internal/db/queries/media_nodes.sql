-- name: ListMediaNodes :many
-- Backs GET /api/v1/assets. Excludes archived rows by default -- an
-- archived node is reachable via its successor's superseded history, not
-- the main asset list.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE lifecycle_state != 'ARCHIVED'
ORDER BY id DESC
LIMIT ?1 OFFSET ?2;

-- name: ListMediaNodesFiltered :many
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE (sqlc.narg('lifecycle_state') IS NULL OR lifecycle_state = sqlc.narg('lifecycle_state'))
  AND (sqlc.narg('camera_model') IS NULL OR camera_model = sqlc.narg('camera_model'))
  AND (sqlc.narg('graph_status') IS NULL OR graph_status = sqlc.narg('graph_status'))
  AND (sqlc.narg('storage_location_id') IS NULL OR storage_location_id = sqlc.narg('storage_location_id'))
ORDER BY id DESC
LIMIT ?1 OFFSET ?2;

-- name: CountMediaNodesFiltered :one
SELECT COUNT(*)
FROM media_nodes
WHERE (sqlc.narg('lifecycle_state') IS NULL OR lifecycle_state = sqlc.narg('lifecycle_state'))
  AND (sqlc.narg('camera_model') IS NULL OR camera_model = sqlc.narg('camera_model'))
  AND (sqlc.narg('graph_status') IS NULL OR graph_status = sqlc.narg('graph_status'))
  AND (sqlc.narg('storage_location_id') IS NULL OR storage_location_id = sqlc.narg('storage_location_id'));

-- name: ListCameraModelFacets :many
SELECT DISTINCT camera_model
FROM media_nodes
WHERE camera_model IS NOT NULL AND camera_model != '' AND lifecycle_state != 'ARCHIVED'
ORDER BY camera_model ASC;


-- name: GetMediaNodeByID :one
-- Includes archived rows, unlike GetLiveNodeByPath -- used to verify a
-- superseded node's post-archive state (superseded_by, lifecycle_state).
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
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
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
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
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
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
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
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
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE document_id = ?1 AND lifecycle_state != 'ARCHIVED';

-- name: ListLiveNodesByFilenameStem :many
-- Tier-2 filenameStem resolver: candidate parents sharing a normalized
-- filename stem with the child, scored further by capture day / camera /
-- directory match in internal/graph. Capped at ?2 rows -- defense in depth
-- on top of (not instead of) versionSuffixRe's own -\d{1,2} bound
-- (internal/pipeline/commit.go), which narrows but does not close the
-- over-collapse bug class: an unpadded 1-2 digit hyphen-numbering scheme
-- can still produce a large same-stem batch. FilenameStemResolver logs
-- when this cap is actually hit.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE filename_stem = ?1 AND lifecycle_state != 'ARCHIVED'
LIMIT ?2;

-- name: ListTier3Candidates :many
-- Tier-3 spatial-temporal resolver candidate lookup: live nodes sharing
-- camera_serial with captured_at_unix within ±2 seconds of a target timestamp,
-- excluding a given node ID.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE camera_serial = ?1
  AND captured_at_unix >= ?2
  AND captured_at_unix <= ?3
  AND id <> ?4
  AND lifecycle_state != 'ARCHIVED';

-- name: ListLiveNodesByFileName :many
-- Look up live media nodes sharing exact file_name for fallback path matching.
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE file_name = ?1 AND lifecycle_state != 'ARCHIVED';

-- name: UpdateMediaNodeGraphStatus :exec
UPDATE media_nodes SET graph_status = ?2, updated_at = unixepoch() WHERE id = ?1;

-- name: InsertMediaNode :one
INSERT INTO media_nodes (
    node_uuid, storage_location_id, file_path, file_name, file_ext,
    size_bytes, mtime_unix, fast_hash, full_hash, phash,
    indexing_status, graph_status, lifecycle_state,
    original_document_id, document_id, derived_from_id,
    captured_at_unix, camera_model, filename_stem,
    camera_serial, lens_model, source_path_hash,
    first_seen_at, last_seen_at, created_at, updated_at
) VALUES (
    ?1, ?2, ?3, ?4, ?5,
    ?6, ?7, ?8, ?9, ?10,
    ?11, ?12, ?13,
    ?14, ?15, ?16,
    ?17, ?18, ?19, ?20, ?21, ?22,
    unixepoch(), unixepoch(), unixepoch(), unixepoch()
)
RETURNING id, node_uuid, storage_location_id, file_path, file_name, file_ext,
          size_bytes, mtime_unix, fast_hash, full_hash, phash,
          indexing_status, graph_status, lifecycle_state, superseded_by,
          original_document_id, document_id, derived_from_id,
          captured_at_unix, camera_model, filename_stem,
          first_seen_at, last_seen_at, created_at, updated_at,
          camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash;

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
-- Same content at the same path, seen again on a later scan. Records that
-- and, if the row was MISSING (a file re-created at its old path), reactivates
-- it in place -- a MISSING row found alive again is not a version collision
-- and must not stay MISSING.
--
-- The CASE is MISSING-only, not a blanket reactivation, and that's what
-- makes #226's now-possible concurrent FULL_SCAN + manual differential
-- INCREMENTAL against the same Tier-3 location safe rather than merely
-- untested: if a concurrent FULL_SCAN archives this node (a version
-- collision on the same path) between the differential sweep's
-- sweepUnchanged check and its deferred touchBatcher flush, this UPDATE
-- still runs against the now-ARCHIVED row id -- but ARCHIVED falls into the
-- ELSE branch and stays ARCHIVED. The touch is a harmless no-op on
-- lifecycle_state (mtime_unix/last_seen_at/updated_at still advance on a
-- dead row, a minor stale-audit-trail artifact), never a resurrection into
-- a live duplicate alongside the FULL_SCAN's freshly inserted successor
-- row. See TestConcurrentFullScanArchiveDoesNotResurrectViaDifferentialTouch
-- in internal/pipeline for the regression test.
UPDATE media_nodes
SET mtime_unix = ?2, last_seen_at = unixepoch(),
    lifecycle_state = CASE WHEN lifecycle_state = 'MISSING' THEN 'ACTIVE' ELSE lifecycle_state END,
    updated_at = unixepoch()
WHERE id = ?1;

-- name: UpdateMediaNodeFullHash :exec
-- Escalation path for T1: computed lazily, only when fast_hash collides
-- with another live node or the file lives on a TIER3_MASTER_ARCHIVE
-- location (docs/schema.md fix #8's full_hash policy).
UPDATE media_nodes SET full_hash = ?2, indexing_status = 'INDEXED_FULL', updated_at = unixepoch() WHERE id = ?1;

-- name: RefreshMediaNodeAfterInPlaceWrite :exec
-- The metadata-inheritance endpoint (#54) is the first server-initiated
-- filesystem write: it rewrites a child's file in place via exiftool, which
-- changes size_bytes/mtime_unix/fast_hash on disk. Without this update the
-- next scan's commitOne sees a changed fast_hash at the same path and treats
-- it as a version collision -- archiving the node and minting a new
-- node_uuid, which strands every media_edges row (including a human
-- CONFIRMED/REJECTED review decision) on the archived row. Called once,
-- immediately after the write succeeds, so the DB and the file agree before
-- any scan observes the change.
--
-- full_hash is always cleared and INDEXED_FULL is downgraded to
-- INDEXED_SHALLOW: the write changed the file's bytes, so any previously
-- computed BLAKE3 full_hash no longer matches it. Once fast_hash agrees
-- again this row takes commitOne's Touched branch on the next scan, which
-- never recomputes full_hash on its own (needsFullHash escalates based on
-- current tier/collision state, not on the node's prior indexing_status) --
-- so a stale full_hash would otherwise persist forever, masquerading as a
-- verified integrity fingerprint it no longer is (docs/schema.md fix #8).
UPDATE media_nodes
SET size_bytes = ?2, mtime_unix = ?3, fast_hash = ?4,
    full_hash = NULL,
    indexing_status = CASE WHEN indexing_status = 'INDEXED_FULL' THEN 'INDEXED_SHALLOW' ELSE indexing_status END,
    last_seen_at = unixepoch(), updated_at = unixepoch()
WHERE id = ?1;

-- name: UpdateMediaNodePromotedColumns :exec
-- #197: the touched/rebased branches' backfill (reconcileAllMetadata ->
-- reconcilePromotedColumns, internal/pipeline/commit.go) refreshes the
-- promoted EXIF/XMP columns from a freshly-probed Result. Before #188 these
-- columns were repopulated incidentally whenever an in-place metadata write
-- (e.g. inherit-metadata) forced a version collision on the next scan; now
-- that the write refreshes fast_hash instead (RefreshMediaNodeAfterInPlaceWrite),
-- the node takes commitOne's Touched branch and would otherwise keep its
-- insert-time values forever -- including a XMP-xmpMM:DerivedFrom written by
-- inherit-metadata that never reaches media_nodes.derived_from_id.
--
-- captured_at_unix (#204) is included on the same overwrite-on-differ
-- contract as the other six, not fill-only-when-NULL: UpsertMediaEdge's
-- confidence = MAX(excluded, stored) makes re-resolution monotone (it can
-- only upgrade or leave an edge, never downgrade or delete one), so a value
-- that changes here can at worst strand an already-committed Tier-3 edge at
-- its old confidence -- not corrupt it -- while a NULL that never gets
-- promoted is a permanent, not just temporary, blind spot for
-- HeuristicSpatialTemporalResolver. See reconcilePromotedColumns' doc
-- comment for the full reasoning, including why the inherit-metadata path's
-- circular evidence (a child temporally matching the parent because the
-- parent's own timestamp was just copied into it) is benign: every resolver
-- derives Rel from the same inferRelationship, so a Tier-3 candidate always
-- merges into the same (parent, child, rel) group as any stronger Tier-2
-- edge and never creates a second one.
--
-- The caller passes effective values: a column whose fresh probe value was
-- empty or unchanged is passed through as the node's current value, so this
-- query is only ever reached with at least one genuine change. Plain
-- positional params (?1..?8), not sqlc.arg: this file already has earlier
-- queries using bare ?N placeholders (ListTier3Candidates, ListPrunableNodes),
-- and sqlc v1.31.1 mis-numbers/corrupts a later sqlc.arg(name) placeholder in
-- the same file when a bare ?N appears anywhere earlier in it.
UPDATE media_nodes SET
    original_document_id = ?2,
    document_id = ?3,
    derived_from_id = ?4,
    camera_model = ?5,
    camera_serial = ?6,
    lens_model = ?7,
    captured_at_unix = ?8,
    updated_at = unixepoch()
WHERE id = ?1;

-- name: MarkNodeMissing :exec
UPDATE media_nodes SET lifecycle_state = 'MISSING', updated_at = unixepoch() WHERE id = ?1;

-- name: MarkUnseenNodesMissing :execrows
-- Phase 1 (#31): at the end of a clean full scan, every ACTIVE node under the
-- scanned storage location whose last_seen_at predates the scan's start is
-- gone. TouchMediaNode/InsertMediaNode/RebaseMissingNodePath all bump
-- last_seen_at on every node the walk actually saw and committed, so anything
-- still old here was genuinely unseen this scan. KeepActive is the pass's
-- seen-but-uncertain set -- paths the walk saw but did not reliably commit
-- (processFile error, submit refused, dropped result, batch Commit failure) --
-- and is excluded from the sweep: a file on disk with a stale last_seen_at is
-- not proof it's gone. KeepActive paths are passed as a JSON array string
-- to json_each(?3) to remain within SQLite per-statement parameter bounds.
-- Scoped by storage_location_id so a scan of one mount never touches another.
-- unixepoch() is 1s granularity, so a node last seen in a scan that happened
-- to end in the SAME wall-clock second as this scan's start may survive one
-- extra scan -- it is swept the next round, which is delayed-not-wrong.
UPDATE media_nodes
SET lifecycle_state = 'MISSING', updated_at = unixepoch()
WHERE storage_location_id = ?1
  AND lifecycle_state = 'ACTIVE'
  AND last_seen_at < ?2
  AND file_path NOT IN (SELECT value FROM json_each(?3));

-- name: GetMediaNodeByUUID :one
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model, thumb_state, thumb_attempts, source_path_hash
FROM media_nodes
WHERE node_uuid = ?1;

-- name: RebaseNodePathByUUID :exec
UPDATE media_nodes
SET file_path = ?2, file_name = ?3, storage_location_id = ?4,
    lifecycle_state = 'ACTIVE', mtime_unix = ?5,
    last_seen_at = unixepoch(), updated_at = unixepoch()
WHERE node_uuid = ?1;

-- name: ListPrunableNodes :many
-- #61's TTL cache pruning eligibility: a Tier-1 node past its TTL
-- (mtime_unix < cutoff_unix) is only a candidate if a LIVE ancestor on a
-- TIER3_MASTER_ARCHIVE location has a verified (non-NULL, 64-hex) full_hash.
-- "live" matches v_media_edges_resolved's own parent_alive definition --
-- ACTIVE or HIDDEN, deliberately not the looser "!= ARCHIVED" -- so a
-- vanished (MISSING) or archived Tier-3 master can never authorize a purge.
-- Ancestor, not "same full_hash": walks media_edges target->source
-- (REJECTED edges excluded, and each walked node must itself be non-ARCHIVED),
-- mirroring ListAncestors' direction convention and its ARCHIVED-intermediate
-- exclusion exactly -- a chain that only connects through a superseded
-- version doesn't represent the file currently on disk.
-- Tier-1-only and prunable-only are already schema-enforced
-- (00001_init.sql's CHECK (tier = 'TIER1_LOCAL_SCRATCH' OR prunable = 0)) --
-- not re-checked here; the caller only invokes this against a location it
-- already knows is prunable.
--
-- Every column in each anchor SELECT is explicitly named/aliased -- sqlc's
-- SQLite parser fails with `*ast.ResTarget has nil name` otherwise (see
-- docs/schema.md's sqlc risk note). "64-hex" is enforced here as
-- length(full_hash) = 64 only, matching the schema's own CHECK exactly
-- (docs/schema.md) -- sqlc's SQLite grammar does not support GLOB
-- ("no viable alternative at input 'GLOB'"), and full_hash is only ever
-- written by internal/hashing.FullHash, which always emits lowercase hex.
-- Plain positional params (?1/?2), not sqlc.arg: this file already has
-- earlier queries (e.g. ListTier3Candidates) using bare ?N placeholders,
-- and sqlc v1.31.1 mis-numbers/corrupts a later sqlc.arg(name) placeholder
-- in the same file when a bare ?N appears anywhere earlier in it --
-- reproduced by bisection, a real generator bug for this sqlc version, not
-- something wrong with sqlc.arg's own syntax elsewhere in the codebase.
WITH RECURSIVE lineage(root, id) AS (
    SELECT n.id AS root, n.id AS id
    FROM media_nodes n
    WHERE n.storage_location_id = ?1
      AND n.lifecycle_state = 'ACTIVE'
      AND n.mtime_unix < ?2
    UNION
    SELECT l.root AS root, e.source_node_id AS id
    FROM media_edges e
    JOIN lineage l ON e.target_node_id = l.id
    JOIN media_nodes a ON a.id = e.source_node_id
    WHERE e.review_state <> 'REJECTED'
      AND a.lifecycle_state <> 'ARCHIVED'
)
SELECT n.id, n.node_uuid, n.file_path, n.file_name, n.size_bytes,
       n.mtime_unix, n.storage_location_id
FROM media_nodes n
WHERE n.storage_location_id = ?1
  AND n.lifecycle_state = 'ACTIVE'
  AND n.mtime_unix < ?2
  AND EXISTS (
    SELECT 1
    FROM lineage l
    JOIN media_nodes m ON m.id = l.id
    JOIN storage_locations s ON s.id = m.storage_location_id
    WHERE l.root = n.id
      AND l.id <> n.id
      AND s.tier = 'TIER3_MASTER_ARCHIVE'
      AND m.lifecycle_state IN ('ACTIVE', 'HIDDEN')
      AND m.full_hash IS NOT NULL
      AND length(m.full_hash) = 64
  )
ORDER BY n.id;

-- name: ListPendingThumbnails :many
-- internal/thumbs.Worker's claim query: up to ?2 PENDING nodes oldest-first
-- (by id, this codebase's usual FIFO tiebreak), backed by 00007's partial
-- index idx_media_nodes_thumb_pending (WHERE thumb_state = 'PENDING'), so
-- the index stays cheap as the backlog drains rather than scanning every
-- row regardless of state. thumb_attempts < ?1 excludes a node that has
-- already exhausted the worker's retry bound -- a permanently-broken
-- source (corrupt file, unsupported format that errors rather than
-- returning thumbs.ErrUnsupported) stops being retried every pass forever,
-- mirroring remote_sync_state's retry_count bound
-- (ResetRemoteSyncStateFailed). lifecycle_state excludes MISSING/ARCHIVED:
-- there is no live file left to read a thumbnail from.
--
-- Joined against storage_locations to exclude TIER0_LOCAL_STAGING (#231):
-- workstation-local staging a future offline-ingest agent may record a node
-- for before its bytes have synced anywhere server-visible. Without this,
-- the worker claims a node whose file the server can never open, fails,
-- and burns a retry (and real read I/O against whatever remote/NFS tier it
-- probes) every pass until thumb_attempts hits its bound -- self-limiting
-- but noisy, and never productive. No other tier is excluded here: Tier 1
-- scratch, Tier 2 exports, Tier 3 masters, and PROJECTS are all
-- server-readable.
--
-- Plain positional params (?1, ?2), not sqlc.arg: this file already has
-- earlier queries using bare ?N placeholders (ListTier3Candidates,
-- ListPrunableNodes, UpdateMediaNodePromotedColumns), and sqlc v1.31.1
-- mis-numbers/corrupts a later sqlc.arg(name) placeholder in the same file
-- when a bare ?N appears anywhere earlier in it.
SELECT n.id, n.node_uuid, n.file_path, n.thumb_attempts
FROM media_nodes n
JOIN storage_locations s ON s.id = n.storage_location_id
WHERE n.thumb_state = 'PENDING'
  AND n.lifecycle_state IN ('ACTIVE', 'HIDDEN')
  AND n.thumb_attempts < ?1
  AND s.tier <> 'TIER0_LOCAL_STAGING'
ORDER BY n.id
LIMIT ?2;

-- name: SetThumbState :exec
-- The caller passes the effective thumb_attempts value itself (0 on a
-- successful READY -- a later invalidation starts the attempt count fresh
-- again; current+1 on a FAILED retry) rather than this query
-- incrementing/resetting on its own, so one statement serves every
-- transition -- same "caller passes effective values" contract as
-- UpdateMediaNodePromotedColumns above.
UPDATE media_nodes SET thumb_state = ?2, thumb_attempts = ?3, updated_at = unixepoch() WHERE id = ?1;

-- name: InvalidateThumbnail :exec
-- Resets a node's thumbnail generation state to PENDING with attempts
-- zeroed, so internal/thumbs.Worker regenerates it on its next pass. The
-- Cache.Write path is os.CreateTemp + os.Rename to the same node_uuid path,
-- so regeneration overwrites the stale file atomically -- no separate
-- Cache.Delete is needed alongside this reset. Called from
-- internal/httpapi's refreshNodeAfterInPlaceWrite, not from
-- internal/pipeline: fast_hash is by construction unchanged on both the
-- Touched branch (its own entry condition) and the rebase branch (it looks
-- the node up BY fast_hash), so neither ever observes a fast_hash change --
-- refreshNodeAfterInPlaceWrite is the one place a node's fast_hash changes
-- while its node_uuid is preserved (the inherit-metadata endpoint's
-- post-write DB sync, #188).
UPDATE media_nodes SET thumb_state = 'PENDING', thumb_attempts = 0, updated_at = unixepoch() WHERE id = ?1;

-- name: GetMediaNodeByFullHash :one
-- Strict dedup: find an active or hidden node with the given BLAKE3 full_hash.
-- Excludes ARCHIVED and MISSING nodes so re-ingesting removed content creates a fresh node.
SELECT id, node_uuid, file_path, lifecycle_state, indexing_status, size_bytes
FROM media_nodes
WHERE full_hash = ?1
  AND lifecycle_state IN ('ACTIVE', 'HIDDEN')
LIMIT 1;

-- name: GetMediaNodeByFastHash :one
SELECT id
FROM media_nodes
WHERE fast_hash = ?1
  AND lifecycle_state IN ('ACTIVE', 'HIDDEN')
LIMIT 1;

-- name: GetMediaNodeBySourcePathHash :one
-- Agent dedup: find an active or hidden node with the given SHA-256 source_path_hash.
-- Excludes ARCHIVED and MISSING nodes so re-ingesting removed content creates a fresh node.
SELECT id, node_uuid, file_path, lifecycle_state, indexing_status
FROM media_nodes
WHERE source_path_hash = ?1
  AND lifecycle_state IN ('ACTIVE', 'HIDDEN')
ORDER BY id DESC
LIMIT 1;
