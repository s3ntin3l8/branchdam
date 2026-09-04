-- +goose Up
-- Issue #132: internal/graph.FilenameStemResolver now caps an index-marker
-- ("-N"/"(N)") derived filename_stem match strictly below Tier 2's
-- AUTO_ACCEPTED threshold, requiring a bare anchor file to even emit a
-- candidate (see resolvers.go's FilenameStemResolver doc comment). That
-- fix changes what a FUTURE resolve pass computes; it does not by itself
-- touch rows already written. UpsertMediaEdge's ON CONFLICT DO UPDATE SET
-- confidence = MAX(excluded.confidence, media_edges.confidence)
-- (media_edges_resolve.sql) means confidence -- and therefore review_state
-- -- never regresses on a later, weaker resolve pass BY DESIGN (that's
-- what makes a human CONFIRMED/REJECTED decision permanent). So an
-- AUTO_ACCEPTED mesh edge written before this migration, e.g. from a
-- "trip-1.jpg".."trip-45.jpg" batch under the pre-#132 unbounded
-- collapse, would otherwise survive every future rescan untouched. This
-- migration is the one-time correction for data already on disk.
--
-- The predicate below identifies exactly the index-suffix case without
-- GLOB (sqlc's SQLite grammar rejects it -- see AGENTS.md's sqlc risk
-- notes) and without LIKE (filename_stem routinely contains literal '_',
-- LIKE's own single-character wildcard, which would make an unescaped
-- LIKE pattern match unintended rows): an index marker ("-N", "(N)") is
-- the only stripped suffix that begins with '-' or '('; role markers
-- (_edit, _proxy, _vN, " copy") begin with '_' or a space. So "the
-- character immediately after the stored stem, in the lowercased
-- filename, is '-' or '('" is a sufficient test for "this node's
-- filename_stem came from stripping an index marker," for the common
-- single-marker case (it is NOT complete for every multi-marker
-- filename -- see the documented miss below) -- verified by hand against
-- every internal/naming.TestStem single-marker case: photo-2.jpg and
-- IMG_0001(1).jpg match; DSC-0001.JPG, render_v1_proxy.jpg,
-- "IMG_0001 copy.jpg", and p_proxy.mov do not.
--
-- Only resolver = 'filename_stem' rows are touched -- a filename_stem
-- match that was also corroborated by a stronger resolver (e.g.
-- xmp_original_document_id) already won mergeCandidates' max and is
-- stamped with THAT resolver's name, not 'filename_stem', so it is
-- correctly left alone here; that is exactly the corroboration path
-- issue #132's criterion 2 asks for.
--
-- Only review_state = 'AUTO_ACCEPTED' rows are touched -- CONFIRMED and
-- REJECTED are human decisions and must never be touched by anything
-- other than the confirm/reject endpoints, migrations included.
--
-- One known, conservative miss (documented, not fixed here): this
-- character-adjacency test only sees the FINAL stem, not the order
-- markers were stripped in. "photo_edit-2.jpg" strips its index marker
-- ("-2") before its role marker ("_edit") in naming.Analyze -- ending on
-- stem "photo", correctly classified SuffixIndex on the Go side -- but the
-- character immediately after "photo" in the original filename is '_'
-- (from the role marker, stripped last), not '-', so this migration's
-- predicate does not flag that row. Left AUTO_ACCEPTED here; the Go-side
-- gate (internal/graph/resolvers.go) handles it correctly for all FUTURE
-- resolves regardless, since it works from the classification, not from
-- string adjacency. This migration only corrects the common,
-- mechanically-detectable case in already-written data.
UPDATE media_edges
SET confidence   = MIN(confidence, 0.89),
    review_state = 'NEEDS_REVIEW',
    updated_at   = unixepoch()
WHERE review_state = 'AUTO_ACCEPTED'
  AND resolver = 'filename_stem'
  AND EXISTS (
      SELECT 1 FROM media_nodes n
      WHERE n.id IN (media_edges.source_node_id, media_edges.target_node_id)
        AND n.filename_stem IS NOT NULL
        AND length(n.filename_stem) > 0
        AND substr(lower(n.file_name), 1, length(n.filename_stem)) = n.filename_stem
        AND substr(lower(n.file_name), length(n.filename_stem) + 1, 1) IN ('-', '(')
  );

-- Correct graph_status for nodes whose only AUTO_ACCEPTED/CONFIRMED
-- parent edge was just downgraded above. Deliberately narrow: the UPDATE
-- above can only ever produce a LINKED -> NEEDS_REVIEW transition (it
-- never deletes edges, never creates one, and never touches a REJECTED or
-- CONFIRMED row), so this scopes to exactly that transition rather than
-- generally re-deriving graph_status the way
-- graph.RecomputeStatusFromPersistedEdges does (internal/graph/status.go).
-- That function's precedence can also emit UNLINKED (no live edges at
-- all) -- correct for its own callers, but WRONG here: 00001_init.sql's
-- CHECK also permits 'ROOT' as a graph_status value, which no code path
-- writes today (confirmed by grep) but which the schema still allows, and
-- RecomputeStatusFromPersistedEdges' logic has no ROOT case -- an
-- unscoped recompute over every node would silently rewrite any ROOT node
-- to UNLINKED. Scoping this migration to the one transition it can
-- actually cause avoids that trap entirely, rather than relying on
-- "nothing sets ROOT yet" staying true forever.
UPDATE media_nodes
SET graph_status = 'NEEDS_REVIEW', updated_at = unixepoch()
WHERE graph_status = 'LINKED'
  AND EXISTS (
      SELECT 1 FROM media_edges e
      WHERE e.target_node_id = media_nodes.id AND e.review_state = 'NEEDS_REVIEW'
  )
  AND NOT EXISTS (
      SELECT 1 FROM media_edges e
      WHERE e.target_node_id = media_nodes.id
        AND e.review_state IN ('AUTO_ACCEPTED', 'CONFIRMED')
  );

-- +goose Down
-- Deliberate no-op: the pre-migration confidence/review_state values this
-- UPDATE overwrote are not reconstructible (MIN/downgrade is lossy by
-- design), so there is nothing correct to restore. TestMigrateUpDownUp
-- (internal/db/open_test.go) only requires Down to succeed, not to be a
-- true inverse of every migration.
SELECT 1;
