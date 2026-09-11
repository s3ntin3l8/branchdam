package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/metadata"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/projectfile"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// InheritWriteTimeout bounds the exiftool write subprocess, mirroring the
// scan path's per-file probe deadline -- a hung exiftool on a stalled network
// mount must not hang caller indefinitely.
const InheritWriteTimeout = 30 * time.Second

// Sentinel errors for metadata inheritance operations.
var (
	ErrNodeNotFound     = errors.New("asset not found")
	ErrIsProjectFile    = errors.New("cannot inherit metadata into a project file")
	ErrNoParent         = errors.New("asset has no resolved parent edge to inherit from")
	ErrIneligibleParent = errors.New("cannot inherit from a Tier-3-resolved or non-ancestry (duplicate/sidecar) parent edge")
)

// ErrParentUnavailable indicates that the parent asset is in an archived or missing state.
type ErrParentUnavailable struct {
	State string
}

func (e *ErrParentUnavailable) Error() string {
	return fmt.Sprintf("parent asset is %s, not a usable identity source", e.State)
}

// ValidParentRelationships is the closed set of relationship types that
// represent identity ancestry -- the only kinds of edge inherit-metadata may
// treat as "this child's parent". DUPLICATE_OF is a content match, not
// ancestry: stamping a duplicate's node_uuid into XMP-xmpMM:DerivedFrom would
// fabricate a false lineage. PROJECT_SIDECAR's "parent" is the project file
// itself (the resolver makes the project file the edge's source), not a
// media ancestor whose EXIF is meaningful to inherit.
var ValidParentRelationships = map[string]bool{
	"DERIVED_FROM": true,
	"FINAL_EXPORT": true,
	"PROXY_OF":     true,
}

// PickWinningParent returns the highest-confidence Tier-1/2 parent edge that
// is AUTO_ACCEPTED or CONFIRMED and represents identity ancestry (see
// ValidParentRelationships), or nil when the node has none. A NEEDS_REVIEW
// (unconfirmed) or REJECTED edge is never a valid identity source: stamping
// an unconfirmed parent's metadata into the child's file would cement a
// possibly-wrong lineage with no recovery path. Tier-3 (heuristic) matches
// are excluded from selection entirely, not merely rejected after one is
// picked -- a Tier-3 edge existing alongside a valid Tier-1/2 parent must
// never prevent inheriting from the valid one. Equal-confidence ties break by
// lowest edge id (ListEdgesByTarget has no ORDER BY), keeping the result
// deterministic.
func PickWinningParent(edges []sqlcgen.MediaEdge) *sqlcgen.MediaEdge {
	var best *sqlcgen.MediaEdge
	for i := range edges {
		e := &edges[i]
		if e.ReviewState != "AUTO_ACCEPTED" && e.ReviewState != "CONFIRMED" {
			continue
		}
		if e.Tier == 3 || !ValidParentRelationships[e.RelationshipType] {
			continue
		}
		// Highest confidence wins; on a tie, the lowest edge id (deterministic,
		// since ListEdgesByTarget has no ORDER BY).
		if best == nil || e.Confidence > best.Confidence || (e.Confidence == best.Confidence && e.ID < best.ID) {
			best = e
		}
	}
	return best
}

// HasResolvedButIneligibleParent reports whether edges contains any
// AUTO_ACCEPTED/CONFIRMED edge at all. Only meaningful as a follow-up check
// after PickWinningParent(edges) has already returned nil: at that point any
// resolved edge found here must be one PickWinningParent excluded (Tier-3, or
// not an identity-ancestry relationship type) -- since if it were eligible,
// PickWinningParent would have picked it.
func HasResolvedButIneligibleParent(edges []sqlcgen.MediaEdge) bool {
	for i := range edges {
		e := &edges[i]
		if e.ReviewState == "AUTO_ACCEPTED" || e.ReviewState == "CONFIRMED" {
			return true
		}
	}
	return false
}

// ErrPostWriteRefreshFailed indicates that WriteTags modified the file on disk,
// but the subsequent RefreshNodeAfterInPlaceWrite failed to re-read or re-hash it.
type ErrPostWriteRefreshFailed struct {
	Err error
}

func (e *ErrPostWriteRefreshFailed) Error() string {
	return fmt.Sprintf("metadata written to disk but node state refresh failed: %v", e.Err)
}

func (e *ErrPostWriteRefreshFailed) Unwrap() error {
	return e.Err
}

