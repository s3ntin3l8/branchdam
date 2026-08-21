package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/naming"
	"github.com/s3ntin3l8/branchdam/internal/probe"
)

// Commit applies results to media_nodes inside a single write transaction
// (db.InTx -- the writer pool's one connection). Every Result must already
// carry its final FastHash/FullHash: Commit does no file I/O and no hash
// computation of its own, only reads and writes -- see result.go's Result
// doc for why that split matters.
//
// Per-result behavior:
//   - A live node already exists at this path with the SAME fast_hash:
//     TouchMediaNode (same content, seen again).
//   - A live node already exists at this path with a DIFFERENT fast_hash:
//     version collision (docs/schema.md fix #3) -- archive the old row,
//     insert a new one, link superseded_by. Archiving happens BEFORE
//     inserting, never after: the partial unique index on file_path
//     (WHERE lifecycle_state != 'ARCHIVED') means a live old row and a live
//     new row can never coexist even for an instant within the transaction.
//   - No live node at this path, but a MISSING node elsewhere shares this
//     fast_hash: Pillar 5 move detection -- rebase that node's path in
//     place. Its id/node_uuid never change, so every edge referencing it
//     survives untouched.
//   - No live node at this path and no MISSING match: brand new node.
//
// What this function deliberately does NOT do: merge two nodes just
// because they share a fast_hash while both are live at different paths.
// That's T1 (spec 9.5) -- a fast_hash collision between two genuinely
// different files must not merge them, and Commit never even looks at
// fast_hash for that case, only at file_path (version collision) and
// MISSING-node fast_hash matches (move detection). The decision to escalate
// to full_hash when a live collision is suspected happens before Commit is
// ever called -- see scan.go's needsFullHash.
func Commit(ctx context.Context, database *db.DB, locationID int64, results []Result, loggers ...*slog.Logger) (Stats, error) {
	log := slog.New(slog.DiscardHandler)
	if len(loggers) > 0 && loggers[0] != nil {
		log = loggers[0]
	}
	var stats Stats
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		for i := range results {
			if err := commitOne(ctx, q, locationID, results[i], &stats, log); err != nil {
				return fmt.Errorf("commit %q: %w", results[i].Path, err)
			}
		}
		return nil
	})
	return stats, err
}

func commitOne(ctx context.Context, q *sqlcgen.Queries, locationID int64, r Result, stats *Stats, log *slog.Logger) error {
	existing, err := q.GetLiveNodeByPath(ctx, r.Path)
	switch {
	case err == nil:
		if existing.FastHash != nil && *existing.FastHash == r.FastHash {
			stats.Touched++
			if err := q.TouchMediaNode(ctx, sqlcgen.TouchMediaNodeParams{
				ID:        existing.ID,
				MtimeUnix: r.ModTime.Unix(),
			}); err != nil {
				return err
			}
			// A node seen unchanged still backfills metadata: one indexed
			// while exiftool/ffprobe were absent from PATH would otherwise
			// stay permanently metadata-less, since its fast_hash never
			// changes and it always takes this branch (see #86).
			// reconcileAllMetadata (not persistAllMetadata) here: this branch
			// runs on EVERY unchanged file on EVERY scan pass, so skipping the
			// write when nothing actually changed matters (see #105). Since
			// #197 it also refreshes the promoted columns the same diff-first
			// way -- the branch is what keeps a freshly-written
			// XMP-xmpMM:DerivedFrom from getting stuck invisible in the DB.
			return reconcileAllMetadata(ctx, q, existing, r, stats, log)
		}
		return commitVersionCollision(ctx, q, locationID, existing, r, stats, log)

	case errors.Is(err, sql.ErrNoRows):
		return commitNoLiveNode(ctx, q, locationID, r, stats, log)

	default:
		return fmt.Errorf("get live node by path: %w", err)
	}
}

func commitVersionCollision(ctx context.Context, q *sqlcgen.Queries, locationID int64, existing sqlcgen.MediaNode, r Result, stats *Stats, log *slog.Logger) error {
	// Archive first -- see Commit's doc comment for why the order matters.
	if err := q.ArchiveMediaNode(ctx, existing.ID); err != nil {
		return fmt.Errorf("archive superseded node: %w", err)
	}
	newNode, err := insertNewNode(ctx, q, locationID, r, log)
	if err != nil {
		return fmt.Errorf("insert successor node: %w", err)
	}
	if err := q.SetSupersededBy(ctx, sqlcgen.SetSupersededByParams{
		ID:           existing.ID,
		SupersededBy: sql.NullInt64{Int64: newNode.ID, Valid: true},
	}); err != nil {
		return fmt.Errorf("link superseded_by: %w", err)
	}
	stats.VersionCollisions++
	return nil
}

