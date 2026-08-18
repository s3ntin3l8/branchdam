package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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
			// write when nothing actually changed matters (see #105).
			return reconcileAllMetadata(ctx, q, existing.ID, r, stats, log)
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
			return reconcileAllMetadata(ctx, q, missing.ID, r, stats, log)
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
		FilenameStem:       nullString(filenameStem(r.FileName)),
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

// versionSuffixRe strips the common "this is a derivative of that other
// file" suffixes the Tier-2 filenameStem resolver (PR 7) matches on:
// _edit, -2, _proxy, _v3, " copy", "(1)". Computed here (at insert time,
// when file_name is already known) rather than deferred to PR 7, so
// filename_stem never needs a backfill migration once resolvers exist.
//
// The "-N" branch is deliberately bounded to 1-2 digits (`-\d{1,2}`), not
// unbounded (`-\d+`, this regex's pre-fix form). Unbounded, it also matched
// a camera's own hyphen-numbered default filenames -- Sony's DSC-0001.JPG,
// IMG-1234.jpg -- stripping every file in a shoot down to the same bare
// "dsc"/"img" stem. FilenameStemResolver (internal/graph/resolvers.go)
// treats a shared stem as a lineage signal (base confidence 0.60, +0.15
// same day, +0.10 same camera, +0.10 same directory, clamped to 0.90 --
// exactly the Tier-2 auto-accept threshold), so a single memory-card
// import produced an O(n^2) fully-connected, auto-accepted DERIVED_FROM
// mesh with no operator ever seeing it in the audit queue. 1-2 digits still
// covers the intended case -- an OS's own "file-2.jpg", "file-12.jpg"
// duplicate-numbering never runs past two digits -- while leaving a
// camera's 3+ digit serial numbering (0001, 1234, ...) as part of the stem,
// where it belongs.
//
// This narrows the bug's trigger window, it does not close the bug class:
// a camera or human numbering scheme that DOES stay within 1-2 digits
// (unpadded counters, "trip-1.jpg".."trip-45.jpg") still collapses to one
// shared stem and can reproduce the same auto-accepted mesh. Closing that
// residual case needs a different signal than digit-count bounding --
// filed as a fast-follow, see the versionSuffixRe test cases for the
// specific inputs this still doesn't cover.
var versionSuffixRe = regexp.MustCompile(`(?i)(_edit|_proxy|_v\d+|-\d{1,2}| copy|\(\d+\))+$`)

func filenameStem(fileName string) string {
	stem := fileName
	if i := strings.LastIndex(stem, "."); i > 0 {
		stem = stem[:i]
	}
	stem = strings.ToLower(strings.TrimSpace(stem))
	for {
		stripped := versionSuffixRe.ReplaceAllString(stem, "")
		if stripped == stem {
			break
		}
		stem = stripped
	}
	return stem
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
// all. Used only where the node is known brand new (insertNewNode and
// commitVersionCollision's successor) -- there every row is unconditionally
// new, so reconcileMetadata's pre-read-and-diff below would be pure overhead.
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
// stats may be nil (watcher.rebaseIfMoved has no *Stats in scope, being
// called from inside an InTx closure returning bare error) -- in that case
// the written count is logged at DEBUG instead of accumulated.
func reconcileAllMetadata(ctx context.Context, q *sqlcgen.Queries, nodeID int64, r Result, stats *Stats, log *slog.Logger) error {
	exif := exifMetadata(r)
	ffprobe := ffprobeMetadata(r)
	if len(exif) == 0 && len(ffprobe) == 0 {
		// Nothing this Result derives at all -- exiftool/ffprobe absent from
		// PATH, or a non-media file. The pre-#105 persistAllMetadata path
		// made zero DB calls in this case (cappedSortedKeys' len(kv)==0
		// short-circuit); reconcileAllMetadata must preserve that, not spend
		// a ListNodeMetadata read on the writer connection for nothing to
		// reconcile against. A node that already has metadata from an
		// earlier pass (tools installed then later removed, or genuinely
		// probe-less content) simply keeps its existing rows untouched.
		return nil
	}

	existing, err := q.ListNodeMetadata(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("list node_metadata for reconcile: %w", err)
	}
	prior := make(map[metadataRowKey]string, len(existing))
	for _, row := range existing {
		prior[metadataRowKey{row.Source, row.Key}] = row.Value
	}

	written, err := reconcileMetadata(ctx, q, nodeID, "exiftool", exif, metadataCap, prior, log)
	if err != nil {
		return err
	}
	n, err := reconcileMetadata(ctx, q, nodeID, "ffprobe", ffprobe, metadataCap, prior, log)
	if err != nil {
		return err
	}
	written += n

	if stats != nil {
		stats.MetadataWritten += written
	} else if written > 0 {
		log.Debug("pipeline: node_metadata reconciled", "nodeID", nodeID, "written", written)
	}
	return nil
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
// next scan.
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