// LoadTagSet assembles a node's inheritable tag values from its promoted
// columns and node_metadata rows (source='exiftool'). A read failure is
// surfaced, not swallowed -- a partial/empty tagset would silently produce a
// partial inheritance.
func LoadTagSet(ctx context.Context, q *sqlcgen.Queries, node sqlcgen.MediaNode) (metadata.TagSet, error) {
	ts := metadata.TagSet{
		Identifier:  node.NodeUuid, // XMP-dc:Identifier is always the node's own uuid
		DerivedFrom: node.DerivedFromID.String,
		Model:       node.CameraModel.String,
	}
	rows, err := q.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		return ts, err
	}
	for _, r := range rows {
		if r.Source != "exiftool" {
			continue
		}
		switch r.Key {
		case "EXIF:Make":
			ts.Make = r.Value
		case "EXIF:LensModel":
			ts.LensModel = r.Value
		case "EXIF:SerialNumber":
			ts.SerialNumber = r.Value
		case "EXIF:DateTimeOriginal":
			ts.DateTimeOriginal = r.Value
		case "EXIF:OffsetTimeOriginal":
			ts.OffsetTimeOriginal = r.Value
		case "Composite:GPSLatitude":
			if f, err := strconv.ParseFloat(r.Value, 64); err == nil {
				ts.GPSLatitude = &f
			}
		case "Composite:GPSLongitude":
			if f, err := strconv.ParseFloat(r.Value, 64); err == nil {
				ts.GPSLongitude = &f
			}
		}
	}
	if ts.DateTimeOriginal == "" && node.CapturedAtUnix.Valid {
		ts.DateTimeOriginal = time.Unix(node.CapturedAtUnix.Int64, 0).UTC().Format("2006:01:02 15:04:05")
		ts.OffsetTimeOriginal = "+00:00"
	}
	return ts, nil
}

// RefreshNodeAfterInPlaceWrite re-reads the file's size and re-hashes it
// (the same fast_hash a scan would compute -- xxHash64 over the same sample
// regions) after an in-place exiftool write, and persists all three onto the
// node's row. Retries up to 3 times to ride out transient file-locks or mount lag
// immediately following subprocess termination.
func RefreshNodeAfterInPlaceWrite(ctx context.Context, database *db.DB, guard *storage.Guard, node sqlcgen.MediaNode) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), InheritWriteTimeout)
	defer cancel()
	var f *os.File
	var stat os.FileInfo
	var openErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-rctx.Done():
				return rctx.Err()
			case <-time.After(time.Duration(attempt*50) * time.Millisecond):
			}
		}
		if guard != nil {
			f, openErr = guard.OpenRead(node.FilePath)
		} else {
			f, openErr = os.OpenFile(node.FilePath, os.O_RDONLY, 0)
		}
		if openErr != nil {
			continue
		}
		stat, openErr = f.Stat()
		if openErr != nil {
			_ = f.Close()
			continue
		}
		break
	}
	if openErr != nil {
		return fmt.Errorf("open for re-hash (after retries): %w", openErr)
	}
	defer func() { _ = f.Close() }()

	fastHash, err := hashing.FastHash(f, stat.Size())
	if err != nil {
		return fmt.Errorf("re-hash: %w", err)
	}

	return database.InTx(rctx, func(q *sqlcgen.Queries) error {
		if err := q.RefreshMediaNodeAfterInPlaceWrite(rctx, sqlcgen.RefreshMediaNodeAfterInPlaceWriteParams{
			ID:        node.ID,
			SizeBytes: stat.Size(),
			MtimeUnix: stat.ModTime().Unix(),
			FastHash:  &fastHash,
		}); err != nil {
			return err
		}
		return q.InvalidateThumbnail(rctx, node.ID)
	})
}

// nodeLocker serializes concurrent in-place writes to the same child node file
// across scan workers and HTTP handlers (edge confirm, edge create, manual inherit).
type nodeLocker struct {
	mu    sync.Mutex
	locks map[int64]*refCountedLock
}

type refCountedLock struct {
	mu       sync.Mutex
	refCount int
}

var globalNodeLocker = &nodeLocker{
	locks: make(map[int64]*refCountedLock),
}

func (l *nodeLocker) lock(id int64) func() {
	l.mu.Lock()
	entry, ok := l.locks[id]
	if !ok {
		entry = &refCountedLock{}
		l.locks[id] = entry
	}
	entry.refCount++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refCount--
		if entry.refCount <= 0 {
			delete(l.locks, id)
		}
		l.mu.Unlock()
	}
}

// InheritDeps bundles dependencies required for metadata inheritance execution.
type InheritDeps struct {
	DB     *db.DB
	Guard  *storage.Guard
	Prober *probe.Prober
	Log    *slog.Logger
}

