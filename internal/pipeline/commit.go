package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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
func Commit(ctx context.Context, database *db.DB, locationID int64, results []Result) (Stats, error) {
	var stats Stats
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		for i := range results {
			if err := commitOne(ctx, q, locationID, results[i], &stats); err != nil {
				return fmt.Errorf("commit %q: %w", results[i].Path, err)
			}
		}
		return nil
	})
	return stats, err
}

func commitOne(ctx context.Context, q *sqlcgen.Queries, locationID int64, r Result, stats *Stats) error {
	existing, err := q.GetLiveNodeByPath(ctx, r.Path)
	switch {
	case err == nil:
		if existing.FastHash != nil && *existing.FastHash == r.FastHash {
			stats.Touched++
			return q.TouchMediaNode(ctx, sqlcgen.TouchMediaNodeParams{
				ID:        existing.ID,
				MtimeUnix: r.ModTime.Unix(),
			})
		}
		return commitVersionCollision(ctx, q, locationID, existing, r, stats)

	case errors.Is(err, sql.ErrNoRows):
		return commitNoLiveNode(ctx, q, locationID, r, stats)

	default:
		return fmt.Errorf("get live node by path: %w", err)
	}
}

func commitVersionCollision(ctx context.Context, q *sqlcgen.Queries, locationID int64, existing sqlcgen.MediaNode, r Result, stats *Stats) error {
	// Archive first -- see Commit's doc comment for why the order matters.
	if err := q.ArchiveMediaNode(ctx, existing.ID); err != nil {
		return fmt.Errorf("archive superseded node: %w", err)
	}
	newNode, err := insertNewNode(ctx, q, locationID, r)
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

func commitNoLiveNode(ctx context.Context, q *sqlcgen.Queries, locationID int64, r Result, stats *Stats) error {
	if r.FastHash != "" {
		missing, err := q.GetMissingNodeByFastHash(ctx, &r.FastHash)
		if err == nil {
			stats.Moved++
			return q.RebaseMissingNodePath(ctx, sqlcgen.RebaseMissingNodePathParams{
				ID:                missing.ID,
				FilePath:          r.Path,
				FileName:          r.FileName,
				StorageLocationID: locationID,
				MtimeUnix:         r.ModTime.Unix(),
			})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get missing node by fast_hash: %w", err)
		}
	}

	stats.Inserted++
	_, err := insertNewNode(ctx, q, locationID, r)
	return err
}

func insertNewNode(ctx context.Context, q *sqlcgen.Queries, locationID int64, r Result) (sqlcgen.MediaNode, error) {
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
		FilenameStem:       nullString(filenameStem(r.FileName)),
	}
	if r.PHash != nil {
		params.Phash = sql.NullInt64{Int64: *r.PHash, Valid: true}
	}
	if r.CapturedAt != nil {
		params.CapturedAtUnix = sql.NullInt64{Int64: r.CapturedAt.Unix(), Valid: true}
	}

	return q.InsertMediaNode(ctx, params)
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
var versionSuffixRe = regexp.MustCompile(`(?i)(_edit|_proxy|_v\d+|-\d+| copy|\(\d+\))+$`)

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