func commitNoLiveNode(ctx context.Context, q *sqlcgen.Queries, locationID int64, r Result, stats *Stats, log *slog.Logger) error {
	if r.FastHash != "" {
		missing, err := q.GetMissingNodeByFastHash(ctx, &r.FastHash)
		if err == nil {
			stats.Moved++
			if err := q.RebaseMissingNodePath(ctx, sqlcgen.RebaseMissingNodePathParams{
				ID:                missing.ID,
				FilePath:          r.Path,
				FileName:          r.FileName,
				StorageLocationID: locationID,
				MtimeUnix:         r.ModTime.Unix(),
			}); err != nil {
				return err
			}
			// A rebased node backfills metadata the same way a touched one
			// does -- see the touched branch above, #86, and #105.
			return reconcileAllMetadata(ctx, q, missing, r, stats, log)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get missing node by fast_hash: %w", err)
		}
	}

	stats.Inserted++
	_, err := insertNewNode(ctx, q, locationID, r, log)
	return err
}

func insertNewNode(ctx context.Context, q *sqlcgen.Queries, locationID int64, r Result, log *slog.Logger) (sqlcgen.MediaNode, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("mint node_uuid: %w", err)
	}

	indexingStatus := "INDEXED_SHALLOW"
	var fullHash *string
	if r.FullHash != "" {
		indexingStatus = "INDEXED_FULL"
		fullHash = &r.FullHash
	}

	params := sqlcgen.InsertMediaNodeParams{
		NodeUuid:           id.String(),
		StorageLocationID:  locationID,
		FilePath:           r.Path,
		FileName:           r.FileName,
		FileExt:            r.FileExt,
		SizeBytes:          r.Size,
		MtimeUnix:          r.ModTime.Unix(),
		FastHash:           &r.FastHash,
		FullHash:           fullHash,
		IndexingStatus:     indexingStatus,
		GraphStatus:        "UNLINKED",
		LifecycleState:     "ACTIVE",
		OriginalDocumentID: nullString(r.OriginalDocumentID),
		DocumentID:         nullString(r.DocumentID),
		DerivedFromID:      nullString(r.DerivedFromID),
		CameraModel:        nullString(r.CameraModel),
		CameraSerial:       nullString(r.SerialNumber),
		LensModel:          nullString(r.LensModel),
		FilenameStem:       nullString(naming.Stem(r.FileName)),
	}
	if r.PHash != nil {
		params.Phash = sql.NullInt64{Int64: *r.PHash, Valid: true}
	}
	if r.CapturedAt != nil {
		params.CapturedAtUnix = sql.NullInt64{Int64: r.CapturedAt.Unix(), Valid: true}
	}

	node, err := q.InsertMediaNode(ctx, params)
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("insert media node: %w", err)
	}
	if err := persistAllMetadata(ctx, q, node.ID, r, log); err != nil {
		return sqlcgen.MediaNode{}, err
	}
	return node, nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

const metadataCap = 64 // per-node metadata row cap -- overflow is logged, never fatal

// cappedSortedKeys returns kv's keys in sorted order, truncated to maxRows.
// Sorting first makes truncation deterministic; logging the drop at DEBUG
// (never an error) means one over-tagged file can't fail a whole scan.
// Shared by persistMetadata and reconcileMetadata so the cap is applied
// identically regardless of which write path is used -- see reconcileMetadata's
// doc comment for why the cap must be applied BEFORE any unchanged-value diff.
func cappedSortedKeys(kv map[string]string, maxRows int, nodeID int64, source string, log *slog.Logger) []string {
	if len(kv) == 0 || maxRows <= 0 {
		return nil
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxRows {
		log.Debug("pipeline: node_metadata overflow dropped", "nodeID", nodeID, "source", source, "dropped", len(keys)-maxRows)
		keys = keys[:maxRows]
	}
	return keys
}

// persistMetadata writes kv as node_metadata rows inside the caller's write
// transaction, in sorted-key order so cap truncation is deterministic.
// Overflow past maxRows is logged at DEBUG and dropped, never an error --
// one over-tagged file must not fail a whole scan. That "logged and dropped"
// guarantee covers cap overflow only: a hard insert error still aborts the
// enclosing Commit transaction, so node and metadata land together or not at
// all. Used where the node is known brand new (insertNewNode and
// commitVersionCollision's successor) -- there every row is unconditionally
// new, so reconcileMetadata's pre-read-and-diff below would be pure overhead.
// The inherit-metadata backfill (PersistExifMetadata, #157) is an intentional
// additional caller on an EXISTING node: a single on-demand request, where
// the idempotent upsert is negligible cost next to the reconcile pre-read
// and diff the per-scan touch/rebase path pays.
func persistMetadata(ctx context.Context, q *sqlcgen.Queries, nodeID int64, source string, kv map[string]string, maxRows int, log *slog.Logger) error {
	for _, k := range cappedSortedKeys(kv, maxRows, nodeID, source, log) {
		if err := q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
			NodeID: nodeID, Source: source, Key: k, Value: kv[k],
		}); err != nil {
			return fmt.Errorf("insert node_metadata %s/%s: %w", source, k, err)
		}
	}
	return nil
}