// InheritMetadata copies identity metadata (EXIF/XMP) from child's winning
// parent edge into the child file on disk, then updates size_bytes, mtime_unix,
// fast_hash, and node_metadata in SQLite.
func InheritMetadata(ctx context.Context, deps InheritDeps, childID int64) (map[string]string, error) {
	if deps.DB == nil {
		return nil, errors.New("database is nil")
	}
	child, err := deps.DB.Reader.GetMediaNodeByID(ctx, childID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	if child.LifecycleState == "ARCHIVED" {
		return nil, ErrNodeNotFound
	}

	// Refuse project archive files
	if _, ok := projectfile.GetParser(child.FilePath); ok {
		return nil, ErrIsProjectFile
	}

	// Guard check
	if deps.Guard != nil {
		if err := deps.Guard.CheckWrite(child.FilePath); err != nil {
			return nil, err
		}
	}

	unlock := globalNodeLocker.lock(child.ID)
	defer unlock()

	parents, err := deps.DB.Reader.ListEdgesByTarget(ctx, child.ID)
	if err != nil {
		return nil, fmt.Errorf("list parent edges: %w", err)
	}
	winning := PickWinningParent(parents)
	if winning == nil {
		if HasResolvedButIneligibleParent(parents) {
			return nil, ErrIneligibleParent
		}
		return nil, ErrNoParent
	}

	parent, err := deps.DB.Reader.GetMediaNodeByID(ctx, winning.SourceNodeID)
	if err != nil {
		return nil, fmt.Errorf("get parent asset: %w", err)
	}
	if parent.LifecycleState == "ARCHIVED" || parent.LifecycleState == "MISSING" {
		return nil, &ErrParentUnavailable{State: parent.LifecycleState}
	}

	parentTags, err := LoadTagSet(ctx, deps.DB.Reader, parent)
	if err != nil {
		return nil, fmt.Errorf("read parent metadata: %w", err)
	}
	childTags, err := LoadTagSet(ctx, deps.DB.Reader, child)
	if err != nil {
		return nil, fmt.Errorf("read child metadata: %w", err)
	}
	childTags.DerivedFrom = parent.NodeUuid

	tags := metadata.Plan(parentTags, childTags)

	// Short-circuit if planned tags already match child's current state:
	// Plan unconditionally emits XMP-dc:Identifier and XMP-xmpMM:DerivedFrom once a child has a parent.
	// If the child already has this parent recorded as derived_from_id and no
	// other missing tags were planned (len(tags) <= 2), nothing has changed --
	// do not spawn an exiftool write or rewrite the file on disk.
	if child.DerivedFromID.Valid && child.DerivedFromID.String == parent.NodeUuid && len(tags) <= 2 {
		return tags, nil
	}

	if deps.Prober == nil {
		return nil, probe.ErrToolUnavailable
	}

	wctx, cancel := context.WithTimeout(ctx, InheritWriteTimeout)
	defer cancel()
	if err := deps.Prober.WriteTags(wctx, child.FilePath, tags); err != nil {
		return nil, err
	}

	if err := RefreshNodeAfterInPlaceWrite(ctx, deps.DB, deps.Guard, child); err != nil {
		return nil, &ErrPostWriteRefreshFailed{Err: err}
	}

	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), InheritWriteTimeout)
	defer bcancel()
	if exif, err := deps.Prober.Exif(bctx, child.FilePath); err != nil {
		if deps.Log != nil {
			deps.Log.Warn("inherit-metadata: re-read child for backfill failed", "nodeID", child.ID, "err", err)
		}
	} else {
		if err := PersistExifMetadata(bctx, deps.DB, child.ID, exif, deps.Log); err != nil {
			if deps.Log != nil {
				deps.Log.Warn("inherit-metadata: backfill node_metadata failed", "nodeID", child.ID, "err", err)
			}
		}
		derivedFrom := child.DerivedFromID
		if parent.NodeUuid != "" {
			derivedFrom = sql.NullString{String: parent.NodeUuid, Valid: true}
		}
		cameraModel := child.CameraModel
		if (!cameraModel.Valid || cameraModel.String == "") && exif.Model != "" {
			cameraModel = sql.NullString{String: exif.Model, Valid: true}
		}
		lensModel := child.LensModel
		if (!lensModel.Valid || lensModel.String == "") && exif.LensModel != "" {
			lensModel = sql.NullString{String: exif.LensModel, Valid: true}
		}
		cameraSerial := child.CameraSerial
		if (!cameraSerial.Valid || cameraSerial.String == "") && exif.SerialNumber != "" {
			cameraSerial = sql.NullString{String: exif.SerialNumber, Valid: true}
		}
		capturedAt := child.CapturedAtUnix
		if !capturedAt.Valid && exif.CapturedAt != nil {
			capturedAt = sql.NullInt64{Int64: exif.CapturedAt.Unix(), Valid: true}
		}
		_ = deps.DB.InTx(bctx, func(q *sqlcgen.Queries) error {
			return q.UpdateMediaNodePromotedColumns(bctx, sqlcgen.UpdateMediaNodePromotedColumnsParams{
				ID:                 child.ID,
				OriginalDocumentID: child.OriginalDocumentID,
				DocumentID:         child.DocumentID,
				DerivedFromID:      derivedFrom,
				CameraModel:        cameraModel,
				CameraSerial:       cameraSerial,
				LensModel:          lensModel,
				CapturedAtUnix:     capturedAt,
			})
		})
	}

	return tags, nil
}