// persistAllMetadata writes both r's exiftool and ffprobe metadata against
// nodeID unconditionally. Only for the two call sites where nodeID is known
// brand new (insertNewNode, commitVersionCollision's successor) -- for a
// touched/rebased node (see #86), use reconcileAllMetadata instead so an
// unchanged file doesn't rewrite every row on every scan pass (#105).
func persistAllMetadata(ctx context.Context, q *sqlcgen.Queries, nodeID int64, r Result, log *slog.Logger) error {
	if err := persistMetadata(ctx, q, nodeID, "exiftool", exifMetadata(r), metadataCap, log); err != nil {
		return err
	}
	return persistMetadata(ctx, q, nodeID, "ffprobe", ffprobeMetadata(r), metadataCap, log)
}

// reconcileMetadata writes only the rows in kv (after the same sort+cap
// persistMetadata applies) whose value differs from -- or is absent from --
// prior. prior is keyed by (source, key) and covers the node's full existing
// row set, read once by reconcileAllMetadata before either source is
// reconciled. The cap MUST be applied before this diff, not after: a key
// truncated past metadataCap must never be compared against prior, or a
// large, stable metadata set would spuriously "change" every pass as its
// tail keys sort in and out of the capped window. Returns the number of rows
// actually upserted, for Stats.MetadataWritten.
func reconcileMetadata(ctx context.Context, q *sqlcgen.Queries, nodeID int64, source string, kv map[string]string, maxRows int, prior map[metadataRowKey]string, log *slog.Logger) (int, error) {
	written := 0
	for _, k := range cappedSortedKeys(kv, maxRows, nodeID, source, log) {
		v := kv[k]
		if old, ok := prior[metadataRowKey{source, k}]; ok && old == v {
			continue // unchanged -- see #105, skip the redundant write
		}
		if err := q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
			NodeID: nodeID, Source: source, Key: k, Value: v,
		}); err != nil {
			return written, fmt.Errorf("insert node_metadata %s/%s: %w", source, k, err)
		}
		written++
	}
	return written, nil
}

// metadataRowKey is node_metadata's natural key minus node_id (the read in
// reconcileAllMetadata is already scoped to one node_id).
type metadataRowKey struct {
	source string
	key    string
}

// reconcileAllMetadata is persistAllMetadata's counterpart for a node that
// might already have metadata: the touched branch (commitOne) and the
// rebase/move branch (commitNoLiveNode, watcher.rebaseIfMoved), both #86's
// backfill paths. It pre-reads the node's existing rows in the same write
// transaction and only upserts rows that are new or genuinely changed --
// InsertNodeMetadata's ON CONFLICT DO UPDATE keeps a naive persistAllMetadata
// call correct here too, just wastefully so: every unchanged file on every
// scan pass was otherwise rewriting its full metadata set (#105).
//
// Since #197 it also refreshes media_nodes' promoted EXIF/XMP columns
// (camera_model, lens_model, camera_serial, original_document_id, document_id,
// derived_from_id) from the freshly-probed Result, with the same diff-first,
// non-empty-values-only contract as the node_metadata reconcile. This is what
// makes #188 safe: an in-place metadata write (inherit-metadata) now refreshes
// fast_hash so the node touches instead of version-colliding, and the touch
// repopulates the columns -- including a XMP-xmpMM:DerivedFrom that, before
// #188, only reached media_nodes.derived_from_id because the collision happened
// to re-run insertNewNode.
//
// stats may be nil (watcher.rebaseIfMoved has no *Stats in scope, being
// called from inside an InTx closure returning bare error) -- in that case
// the written counts are logged at DEBUG instead of accumulated.
func reconcileAllMetadata(ctx context.Context, q *sqlcgen.Queries, node sqlcgen.MediaNode, r Result, stats *Stats, log *slog.Logger) error {
	exif := exifMetadata(r)
	ffprobe := ffprobeMetadata(r)
	if len(exif) > 0 || len(ffprobe) > 0 {
		// Only spend a ListNodeMetadata read on the writer connection when
		// the probe actually derived rows to diff against. The pre-#105
		// persistAllMetadata path made zero DB calls when neither tool
		// produced anything (cappedSortedKeys' len(kv)==0 short-circuit);
		// reconcileAllMetadata must preserve that. A node that derives
		// nothing on this pass -- exiftool/ffprobe absent from PATH, or a
		// genuinely probe-less file -- skips the read and simply keeps its
		// existing rows untouched.
		existing, err := q.ListNodeMetadata(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("list node_metadata for reconcile: %w", err)
		}
		prior := make(map[metadataRowKey]string, len(existing))
		for _, row := range existing {
			prior[metadataRowKey{row.Source, row.Key}] = row.Value
		}

		written, err := reconcileMetadata(ctx, q, node.ID, "exiftool", exif, metadataCap, prior, log)
		if err != nil {
			return err
		}
		n, err := reconcileMetadata(ctx, q, node.ID, "ffprobe", ffprobe, metadataCap, prior, log)
		if err != nil {
			return err
		}
		written += n

		if stats != nil {
			stats.MetadataWritten += written
		} else if written > 0 {
			log.Debug("pipeline: node_metadata reconciled", "nodeID", node.ID, "written", written)
		}
	}

	// Promoted-column reconcile runs even when r derives no free-form rows: a
	// file whose only EXIF field is a promoted one (e.g. Model) still needs
	// its media_nodes column refreshed -- and when the probe is absent, the
	// non-empty-only rule inside reconcilePromotedColumns makes this a no-op.
	if refreshed, err := reconcilePromotedColumns(ctx, q, node, r); err != nil {
		return err
	} else if refreshed {
		if stats != nil {
			stats.PromotedColumnsRefreshed++
		} else {
			log.Debug("pipeline: media_nodes promoted columns reconciled", "nodeID", node.ID)
		}
	}
	return nil
}

// reconcilePromotedColumns refreshes media_nodes' promoted EXIF/XMP columns
// (camera_model, lens_model, camera_serial, original_document_id, document_id,
// derived_from_id) from a freshly-probed Result, on the touched/rebased
// branches that never re-run insertNewNode (#197). captured_at_unix is
// deliberately NOT among them: re-promoting it on an existing node interacts
// with HeuristicSpatialTemporalResolver's candidate matching (a node whose
// captured_at_unix changes after other nodes already resolved edges against
// it), which needs reasoning through before any write path does it -- tracked
// as #204.
//
// Only a NON-EMPTY fresh value may overwrite a column, and only when it
// differs from what's stored: a probe that ran but genuinely found no value
// for a tag -- or a probe that never ran at all (exiftool absent from PATH) --
// must not clear a column. This is the same one-directional contract
// reconcileMetadata applies to node_metadata (never delete, only add/update
// changed values), so an absent probe degrades to a no-op rather than a
// destructive wipe. Returns true when an UPDATE was actually issued.
func reconcilePromotedColumns(ctx context.Context, q *sqlcgen.Queries, node sqlcgen.MediaNode, r Result) (bool, error) {
	params := sqlcgen.UpdateMediaNodePromotedColumnsParams{ID: node.ID}
	changed := false
	assign := func(fresh string, stored sql.NullString, target *sql.NullString) {
		if fresh != "" && stored.String != fresh {
			*target = sql.NullString{String: fresh, Valid: true}
			changed = true
		} else {
			*target = stored
		}
	}
	assign(r.OriginalDocumentID, node.OriginalDocumentID, &params.OriginalDocumentID)
	assign(r.DocumentID, node.DocumentID, &params.DocumentID)
	assign(r.DerivedFromID, node.DerivedFromID, &params.DerivedFromID)
	assign(r.CameraModel, node.CameraModel, &params.CameraModel)
	assign(r.SerialNumber, node.CameraSerial, &params.CameraSerial)
	assign(r.LensModel, node.LensModel, &params.LensModel)
	if !changed {
		return false, nil
	}
	if err := q.UpdateMediaNodePromotedColumns(ctx, params); err != nil {
		return false, fmt.Errorf("refresh media_nodes promoted columns: %w", err)
	}
	return true, nil
}

// exifMetadata assembles the source='exiftool' rows for a fresh node: the
// typed fields (grouped tag names) plus the allowlisted Raw subset.
func exifMetadata(r Result) map[string]string {
	kv := make(map[string]string, len(r.ExifRaw)+5)
	if r.Make != "" {
		kv["EXIF:Make"] = r.Make
	}
	if r.LensModel != "" {
		kv["EXIF:LensModel"] = r.LensModel
	}
	if r.SerialNumber != "" {
		kv["EXIF:SerialNumber"] = r.SerialNumber
	}
	if r.GPSLatitude != nil {
		kv["Composite:GPSLatitude"] = strconv.FormatFloat(*r.GPSLatitude, 'f', -1, 64)
	}
	if r.GPSLongitude != nil {
		kv["Composite:GPSLongitude"] = strconv.FormatFloat(*r.GPSLongitude, 'f', -1, 64)
	}
	// The allowlist is re-applied here (not only in selectExifRaw) so the
	// unit test can bypass selectExifRaw by feeding ExifRaw directly, and so
	// hand-built Results are filtered too -- defense-in-depth so the two
	// filters can't drift.
	for k, v := range r.ExifRaw {
		if exifRawAllowlist[k] {
			kv[k] = v
		}
	}
	return kv
}

// ffprobeMetadata assembles the source='ffprobe' rows for a fresh node.
// Structured fields only -- size_bytes is already a column, and RawJSON is
// out of scope by design (#34).
func ffprobeMetadata(r Result) map[string]string {
	f := r.FFProbe
	if f == nil {
		return nil
	}
	kv := make(map[string]string, 6)
	if f.FormatName != "" {
		kv["format_name"] = f.FormatName
	}
	if f.DurationSeconds != nil {
		kv["duration_seconds"] = strconv.FormatFloat(*f.DurationSeconds, 'f', -1, 64)
	}
	if f.VideoCodec != "" {
		kv["video_codec"] = f.VideoCodec
	}
	if f.AudioCodec != "" {
		kv["audio_codec"] = f.AudioCodec
	}
	if f.Width > 0 {
		kv["width"] = strconv.Itoa(f.Width)
	}
	if f.Height > 0 {
		kv["height"] = strconv.Itoa(f.Height)
	}
	return kv
}

// PersistExifMetadata upserts a node's exiftool-derived metadata rows from a
// probe.ExifResult, reusing the scan's own allowlist + row cap. Used by the
// inherit-metadata endpoint (#54) so node_metadata stays consistent with a
// file that was just rewritten in place -- otherwise the DB metadata store
// and a second inheritance would re-plan from stale (empty) values until the
// next scan. The caller owns bounding ctx (the httpapi handler wraps the
// exiftool re-read in inheritWriteTimeout): this function does no deadline
// of its own, matching probe.Exif's contract.
//
// Deliberately does NOT touch media_nodes' promoted columns (captured_at_unix,
// camera_model, camera_serial, lens_model). The scan's touched/rebased
// backfill refreshes the non-captured promoted columns from a freshly-probed
// file (camera_model, camera_serial, lens_model, original_document_id,
// document_id, derived_from_id -- #197, reconcileAllMetadata ->
// reconcilePromotedColumns); captured_at_unix is deliberately not among them,
// and this on-demand endpoint re-promotes nothing at all. The practical
// effect: after an inherit-metadata call, a child's node_metadata can carry
// an inherited EXIF:DateTimeOriginal while captured_at_unix stays NULL, so
// HeuristicSpatialTemporalResolver (which queries captured_at_unix via
// ix_media_nodes_camera_time) won't see it. loadTagSet is unaffected (it
// prefers node_metadata), so this is a divergence between the two metadata
// stores, not a live bug in the inheritance endpoint itself. Whether this
// write path -- or any -- should re-promote captured_at_unix is a broader
// question than this function answers alone -- tracked separately as #204,
// not fixed here.
func PersistExifMetadata(ctx context.Context, database *db.DB, nodeID int64, exif *probe.ExifResult, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if exif == nil {
		return nil
	}
	r := Result{
		Make:         exif.Make,
		LensModel:    exif.LensModel,
		SerialNumber: exif.SerialNumber,
		GPSLatitude:  exif.GPSLatitude,
		GPSLongitude: exif.GPSLongitude,
		ExifRaw:      selectExifRaw(exif.Raw),
	}
	return database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return persistMetadata(ctx, q, nodeID, "exiftool", exifMetadata(r), metadataCap, log)
	})
}
