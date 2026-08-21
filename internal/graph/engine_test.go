package graph

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/naming"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return database
}

func seedLocation(t *testing.T, database *db.DB) int64 {
	t.Helper()
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     t.Name(),
			RootPath: t.TempDir(),
			Tier:     "TIER2_EXPORTS",
			ReadOnly: 0,
			Prunable: 0,
		})
		id = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	return id
}

// nodeFixture is the subset of pipeline.Result's fields these tests need.
// internal/pipeline imports internal/graph (to run edge resolution after
// committing a node), so graph's tests can't import pipeline back --
// that's a real import cycle, not just a style preference. seedNode
// therefore inserts directly via sqlcgen rather than through
// pipeline.Commit.
type nodeFixture struct {
	Path               string
	FileName           string
	FileExt            string
	FastHash           string
	OriginalDocumentID string
	DocumentID         string
	CapturedAt         *time.Time
	CameraModel        string
	CameraSerial       string
	LensModel          string
	PHash              *int64
}

func seedNode(t *testing.T, database *db.DB, locationID int64, f nodeFixture) sqlcgen.MediaNode {
	t.Helper()
	ctx := context.Background()

	if f.FastHash == "" {
		f.FastHash = strings.Repeat("a", 16) // unique-enough per call site's distinct path; not asserted on
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint node_uuid: %v", err)
	}

	params := sqlcgen.InsertMediaNodeParams{
		NodeUuid:           id.String(),
		StorageLocationID:  locationID,
		FilePath:           f.Path,
		FileName:           f.FileName,
		FileExt:            f.FileExt,
		FastHash:           &f.FastHash,
		IndexingStatus:     "INDEXED_SHALLOW",
		GraphStatus:        "UNLINKED",
		LifecycleState:     "ACTIVE",
		OriginalDocumentID: nullString(f.OriginalDocumentID),
		DocumentID:         nullString(f.DocumentID),
		CameraModel:        nullString(f.CameraModel),
		FilenameStem:       nullString(naming.Stem(f.FileName)),
		CameraSerial:       nullString(f.CameraSerial),
		LensModel:          nullString(f.LensModel),
	}
	if f.CapturedAt != nil {
		params.CapturedAtUnix = sql.NullInt64{Int64: f.CapturedAt.Unix(), Valid: true}
	}
	if f.PHash != nil {
		params.Phash = sql.NullInt64{Int64: *f.PHash, Valid: true}
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.InsertMediaNode(ctx, params)
		return err
	})
	if err != nil {
		t.Fatalf("seed node %s: %v", f.Path, err)
	}
	node, err := database.Reader.GetLiveNodeByPath(ctx, f.Path)
	if err != nil {
		t.Fatalf("get seeded node %s: %v", f.Path, err)
	}
	return node
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func asGraphNode(row sqlcgen.MediaNode) Node {
	return toNodes([]sqlcgen.MediaNode{row})[0]
}

func newEngine(database *db.DB) *Engine {
	return NewEngine(database, nil, XMPOriginalDocumentIDResolver{}, FilenameStemResolver{})
}

// TestXMPOriginalDocumentIDMatch: matching XMP:OriginalDocumentID / document_id
// is a 0.95-confidence DERIVED_FROM edge, auto-accepted.
func TestXMPOriginalDocumentIDMatch(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	parent := seedNode(t, database, locationID, nodeFixture{
		Path: "/raw/MASTER.ARW", FileName: "MASTER.ARW", FileExt: "arw",
		DocumentID: "doc-xyz",
	})
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/exports/COMPLETELY_DIFFERENT_NAME.jpg", FileName: "COMPLETELY_DIFFERENT_NAME.jpg", FileExt: "jpg",
		OriginalDocumentID: "doc-xyz",
	})

	engine := newEngine(database)
	edges, created, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1 (a brand new edge)", created)
	}
	edge := edges[0]
	if edge.SourceNodeID != parent.ID || edge.TargetNodeID != childRow.ID {
		t.Errorf("edge = %d->%d, want %d->%d", edge.SourceNodeID, edge.TargetNodeID, parent.ID, childRow.ID)
	}
	// parent ext is "arw" (raw), child is "jpg" -- inferRelationship says FINAL_EXPORT.
	if edge.RelationshipType != "FINAL_EXPORT" {
		t.Errorf("relationship_type = %q, want FINAL_EXPORT", edge.RelationshipType)
	}
	if edge.Confidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95", edge.Confidence)
	}
	if edge.ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED", edge.ReviewState)
	}

	childAfter, err := database.Reader.GetMediaNodeByID(ctx, childRow.ID)
	if err != nil {
		t.Fatalf("get child after resolve: %v", err)
	}
	if childAfter.GraphStatus != "LINKED" {
		t.Errorf("child graph_status = %q, want LINKED", childAfter.GraphStatus)
	}
}

// TestFilenameStemAllBoostsReachesAutoAccept: same stem + same capture day +
// same camera_model + same directory stacks to the 0.90 clamp, which meets
// (not exceeds) the auto-accept threshold.
func TestFilenameStemAllBoostsReachesAutoAccept(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/DSC01234.ARW", FileName: "DSC01234.ARW", FileExt: "arw",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/DSC01234.jpg", FileName: "DSC01234.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].Confidence != 0.90 {
		t.Errorf("confidence = %v, want 0.90 (clamped)", edges[0].Confidence)
	}
	if edges[0].RelationshipType != "FINAL_EXPORT" {
		t.Errorf("relationship_type = %q, want FINAL_EXPORT", edges[0].RelationshipType)
	}
	if edges[0].ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED", edges[0].ReviewState)
	}
}

// TestFilenameStemWeakMatchNeedsReview: stem match alone (no day/camera/dir
// corroboration) lands at the 0.60 base, well under auto-accept, and shows
// up in the audit queue.
func TestFilenameStemWeakMatchNeedsReview(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	oldDay := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	newDay := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
	seedNode(t, database, locationID, nodeFixture{
		Path: "/archive/DSC0999.ARW", FileName: "DSC0999.ARW", FileExt: "arw",
		CapturedAt: &oldDay, CameraModel: "OLD-CAMERA",
	})
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/exports/DSC0999.jpg", FileName: "DSC0999.jpg", FileExt: "jpg",
		CapturedAt: &newDay, CameraModel: "NEW-CAMERA",
	})

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].Confidence != 0.60 {
		t.Errorf("confidence = %v, want 0.60 (base only)", edges[0].Confidence)
	}
	if edges[0].ReviewState != "NEEDS_REVIEW" {
		t.Errorf("review_state = %q, want NEEDS_REVIEW", edges[0].ReviewState)
	}

	rows, err := database.Reader.ListAuditQueue(ctx, sqlcgen.ListAuditQueueParams{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("ListAuditQueue: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == edges[0].ID {
			found = true
		}
	}
	if !found {
		t.Error("the NEEDS_REVIEW edge does not appear in the audit queue")
	}
}

// TestFilenameStemDistinctCameraNumbersDoNotCollapse backs H3: a batch of
// camera-default-named files sharing capture day, camera model, and
// directory -- the common case for one memory-card import -- must not
// collapse to a single shared filename_stem and produce an O(n^2)
// auto-accepted DERIVED_FROM mesh. Before the fix (unbounded -\d+ in
// versionSuffixRe), every DSC-NNNN.JPG in this batch shared the bare stem
// "dsc"; with the -\d{1,2} bound, each keeps its own 4-digit suffix and
// FilenameStemResolver finds no candidates at all among siblings that share
// nothing but a camera-generated frame number.
func TestFilenameStemDistinctCameraNumbersDoNotCollapse(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	var last sqlcgen.MediaNode
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("DSC-%04d.JPG", i)
		last = seedNode(t, database, locationID, nodeFixture{
			Path: "/2026-07-15/" + name, FileName: name, FileExt: "jpg",
			CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
		})
	}

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(last))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges among 20 distinct DSC-NNNN siblings, want 0 -- each keeps its own numeric suffix, not a shared stem: %+v", len(edges), edges)
	}
}

// TestFilenameStemUnpaddedIndexBatchDoesNotCollapse backs issue #132: unlike
// TestFilenameStemDistinctCameraNumbersDoNotCollapse's 4-digit DSC-NNNN
// siblings (which keep distinct stems by construction), an unpadded 1-2
// digit hyphen-numbering scheme -- an unpadded camera counter, or a human
// numbering a batch "trip-1.jpg".."trip-45.jpg" -- DOES collapse to one
// shared stem ("trip"). Without a "trip.jpg" anchor present, the resolver's
// anchor rule must still emit zero candidates among the batch.
func TestFilenameStemUnpaddedIndexBatchDoesNotCollapse(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	var last sqlcgen.MediaNode
	for i := 1; i <= 45; i++ {
		name := fmt.Sprintf("trip-%d.jpg", i)
		last = seedNode(t, database, locationID, nodeFixture{
			Path: "/2026-07-15/" + name, FileName: name, FileExt: "jpg",
			CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
		})
	}

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(last))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges among 45 unpadded trip-N siblings with no trip.jpg anchor, want 0: %+v", len(edges), edges)
	}
}

// TestFilenameStemIndexBatchWithAnchorFormsStarNotMesh: the same
// unpadded-index batch as TestFilenameStemUnpaddedIndexBatchDoesNotCollapse,
// but WITH a "trip.jpg" anchor present. Resolving one child against 45
// index-suffixed siblings plus the anchor must still find exactly ONE
// candidate parent -- the anchor itself -- not one per sibling: the
// direction rule (an index-suffixed node is never a parent) is what keeps
// an anchored batch a star (n-1 edges, one per child resolved) rather than
// degenerating back into the O(n^2) mesh issue #132 exists to close.
func TestFilenameStemIndexBatchWithAnchorFormsStarNotMesh(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	anchor := seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/trip.jpg", FileName: "trip.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})
	var last sqlcgen.MediaNode
	for i := 1; i <= 45; i++ {
		name := fmt.Sprintf("trip-%d.jpg", i)
		last = seedNode(t, database, locationID, nodeFixture{
			Path: "/2026-07-15/" + name, FileName: name, FileExt: "jpg",
			CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
		})
	}

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(last))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges resolving one child against 45 index-suffixed siblings + 1 anchor, want 1 (a star, not a mesh): %+v", len(edges), edges)
	}
	if edges[0].SourceNodeID != anchor.ID {
		t.Errorf("edge source = %d, want the anchor node %d -- only the bare anchor may be a parent", edges[0].SourceNodeID, anchor.ID)
	}
	if edges[0].Confidence != indexMatchConfidenceCap {
		t.Errorf("confidence = %v, want %v (index match cap)", edges[0].Confidence, indexMatchConfidenceCap)
	}
}

// TestFilenameStemParenIndexBatchDoesNotCollapse is
// TestFilenameStemUnpaddedIndexBatchDoesNotCollapse's counterpart for the
// "(N)" OS duplicate-index marker, which -- unlike "-N" -- was never
// digit-bounded at all (issue #132's scope call: both are index markers).
func TestFilenameStemParenIndexBatchDoesNotCollapse(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	var last sqlcgen.MediaNode
	for i := 1; i <= 45; i++ {
		name := fmt.Sprintf("trip (%d).jpg", i)
		last = seedNode(t, database, locationID, nodeFixture{
			Path: "/2026-07-15/" + name, FileName: name, FileExt: "jpg",
			CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
		})
	}

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(last))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges among 45 trip (N) siblings with no trip.jpg anchor, want 0: %+v", len(edges), edges)
	}
}

// TestFilenameStemIndexAnchorNeedsReview: with a bare "photo.jpg" anchor
// present, a "photo-2.jpg" child DOES emit a candidate -- but capped at
// indexMatchConfidenceCap (0.89), strictly below AUTO_ACCEPTED, even with
// every boost. It still surfaces in the audit queue (mirrors
// TestFilenameStemWeakMatchNeedsReview's ListAuditQueue assertion).
func TestFilenameStemIndexAnchorNeedsReview(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/photo.jpg", FileName: "photo.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/photo-2.jpg", FileName: "photo-2.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].Confidence != indexMatchConfidenceCap {
		t.Errorf("confidence = %v, want %v (index match cap)", edges[0].Confidence, indexMatchConfidenceCap)
	}
	if edges[0].ReviewState != "NEEDS_REVIEW" {
		t.Errorf("review_state = %q, want NEEDS_REVIEW", edges[0].ReviewState)
	}

	rows, err := database.Reader.ListAuditQueue(ctx, sqlcgen.ListAuditQueueParams{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("ListAuditQueue: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == edges[0].ID {
			found = true
		}
	}
	if !found {
		t.Error("the index-anchored NEEDS_REVIEW edge does not appear in the audit queue")
	}
}

// TestFilenameStemIndexSuffixNeverParent: the direction rule -- an
// index-suffixed node is never treated as a candidate PARENT. Resolving
// the bare "photo.jpg" (not "photo-2.jpg") against a "photo-2.jpg" sibling
// must emit nothing.
func TestFilenameStemIndexSuffixNeverParent(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/photo.jpg", FileName: "photo.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})
	seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/photo-2.jpg", FileName: "photo-2.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges resolving bare photo.jpg against photo-2.jpg, want 0 (index-suffixed node can't be a parent): %+v", len(edges), edges)
	}
}

// TestFilenameStemIndexAutoAcceptsWithCorroboration proves issue #132
// criterion 2 as worded: an index-derived filename_stem match alone never
// auto-accepts, but with a corroborating non-filename_stem signal (here,
// xmp_original_document_id at 0.95) for the SAME (parent, child, rel), the
// merged edge still reaches AUTO_ACCEPTED -- mergeCandidates' max-confidence
// merge needs no changes for this to work.
func TestFilenameStemIndexAutoAcceptsWithCorroboration(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/photo.jpg", FileName: "photo.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
		OriginalDocumentID: "doc-1", DocumentID: "doc-1",
	})
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/photo-2.jpg", FileName: "photo-2.jpg", FileExt: "jpg",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
		OriginalDocumentID: "doc-1",
	})

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1 (filename_stem and xmp_original_document_id candidates merge to one edge): %+v", len(edges), edges)
	}
	if edges[0].Confidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95 (xmp_original_document_id wins the merge)", edges[0].Confidence)
	}
	if edges[0].ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED", edges[0].ReviewState)
	}
}

// TestFilenameStemRoleSuffixStillAutoAccepts proves the index gate does not
// leak into role-suffixed matches: role suffixes (_proxy here) are
// unaffected by issue #132, and a fully-boosted role-suffix match still
// reaches AUTO_ACCEPTED exactly as before.
func TestFilenameStemRoleSuffixStillAutoAccepts(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/render.mov", FileName: "render.mov", FileExt: "mov",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})
	childRow := seedNode(t, database, locationID, nodeFixture{
		Path: "/2026-07-15/render_proxy.mov", FileName: "render_proxy.mov", FileExt: "mov",
		CapturedAt: &capturedAt, CameraModel: "ILCE-7M4",
	})

	engine := newEngine(database)
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].Confidence != 0.90 {
		t.Errorf("confidence = %v, want 0.90 (clamped, role suffix unaffected by the index gate)", edges[0].Confidence)
	}
	if edges[0].RelationshipType != "PROXY_OF" {
		t.Errorf("relationship_type = %q, want PROXY_OF", edges[0].RelationshipType)
	}
	if edges[0].ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED", edges[0].ReviewState)
	}
}

// TestFilenameStemProxyExtAlwaysChildRegardlessOfEvalOrder proves issue
// #228: DJI_0001.MP4 and DJI_0001.LRF share an identical bare stem
// (naming.Stem strips only the extension), so neither side carries an
// index suffix and #132's indexGated rule never fires for this pair --
// without the #228 fix, FilenameStemResolver would propose a candidate in
// whichever direction Engine happens to resolve first, since it runs once
// per node as "child" (Engine.ResolveAndCommit). Two subtests seed the
// exact same pair and only vary which node is passed as the resolver's
// "child" input, mirroring scan/resolve order being effectively random --
// both must land on the identical edge: the proxy (.lrf) as the target
// (child), the real video as the source (parent), relationship PROXY_OF.
func TestFilenameStemProxyExtAlwaysChildRegardlessOfEvalOrder(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)

	assertProxyEdge := func(t *testing.T, edges []sqlcgen.MediaEdge, video, proxy sqlcgen.MediaNode) {
		t.Helper()
		if len(edges) != 1 {
			t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
		}
		if edges[0].SourceNodeID != video.ID || edges[0].TargetNodeID != proxy.ID {
			t.Errorf("edge = %d->%d, want video(%d)->proxy(%d) (proxy must always be the child)",
				edges[0].SourceNodeID, edges[0].TargetNodeID, video.ID, proxy.ID)
		}
		if edges[0].RelationshipType != "PROXY_OF" {
			t.Errorf("relationship_type = %q, want PROXY_OF", edges[0].RelationshipType)
		}
	}

	t.Run("resolver evaluates the proxy as its child input", func(t *testing.T) {
		database := openTestDB(t)
		ctx := context.Background()
		locationID := seedLocation(t, database)

		video := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.MP4", FileName: "DJI_0001.MP4", FileExt: "mp4",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("3", 16),
		})
		proxy := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.LRF", FileName: "DJI_0001.LRF", FileExt: "lrf",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("4", 16),
		})

		engine := newEngine(database)
		edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(proxy))
		if err != nil {
			t.Fatalf("ResolveAndCommit: %v", err)
		}
		assertProxyEdge(t, edges, video, proxy)
	})

	t.Run("resolver evaluates the real video as its child input", func(t *testing.T) {
		database := openTestDB(t)
		ctx := context.Background()
		locationID := seedLocation(t, database)

		video := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.MP4", FileName: "DJI_0001.MP4", FileExt: "mp4",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("5", 16),
		})
		proxy := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.LRF", FileName: "DJI_0001.LRF", FileExt: "lrf",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("6", 16),
		})

		engine := newEngine(database)
		edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(video))
		if err != nil {
			t.Fatalf("ResolveAndCommit: %v", err)
		}
		assertProxyEdge(t, edges, video, proxy)
	})

	// A real scan resolves BOTH committed nodes against the same database,
	// one ResolveAndCommit call per node in the batch (pipeline.
	// drainAndCommit). Pre-#228, whichever node was resolved second would
	// propose the edge in the OPPOSITE direction from the first, and
	// Engine's cycle guard (WouldCreateCycle, fix #7) would silently
	// refuse it -- the "near-coin-flip" the issue describes, since which
	// direction wins depends on which node the engine happened to resolve
	// first. Post-fix, the second call's flip reproduces the SAME
	// (parent, child, rel) as the first, so UpsertMediaEdge treats it as
	// an idempotent upsert, not a rejected cycle: exactly one edge must
	// exist afterward, not zero.
	t.Run("resolving both nodes in sequence converges on one edge, not a rejected cycle", func(t *testing.T) {
		database := openTestDB(t)
		ctx := context.Background()
		locationID := seedLocation(t, database)

		video := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.MP4", FileName: "DJI_0001.MP4", FileExt: "mp4",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("7", 16),
		})
		proxy := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.LRF", FileName: "DJI_0001.LRF", FileExt: "lrf",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("8", 16),
		})

		engine := newEngine(database)

		// Resolve the proxy first (as a batch's first committed node
		// might be either file, in either order).
		firstEdges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(proxy))
		if err != nil {
			t.Fatalf("ResolveAndCommit(proxy): %v", err)
		}
		assertProxyEdge(t, firstEdges, video, proxy)

		// Then resolve the video -- pre-#228 this proposes the reverse
		// edge and gets silently refused as a cycle.
		secondEdges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(video))
		if err != nil {
			t.Fatalf("ResolveAndCommit(video): %v", err)
		}
		assertProxyEdge(t, secondEdges, video, proxy)

		edge, err := database.Reader.GetMediaEdgeBySourceTargetRel(ctx, sqlcgen.GetMediaEdgeBySourceTargetRelParams{
			SourceNodeID: video.ID, TargetNodeID: proxy.ID, RelationshipType: "PROXY_OF",
		})
		if err != nil {
			t.Fatalf("GetMediaEdgeBySourceTargetRel: %v", err)
		}
		if edge.ID != firstEdges[0].ID {
			t.Errorf("second resolve created a distinct edge (id %d) instead of upserting the first (id %d)", edge.ID, firstEdges[0].ID)
		}

		// And the reverse edge must NOT exist -- confirming the second
		// call converged on the same direction rather than being rejected
		// as a cycle after proposing the opposite one.
		if _, err := database.Reader.GetMediaEdgeBySourceTargetRel(ctx, sqlcgen.GetMediaEdgeBySourceTargetRelParams{
			SourceNodeID: proxy.ID, TargetNodeID: video.ID, RelationshipType: "PROXY_OF",
		}); err == nil {
			t.Error("a reverse proxy->video edge exists; want only the forward video->proxy edge")
		}
	})
}

// TestFilenameStemProxyExtNeverOverridesIndexAnchorInvariant is a
// regression test for a bug in the original #228 fix: the proxy-ext
// direction swap ran unconditionally, even for a pair that had ALREADY
// satisfied #132's own anchor+direction rules pre-swap. For
// DJI_0001.LRF (bare anchor, proxy ext) paired with DJI_0001-2.MP4
// (index-suffixed, non-proxy real video), the swap would flip the
// already-valid (parent=LRF anchor, child=MP4-2 index-suffixed) pairing
// into (parent=MP4-2, child=LRF) -- seating the index-suffixed node as
// the PARENT, directly violating #132's direction rule ("only the child
// may carry the index suffix. A derived duplicate is never treated as a
// parent"). #228 must not silently win over #132: the fix is to skip
// emitting any candidate for this pair rather than picking a winner
// between the two invariants. Both evaluation orders are tested, exactly
// like TestFilenameStemProxyExtAlwaysChildRegardlessOfEvalOrder above.
func TestFilenameStemProxyExtNeverOverridesIndexAnchorInvariant(t *testing.T) {
	capturedAt := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)

	t.Run("resolver evaluates the bare proxy anchor as its child input", func(t *testing.T) {
		database := openTestDB(t)
		ctx := context.Background()
		locationID := seedLocation(t, database)

		anchor := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.LRF", FileName: "DJI_0001.LRF", FileExt: "lrf",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("9", 16),
		})
		seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001-2.MP4", FileName: "DJI_0001-2.MP4", FileExt: "mp4",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("a", 16),
		})

		engine := newEngine(database)
		edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(anchor))
		if err != nil {
			t.Fatalf("ResolveAndCommit(anchor): %v", err)
		}
		if len(edges) != 0 {
			t.Fatalf("got %d edges, want 0 (the index-suffixed sibling can never be a parent, proxy-ext or not): %+v", len(edges), edges)
		}
	})

	t.Run("resolver evaluates the index-suffixed real video as its child input", func(t *testing.T) {
		database := openTestDB(t)
		ctx := context.Background()
		locationID := seedLocation(t, database)

		seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001.LRF", FileName: "DJI_0001.LRF", FileExt: "lrf",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("b", 16),
		})
		indexedVideo := seedNode(t, database, locationID, nodeFixture{
			Path: "/dcim/DJI_0001-2.MP4", FileName: "DJI_0001-2.MP4", FileExt: "mp4",
			CapturedAt: &capturedAt, FastHash: strings.Repeat("c", 16),
		})

		engine := newEngine(database)
		edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(indexedVideo))
		if err != nil {
			t.Fatalf("ResolveAndCommit(indexedVideo): %v", err)
		}
		if len(edges) != 0 {
			t.Fatalf("got %d edges, want 0 -- without the fix, the proxy-ext swap seats the index-suffixed node (DJI_0001-2.MP4) as the parent, violating #132: %+v", len(edges), edges)
		}
	})
}

// TestIndexMatchCapBelowTier2Threshold is a one-line guard that
// indexMatchConfidenceCap and AutoAcceptThresholdForTier(2) can never drift
// out of the relationship FilenameStemResolver's doc comment depends on.
func TestIndexMatchCapBelowTier2Threshold(t *testing.T) {
	if indexMatchConfidenceCap >= AutoAcceptThresholdForTier(2) {
		t.Errorf("indexMatchConfidenceCap (%v) must be strictly below AutoAcceptThresholdForTier(2) (%v)",
			indexMatchConfidenceCap, AutoAcceptThresholdForTier(2))
	}
}

// TestCycleRejected: A->B exists; proposing B->A must be refused, and must
// not write a row (fix #7).
func TestCycleRejected(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	a := seedNode(t, database, locationID, nodeFixture{Path: "/a.jpg", FileName: "a.jpg", FileExt: "jpg", FastHash: strings.Repeat("1", 16)})
	b := seedNode(t, database, locationID, nodeFixture{Path: "/b.jpg", FileName: "b.jpg", FileExt: "jpg", FastHash: strings.Repeat("2", 16)})

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: a.ID, TargetNodeID: b.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.95, Tier: 1, Resolver: "test-fixture", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed A->B edge: %v", err)
	}

	engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: b.ID, ChildID: a.ID, Rel: "DERIVED_FROM", Confidence: 0.99, Tier: 1,
		Resolver: "cycle-fixture", Evidence: map[string]any{},
	}})
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(a))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("cycle-closing candidate was committed: %+v", edges)
	}

	rows, err := database.Reader.ListEdgesBySource(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListEdgesBySource: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("found %d edges from B, want 0 (B->A must never have been written)", len(rows))
	}
}

// fixedCandidateResolver is a test-only Resolver that always proposes the
// same candidate, regardless of the child passed in -- used to force a
// specific (parent, child) pair through Engine without needing a real
// filename/XMP match.
type fixedCandidateResolver struct{ c Candidate }

func (fixedCandidateResolver) Name() string { return "fixed" }
func (fixedCandidateResolver) Tier() int    { return 1 }
func (r fixedCandidateResolver) Resolve(_ context.Context, _ Node, _ Lookup) ([]Candidate, error) {
	return []Candidate{r.c}, nil
}

// TestSelfEdgeRejectedByEngineCheck: the DB's own CHECK (source_node_id <>
// target_node_id) fires independent of any application logic -- belt to
// the resolvers' own "skip if parent.ID == child.ID" braces.
func TestSelfEdgeRejectedByEngineCheck(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)
	node := seedNode(t, database, locationID, nodeFixture{Path: "/self.jpg", FileName: "self.jpg", FileExt: "jpg"})

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: node.ID, TargetNodeID: node.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.5, Tier: 1, Resolver: "test", EvidenceJson: "{}", ReviewState: "NEEDS_REVIEW",
		})
		return err
	})
	if err == nil {
		t.Fatal("self-edge insert succeeded, want a CHECK constraint failure")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("error = %v, want a CHECK constraint failure", err)
	}
}

// TestParentMissingPropagatesForEveryRelationshipType is the regression
// guard for docs/schema.md fix #4: v_media_edges_resolved.parent_missing
// must be true when the parent is MISSING, for EVERY relationship_type --
// the spec's deleted trigger only ever did this for DERIVED_FROM.
func TestParentMissingPropagatesForEveryRelationshipType(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	parent := seedNode(t, database, locationID, nodeFixture{Path: "/p.arw", FileName: "p.arw", FileExt: "arw"})
	child := seedNode(t, database, locationID, nodeFixture{Path: "/p_proxy.mov", FileName: "p_proxy.mov", FileExt: "mov"})

	var edgeID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		edge, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parent.ID, TargetNodeID: child.ID, RelationshipType: "PROXY_OF",
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		edgeID = edge.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed PROXY_OF edge: %v", err)
	}

	before, err := database.Reader.ResolvedEdgeParentMissing(ctx, edgeID)
	if err != nil {
		t.Fatalf("ResolvedEdgeParentMissing (before): %v", err)
	}
	if before {
		t.Fatal("parent_missing = true before the parent was ever marked MISSING")
	}

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkNodeMissing(ctx, parent.ID)
	}); err != nil {
		t.Fatalf("mark parent missing: %v", err)
	}

	after, err := database.Reader.ResolvedEdgeParentMissing(ctx, edgeID)
	if err != nil {
		t.Fatalf("ResolvedEdgeParentMissing (after): %v", err)
	}
	if !after {
		t.Error("parent_missing = false after the parent was marked MISSING, for a PROXY_OF edge -- fix #4 regressed")
	}
}

func TestMergeCandidatesKeepsMaxConfidenceAndUnionsEvidence(t *testing.T) {
	candidates := []Candidate{
		{ParentID: 1, ChildID: 2, Rel: "DERIVED_FROM", Confidence: 0.60, Resolver: "filename_stem", Evidence: map[string]any{"a": 1}},
		{ParentID: 1, ChildID: 2, Rel: "DERIVED_FROM", Confidence: 0.95, Resolver: "xmp_original_document_id", Evidence: map[string]any{"b": 2}},
	}
	merged := mergeCandidates(candidates)
	if len(merged) != 1 {
		t.Fatalf("got %d merged candidates, want 1", len(merged))
	}
	if merged[0].Confidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95 (max)", merged[0].Confidence)
	}
	if merged[0].Resolver != "xmp_original_document_id" {
		t.Errorf("resolver = %q, want the winning resolver's name", merged[0].Resolver)
	}
	if _, ok := merged[0].Evidence["filename_stem"]; !ok {
		t.Error("evidence from the losing resolver was dropped, want it unioned in")
	}
	if _, ok := merged[0].Evidence["xmp_original_document_id"]; !ok {
		t.Error("evidence from the winning resolver is missing")
	}
}

func TestBelowFloorCandidatesNeverPersisted(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	a := seedNode(t, database, locationID, nodeFixture{Path: "/weak-a.jpg", FileName: "weak-a.jpg", FileExt: "jpg"})
	b := seedNode(t, database, locationID, nodeFixture{Path: "/weak-b.jpg", FileName: "weak-b.jpg", FileExt: "jpg"})

	engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: a.ID, ChildID: b.ID, Rel: "DERIVED_FROM", Confidence: 0.49, Tier: 3,
		Resolver: "weak-fixture", Evidence: map[string]any{},
	}})
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(b))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("a confidence-0.49 candidate was committed: %+v", edges)
	}

	rows, err := database.Reader.ListEdgesBySource(ctx, a.ID)
	if err != nil {
		t.Fatalf("ListEdgesBySource: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("found %d edges, want 0 (below-floor candidate must never be written)", len(rows))
	}
}

func TestPerTierAutoAcceptThreshold(t *testing.T) {
	tests := []struct {
		name            string
		tier            int
		confidence      float64
		wantReviewState string
	}{
		{
			name:            "Tier 3 candidate at 0.84 lands NEEDS_REVIEW",
			tier:            3,
			confidence:      0.84,
			wantReviewState: "NEEDS_REVIEW",
		},
		{
			name:            "Tier 3 candidate at exact boundary 0.85 lands AUTO_ACCEPTED",
			tier:            3,
			confidence:      0.85,
			wantReviewState: "AUTO_ACCEPTED",
		},
		{
			name:            "Tier 3 candidate at 0.86 lands AUTO_ACCEPTED",
			tier:            3,
			confidence:      0.86,
			wantReviewState: "AUTO_ACCEPTED",
		},
		{
			name:            "Tier 2 candidate at 0.86 lands NEEDS_REVIEW",
			tier:            2,
			confidence:      0.86,
			wantReviewState: "NEEDS_REVIEW",
		},
		{
			name:            "Tier 2 candidate at 0.90 lands AUTO_ACCEPTED",
			tier:            2,
			confidence:      0.90,
			wantReviewState: "AUTO_ACCEPTED",
		},
		{
			name:            "Tier 1 candidate at 0.90 lands AUTO_ACCEPTED",
			tier:            1,
			confidence:      0.90,
			wantReviewState: "AUTO_ACCEPTED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := openTestDB(t)
			ctx := context.Background()
			locationID := seedLocation(t, database)

			p := seedNode(t, database, locationID, nodeFixture{Path: "/p-" + tt.name + ".jpg", FileName: "p.jpg", FileExt: "jpg"})
			c := seedNode(t, database, locationID, nodeFixture{Path: "/c-" + tt.name + ".jpg", FileName: "c.jpg", FileExt: "jpg"})

			engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
				ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: tt.confidence, Tier: tt.tier,
				Resolver: "test-resolver", Evidence: map[string]any{},
			}})

			edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(c))
			if err != nil {
				t.Fatalf("ResolveAndCommit: %v", err)
			}
			if len(edges) != 1 {
				t.Fatalf("edges count = %d, want 1", len(edges))
			}
			if edges[0].ReviewState != tt.wantReviewState {
				t.Errorf("ReviewState = %q, want %q", edges[0].ReviewState, tt.wantReviewState)
			}
		})
	}
}

// TestResolveAndCommitCreatedCountOnlyCountsNewEdges backs scan_jobs.edges_created:
// resolving the same (parent, child) pair twice must only count the first
// call as a creation -- the second call's UpsertMediaEdge touches an
// existing row (refreshing confidence/evidence, per media_edges_resolve.sql's
// own doc comment), which is not a new edge even though it's still returned
// in committed.
func TestResolveAndCommitCreatedCountOnlyCountsNewEdges(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	p := seedNode(t, database, locationID, nodeFixture{Path: "/p.jpg", FileName: "p.jpg", FileExt: "jpg"})
	c := seedNode(t, database, locationID, nodeFixture{Path: "/c.jpg", FileName: "c.jpg", FileExt: "jpg"})

	engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		Resolver: "fixed", Evidence: map[string]any{},
	}})

	edges, created, err := engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (first): %v", err)
	}
	if len(edges) != 1 || created != 1 {
		t.Fatalf("first call: got %d edges, %d created, want 1, 1", len(edges), created)
	}

	edges, created, err = engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (second): %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("second call: got %d edges, want 1 (the same edge, refreshed)", len(edges))
	}
	if created != 0 {
		t.Errorf("second call: created = %d, want 0 (edge already existed)", created)
	}
}

// TestConfirmedEdgeSurvivesRescan backs H1: UpsertMediaEdge's WHERE-gated DO
// UPDATE returns sql.ErrNoRows (not the pre-existing row) for a
// CONFIRMED/REJECTED edge, and ResolveAndCommit must treat that as "this one
// candidate is locked, keep going" rather than aborting the whole batch --
// dropping every OTHER candidate edge for the same child node along with it.
func TestConfirmedEdgeSurvivesRescan(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	p1 := seedNode(t, database, locationID, nodeFixture{Path: "/p1.jpg", FileName: "p1.jpg", FileExt: "jpg"})
	p2 := seedNode(t, database, locationID, nodeFixture{Path: "/p2.jpg", FileName: "p2.jpg", FileExt: "jpg"})
	c := seedNode(t, database, locationID, nodeFixture{Path: "/c.jpg", FileName: "c.jpg", FileExt: "jpg"})

	engine := NewEngine(database, nil,
		fixedCandidateResolver{Candidate{
			ParentID: p1.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.60, Tier: 2,
			Resolver: "r1", Evidence: map[string]any{},
		}},
		fixedCandidateResolver{Candidate{
			ParentID: p2.ID, ChildID: c.ID, Rel: "PROXY_OF", Confidence: 0.95, Tier: 1,
			Resolver: "r2", Evidence: map[string]any{},
		}},
	)

	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (first): %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("first call: got %d edges, want 2", len(edges))
	}

	var p1EdgeID int64
	for _, e := range edges {
		if e.SourceNodeID == p1.ID {
			p1EdgeID = e.ID
		}
	}
	if p1EdgeID == 0 {
		t.Fatalf("no edge found from p1 (%d) in %+v", p1.ID, edges)
	}

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{ID: p1EdgeID, ReviewedBy: sql.NullString{String: "operator", Valid: true}})
		return err
	}); err != nil {
		t.Fatalf("ConfirmMediaEdge: %v", err)
	}

	// Re-resolving must NOT error and must NOT drop the p2 edge just
	// because the p1 edge is now human-locked.
	edges, created, err := engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (second, after confirm): %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("second call: got %d edges, want 2 (confirm must not drop the other candidate)", len(edges))
	}
	if created != 0 {
		t.Errorf("second call: created = %d, want 0 (both edges already existed -- the human-locked one must not be miscounted as new)", created)
	}

	for _, e := range edges {
		if e.ID == p1EdgeID {
			if e.ReviewState != "CONFIRMED" {
				t.Errorf("confirmed edge review_state = %q, want CONFIRMED (must not be touched by a later resolve pass)", e.ReviewState)
			}
			if e.Confidence != 0.60 {
				t.Errorf("confirmed edge confidence = %v, want unchanged 0.60", e.Confidence)
			}
		}
	}

	childAfter, err := database.Reader.GetMediaNodeByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("get child after rescan: %v", err)
	}
	if childAfter.GraphStatus != "LINKED" {
		t.Errorf("child graph_status = %q, want LINKED", childAfter.GraphStatus)
	}
}

// TestRejectedEdgeDoesNotForceGraphStatusToNeedsReview backs the
// human-locked-edge fix's own regression: a REJECTED edge is a decision
// about ONE candidate, not a statement that the node needs review. If the
// only edge committed in a resolve pass happens to be a previously-REJECTED
// one (e.g. every other candidate this pass was below needsReviewFloor or
// would have closed a cycle), graph_status must not be forced to
// NEEDS_REVIEW -- that would show the node as "needing review" in the asset
// list while nothing actually appears in the audit queue to resolve it,
// since the audit queue filters on edge-level review_state, not
// graph_status.
func TestRejectedEdgeDoesNotForceGraphStatusToNeedsReview(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	p := seedNode(t, database, locationID, nodeFixture{Path: "/p-reject.jpg", FileName: "p.jpg", FileExt: "jpg"})
	c := seedNode(t, database, locationID, nodeFixture{Path: "/c-reject.jpg", FileName: "c.jpg", FileExt: "jpg"})

	engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		Resolver: "r1", Evidence: map[string]any{},
	}})

	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (first): %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("first call: got %d edges, want 1", len(edges))
	}
	edgeID := edges[0].ID

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{ID: edgeID, ReviewedBy: sql.NullString{String: "operator", Valid: true}})
		return err
	}); err != nil {
		t.Fatalf("RejectMediaEdge: %v", err)
	}

	childAfterReject, err := database.Reader.GetMediaNodeByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("get child after reject: %v", err)
	}
	statusBeforeRescan := childAfterReject.GraphStatus

	// Re-resolving proposes the SAME candidate again (the resolver is
	// deterministic), which is now human-locked -- the only edge in
	// `committed` this pass is REJECTED.
	edges, created, err := engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (second, after reject): %v", err)
	}
	if len(edges) != 1 || edges[0].ReviewState != "REJECTED" {
		t.Fatalf("second call: got %+v, want one REJECTED edge", edges)
	}
	if created != 0 {
		t.Errorf("second call: created = %d, want 0", created)
	}

	childAfterRescan, err := database.Reader.GetMediaNodeByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("get child after rescan: %v", err)
	}
	if childAfterRescan.GraphStatus != statusBeforeRescan {
		t.Errorf("graph_status changed from %q to %q after a REJECTED-only resolve pass, want unchanged", statusBeforeRescan, childAfterRescan.GraphStatus)
	}
	if childAfterRescan.GraphStatus == "NEEDS_REVIEW" {
		t.Error("graph_status = NEEDS_REVIEW, but the only edge is REJECTED -- nothing will ever appear in the audit queue to resolve this")
	}
}

// TestUpsertMediaEdgeKeepsStrongerPriorPass backs H2: a later resolve pass
// that only proposes a WEAKER candidate for an edge already committed at a
// higher confidence must not downgrade that edge's tier/resolver/
// evidence_json/review_state, even though confidence itself (MAX'd) never
// regresses. Simulates the concrete failure mode: a Tier-3 candidate at
// 0.89 lands AUTO_ACCEPTED on pass 1; pass 2's pHash extraction hiccups so
// only a Tier-2 filename_stem candidate at 0.60 fires for the same pair.
func TestUpsertMediaEdgeKeepsStrongerPriorPass(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	p := seedNode(t, database, locationID, nodeFixture{Path: "/p-downgrade.jpg", FileName: "p.jpg", FileExt: "jpg"})
	c := seedNode(t, database, locationID, nodeFixture{Path: "/c-downgrade.jpg", FileName: "c.jpg", FileExt: "jpg"})

	strongEngine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.89, Tier: 3,
		Resolver: "heuristic_spatial_temporal", Evidence: map[string]any{"pass": "1"},
	}})
	edges, _, err := strongEngine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (pass 1, strong): %v", err)
	}
	if len(edges) != 1 || edges[0].ReviewState != "AUTO_ACCEPTED" || edges[0].Confidence != 0.89 {
		t.Fatalf("pass 1: got %+v, want one AUTO_ACCEPTED edge at 0.89", edges)
	}

	weakEngine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.60, Tier: 2,
		Resolver: "filename_stem", Evidence: map[string]any{"pass": "2"},
	}})
	edges, _, err = weakEngine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (pass 2, weak): %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("pass 2: got %d edges, want 1", len(edges))
	}
	edge := edges[0]
	if edge.Confidence != 0.89 {
		t.Errorf("confidence = %v, want 0.89 (MAX must never regress)", edge.Confidence)
	}
	if edge.Tier != 3 {
		t.Errorf("tier = %d, want 3 (the winning pass's tier, not the losing pass's)", edge.Tier)
	}
	if edge.Resolver != "heuristic_spatial_temporal" {
		t.Errorf("resolver = %q, want heuristic_spatial_temporal (the winning pass's resolver)", edge.Resolver)
	}
	if edge.ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED (a weaker later pass must not downgrade it)", edge.ReviewState)
	}
}

// TestUpsertMediaEdgeUpgradesOnStrongerPass is TestUpsertMediaEdgeKeepsStrongerPriorPass's
// mirror: a genuinely STRONGER later pass must still win and its
// tier/resolver/evidence/review_state must all be adopted together, not
// left half-updated.
func TestUpsertMediaEdgeUpgradesOnStrongerPass(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	p := seedNode(t, database, locationID, nodeFixture{Path: "/p-upgrade.jpg", FileName: "p.jpg", FileExt: "jpg"})
	c := seedNode(t, database, locationID, nodeFixture{Path: "/c-upgrade.jpg", FileName: "c.jpg", FileExt: "jpg"})

	weakEngine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.60, Tier: 2,
		Resolver: "filename_stem", Evidence: map[string]any{"pass": "1"},
	}})
	if _, _, err := weakEngine.ResolveAndCommit(ctx, asGraphNode(c)); err != nil {
		t.Fatalf("ResolveAndCommit (pass 1, weak): %v", err)
	}

	strongEngine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.95, Tier: 1,
		Resolver: "xmp_original_document_id", Evidence: map[string]any{"pass": "2"},
	}})
	edges, _, err := strongEngine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (pass 2, strong): %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("pass 2: got %d edges, want 1", len(edges))
	}
	edge := edges[0]
	if edge.Confidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95", edge.Confidence)
	}
	if edge.Tier != 1 {
		t.Errorf("tier = %d, want 1 (the stronger pass's tier)", edge.Tier)
	}
	if edge.Resolver != "xmp_original_document_id" {
		t.Errorf("resolver = %q, want xmp_original_document_id", edge.Resolver)
	}
	if edge.ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED", edge.ReviewState)
	}
}

// TestUpsertMediaEdgeTieDoesNotDowngradeAutoAccept backs a Hermes review
// finding on PR #128: per-tier auto-accept thresholds differ (0.85 for
// Tier 3, 0.90 otherwise), so an EXACT confidence tie between a stored
// Tier-3 AUTO_ACCEPTED edge and a later Tier-2 candidate at that same
// confidence must not let the tie-break hand the row to the weaker-tier
// candidate -- confidence wouldn't regress (still 0.89), but review_state
// would, from AUTO_ACCEPTED to NEEDS_REVIEW, which is exactly the class of
// bug UpsertMediaEdge's WHERE-keyed CASE exists to prevent. And because
// tier/resolver/evidence_json/review_state are always adopted together
// (never independently), the blocked tie must leave the ORIGINAL Tier-3
// provenance fully intact, not a mix of new tier/resolver with stale
// review_state.
func TestUpsertMediaEdgeTieDoesNotDowngradeAutoAccept(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	p := seedNode(t, database, locationID, nodeFixture{Path: "/p-tie.jpg", FileName: "p.jpg", FileExt: "jpg"})
	c := seedNode(t, database, locationID, nodeFixture{Path: "/c-tie.jpg", FileName: "c.jpg", FileExt: "jpg"})

	tier3Engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.89, Tier: 3,
		Resolver: "heuristic_spatial_temporal", Evidence: map[string]any{"pass": "1"},
	}})
	edges, _, err := tier3Engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (pass 1, tier 3 @ 0.89): %v", err)
	}
	if len(edges) != 1 || edges[0].ReviewState != "AUTO_ACCEPTED" {
		t.Fatalf("pass 1: got %+v, want one AUTO_ACCEPTED edge", edges)
	}

	// Same confidence (0.89), lower tier: 0.89 clears Tier 3's 0.85
	// threshold but not Tier 2's 0.90, so Engine computes NEEDS_REVIEW for
	// THIS candidate even though its confidence ties the stored value.
	tier2Engine := NewEngine(database, nil, fixedCandidateResolver{Candidate{
		ParentID: p.ID, ChildID: c.ID, Rel: "DERIVED_FROM", Confidence: 0.89, Tier: 2,
		Resolver: "filename_stem", Evidence: map[string]any{"pass": "2"},
	}})
	edges, _, err = tier2Engine.ResolveAndCommit(ctx, asGraphNode(c))
	if err != nil {
		t.Fatalf("ResolveAndCommit (pass 2, tier 2 @ 0.89 tie): %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("pass 2: got %d edges, want 1", len(edges))
	}
	edge := edges[0]
	if edge.Confidence != 0.89 {
		t.Errorf("confidence = %v, want 0.89 (unchanged)", edge.Confidence)
	}
	if edge.ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("review_state = %q, want AUTO_ACCEPTED (a same-confidence, lower-tier candidate must not downgrade it)", edge.ReviewState)
	}
	if edge.Tier != 3 {
		t.Errorf("tier = %d, want 3 (original provenance must stay intact when the tie is blocked)", edge.Tier)
	}
	if edge.Resolver != "heuristic_spatial_temporal" {
		t.Errorf("resolver = %q, want heuristic_spatial_temporal (original provenance must stay intact when the tie is blocked)", edge.Resolver)
	}
}

func TestLookupBySpatialTemporal(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	baseTime := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	timePlus1s := baseTime.Add(1 * time.Second)
	timePlus5s := baseTime.Add(5 * time.Second)
	phashVal := int64(123456789)

	matchParent := seedNode(t, database, locationID, nodeFixture{
		Path:         "/match_parent.arw",
		FileName:     "match_parent.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL123",
		LensModel:    "FE 24-70mm F2.8 GM",
		CapturedAt:   &baseTime,
		PHash:        &phashVal,
	})

	_ = seedNode(t, database, locationID, nodeFixture{
		Path:         "/outside_window.arw",
		FileName:     "outside_window.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL123",
		CapturedAt:   &timePlus5s,
	})

	_ = seedNode(t, database, locationID, nodeFixture{
		Path:         "/different_serial.arw",
		FileName:     "different_serial.arw",
		FileExt:      "arw",
		CameraSerial: "OTHER_SERIAL",
		CapturedAt:   &baseTime,
	})

	archivedParent := seedNode(t, database, locationID, nodeFixture{
		Path:         "/archived.arw",
		FileName:     "archived.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL123",
		CapturedAt:   &baseTime,
	})

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.ArchiveMediaNode(ctx, archivedParent.ID)
	}); err != nil {
		t.Fatalf("ArchiveMediaNode: %v", err)
	}

	childNode := seedNode(t, database, locationID, nodeFixture{
		Path:         "/child.jpg",
		FileName:     "child.jpg",
		FileExt:      "jpg",
		CameraSerial: "SERIAL123",
		LensModel:    "FE 24-70mm F2.8 GM",
		CapturedAt:   &timePlus1s,
	})

	lookup := NewLookup(database.Reader)

	// Guard checks: empty serial or zero time return empty slice
	noSerial, err := lookup.BySpatialTemporal(ctx, "", timePlus1s, 2*time.Second, childNode.ID)
	if err != nil || len(noSerial) != 0 {
		t.Errorf("empty cameraSerial returned %d candidates, err: %v, want 0", len(noSerial), err)
	}
	zeroTime, err := lookup.BySpatialTemporal(ctx, "SERIAL123", time.Time{}, 2*time.Second, childNode.ID)
	if err != nil || len(zeroTime) != 0 {
		t.Errorf("zero capturedAt returned %d candidates, err: %v, want 0", len(zeroTime), err)
	}

	candidates, err := lookup.BySpatialTemporal(ctx, "SERIAL123", timePlus1s, 2*time.Second, childNode.ID)
	if err != nil {
		t.Fatalf("BySpatialTemporal: %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("candidates count = %d, want 1", len(candidates))
	}

	c := candidates[0]
	if c.ID != matchParent.ID {
		t.Errorf("candidate ID = %d, want %d", c.ID, matchParent.ID)
	}
	if c.CameraSerial != "SERIAL123" {
		t.Errorf("CameraSerial = %q, want SERIAL123", c.CameraSerial)
	}
	if c.LensModel != "FE 24-70mm F2.8 GM" {
		t.Errorf("LensModel = %q, want FE 24-70mm F2.8 GM", c.LensModel)
	}
	if c.PHash == nil || *c.PHash != phashVal {
		t.Errorf("PHash = %v, want %d", c.PHash, phashVal)
	}
}

func TestHeuristicSpatialTemporalResolver(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Second)
	phashParent := int64(0x0000000000000000)
	phashClose := int64(0x000000000000000F) // Hamming distance 4 (<= 10)
	phashFar := int64(0x00000000FFFFFFFF)   // Hamming distance 32 (> 10)

	// Parent nodes
	parentFull := seedNode(t, database, locationID, nodeFixture{
		Path:         "/parent_full.arw",
		FileName:     "parent_full.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL_XYZ",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t0,
		PHash:        &phashParent,
	})

	parentNoPHash := seedNode(t, database, locationID, nodeFixture{
		Path:         "/parent_nophash.arw",
		FileName:     "parent_nophash.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL_XYZ2",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t0,
	})

	_ = seedNode(t, database, locationID, nodeFixture{
		Path:         "/parent_far.arw",
		FileName:     "parent_far.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL_XYZ3",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t0,
		PHash:        &phashParent,
	})

	// Children nodes
	childFull := seedNode(t, database, locationID, nodeFixture{
		Path:         "/child_full.jpg",
		FileName:     "child_full.jpg",
		FileExt:      "jpg",
		CameraSerial: "SERIAL_XYZ",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t1,
		PHash:        &phashClose,
	})

	childNoPHash := seedNode(t, database, locationID, nodeFixture{
		Path:         "/child_nophash.jpg",
		FileName:     "child_nophash.jpg",
		FileExt:      "jpg",
		CameraSerial: "SERIAL_XYZ2",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t1,
	})

	childFarPHash := seedNode(t, database, locationID, nodeFixture{
		Path:         "/child_far.jpg",
		FileName:     "child_far.jpg",
		FileExt:      "jpg",
		CameraSerial: "SERIAL_XYZ3",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t1,
		PHash:        &phashFar,
	})

	r := HeuristicSpatialTemporalResolver{}
	engine := NewEngine(database, nil, r)

	// Case 1: Full match (Serial + Lens + Time + pHash <= 10) -> score 0.89 -> AUTO_ACCEPTED
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childFull))
	if err != nil {
		t.Fatalf("ResolveAndCommit full: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("full match edges = %d, want 1", len(edges))
	}
	if edges[0].SourceNodeID != parentFull.ID {
		t.Errorf("SourceNodeID = %d, want %d", edges[0].SourceNodeID, parentFull.ID)
	}
	if edges[0].Confidence != 0.89 {
		t.Errorf("Confidence = %v, want 0.89", edges[0].Confidence)
	}
	if edges[0].ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("ReviewState = %q, want AUTO_ACCEPTED", edges[0].ReviewState)
	}

	// Case 2: Missing pHash (NULL phash) -> capped at 0.79 -> NEEDS_REVIEW
	edgesNoPHash, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childNoPHash))
	if err != nil {
		t.Fatalf("ResolveAndCommit no pHash: %v", err)
	}
	if len(edgesNoPHash) != 1 {
		t.Fatalf("no pHash edges = %d, want 1", len(edgesNoPHash))
	}
	if edgesNoPHash[0].SourceNodeID != parentNoPHash.ID {
		t.Errorf("SourceNodeID = %d, want %d", edgesNoPHash[0].SourceNodeID, parentNoPHash.ID)
	}
	if edgesNoPHash[0].Confidence != 0.79 {
		t.Errorf("Confidence = %v, want 0.79 (capped below 0.80)", edgesNoPHash[0].Confidence)
	}
	if edgesNoPHash[0].ReviewState != "NEEDS_REVIEW" {
		t.Errorf("ReviewState = %q, want NEEDS_REVIEW", edgesNoPHash[0].ReviewState)
	}

	// Case 3: Far pHash (Hamming distance > 10) -> no candidate emitted
	edgesFar, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childFarPHash))
	if err != nil {
		t.Fatalf("ResolveAndCommit far pHash: %v", err)
	}
	if len(edgesFar) != 0 {
		t.Errorf("far pHash emitted %d candidates, want 0 (Hamming > 10 dropped)", len(edgesFar))
	}

	// Case 4: Base-only match (Serial + Time + pHash <= 10, but different lens) -> score 0.79 -> NEEDS_REVIEW
	parentDiffLens := seedNode(t, database, locationID, nodeFixture{
		Path:         "/parent_difflens.arw",
		FileName:     "parent_difflens.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL_XYZ4",
		LensModel:    "FE 24mm F1.4 GM",
		CapturedAt:   &t0,
		PHash:        &phashParent,
	})
	childDiffLens := seedNode(t, database, locationID, nodeFixture{
		Path:         "/child_difflens.jpg",
		FileName:     "child_difflens.jpg",
		FileExt:      "jpg",
		CameraSerial: "SERIAL_XYZ4",
		LensModel:    "FE 50mm F1.2 GM",
		CapturedAt:   &t1,
		PHash:        &phashClose,
	})
	edgesDiffLens, _, err := engine.ResolveAndCommit(ctx, asGraphNode(childDiffLens))
	if err != nil {
		t.Fatalf("ResolveAndCommit diff lens: %v", err)
	}
	if len(edgesDiffLens) != 1 {
		t.Fatalf("diff lens edges = %d, want 1", len(edgesDiffLens))
	}
	if edgesDiffLens[0].SourceNodeID != parentDiffLens.ID {
		t.Errorf("SourceNodeID = %d, want %d", edgesDiffLens[0].SourceNodeID, parentDiffLens.ID)
	}
	if edgesDiffLens[0].Confidence != 0.79 {
		t.Errorf("Confidence = %v, want 0.79 (0.70 base + 0.09 phash)", edgesDiffLens[0].Confidence)
	}
	if edgesDiffLens[0].ReviewState != "NEEDS_REVIEW" {
		t.Errorf("ReviewState = %q, want NEEDS_REVIEW", edgesDiffLens[0].ReviewState)
	}

	// Case 5: Guard checks (empty serial / nil capture time)
	noSerialCandidates, err := r.Resolve(ctx, Node{ID: 99, CameraSerial: "", CapturedAt: &t0}, NewLookup(database.Reader))
	if err != nil || len(noSerialCandidates) != 0 {
		t.Errorf("empty serial returned %d candidates, want 0", len(noSerialCandidates))
	}
	noTimeCandidates, err := r.Resolve(ctx, Node{ID: 99, CameraSerial: "SERIAL_XYZ", CapturedAt: nil}, NewLookup(database.Reader))
	if err != nil || len(noTimeCandidates) != 0 {
		t.Errorf("nil CapturedAt returned %d candidates, want 0", len(noTimeCandidates))
	}
}

// TestPromotedCapturedAtUnixEnablesTier3MatchAfterReconcile backs #204: a
// child indexed before it had a capture time (the inherit-metadata case --
// node_metadata can carry an inherited EXIF:DateTimeOriginal while
// captured_at_unix stays NULL until the pipeline's touched-branch reconcile,
// internal/pipeline.reconcilePromotedColumns, promotes it) is invisible to
// HeuristicSpatialTemporalResolver, since ListTier3Candidates matches on
// camera_serial + captured_at_unix via ix_media_nodes_camera_time and a NULL
// captured_at_unix can never fall inside anyone's ±2s window. Once that
// column is promoted -- exercised here via the same
// UpdateMediaNodePromotedColumns query the pipeline uses, not raw SQL -- a
// re-resolve of the same child finds the match. There is no test elsewhere
// that re-resolves after a node field changes underneath an already-seeded
// node; this is that case.
func TestPromotedCapturedAtUnixEnablesTier3MatchAfterReconcile(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	phash := int64(0x0000000000000000)

	parent := seedNode(t, database, locationID, nodeFixture{
		Path:         "/parent.arw",
		FileName:     "parent.arw",
		FileExt:      "arw",
		CameraSerial: "SERIAL_ABC",
		LensModel:    "FE 85mm F1.4 GM",
		CapturedAt:   &t0,
		PHash:        &phash,
	})

	// Child seeded with no CapturedAt at all -- captured_at_unix NULL, as if
	// indexed before exiftool derived a capture time (or before an
	// inherit-metadata call copied one in).
	child := seedNode(t, database, locationID, nodeFixture{
		Path:         "/child.jpg",
		FileName:     "child.jpg",
		FileExt:      "jpg",
		CameraSerial: "SERIAL_ABC",
		LensModel:    "FE 85mm F1.4 GM",
		PHash:        &phash,
	})
	if child.CapturedAtUnix.Valid {
		t.Fatalf("pre-condition broken: child already has captured_at_unix set (%+v)", child.CapturedAtUnix)
	}

	engine := NewEngine(database, nil, HeuristicSpatialTemporalResolver{})

	edgesBefore, _, err := engine.ResolveAndCommit(ctx, asGraphNode(child))
	if err != nil {
		t.Fatalf("ResolveAndCommit (captured_at_unix NULL): %v", err)
	}
	if len(edgesBefore) != 0 {
		t.Fatalf("edges before promotion = %d, want 0 (NULL captured_at_unix can't fall in any ±2s window)", len(edgesBefore))
	}

	// Promote captured_at_unix the same way the pipeline's touched-branch
	// reconcile does -- via UpdateMediaNodePromotedColumns, not raw SQL --
	// landing the child within the parent's ±2s window.
	t1 := t0.Add(1 * time.Second)
	// Pass every column as an effective value, not just the one under test --
	// the query's contract (see its doc comment) is that the caller always
	// passes a full effective set, never a partial one; a partial call here
	// would otherwise model (and silently normalize) misuse of the query.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.UpdateMediaNodePromotedColumns(ctx, sqlcgen.UpdateMediaNodePromotedColumnsParams{
			ID:                 child.ID,
			OriginalDocumentID: child.OriginalDocumentID,
			DocumentID:         child.DocumentID,
			DerivedFromID:      child.DerivedFromID,
			CameraModel:        child.CameraModel,
			CameraSerial:       child.CameraSerial,
			LensModel:          child.LensModel,
			CapturedAtUnix:     sql.NullInt64{Int64: t1.Unix(), Valid: true},
		})
	}); err != nil {
		t.Fatalf("UpdateMediaNodePromotedColumns: %v", err)
	}

	reconciledChild, err := database.Reader.GetLiveNodeByPath(ctx, "/child.jpg")
	if err != nil {
		t.Fatalf("re-fetch child: %v", err)
	}
	if !reconciledChild.CapturedAtUnix.Valid || reconciledChild.CapturedAtUnix.Int64 != t1.Unix() {
		t.Fatalf("pre-condition broken: promotion didn't take (%+v)", reconciledChild.CapturedAtUnix)
	}

	edgesAfter, _, err := engine.ResolveAndCommit(ctx, asGraphNode(reconciledChild))
	if err != nil {
		t.Fatalf("ResolveAndCommit (captured_at_unix promoted): %v", err)
	}
	if len(edgesAfter) != 1 {
		t.Fatalf("edges after promotion = %d, want 1 (the match the resolver couldn't previously see)", len(edgesAfter))
	}
	if edgesAfter[0].SourceNodeID != parent.ID {
		t.Errorf("SourceNodeID = %d, want %d", edgesAfter[0].SourceNodeID, parent.ID)
	}
	if edgesAfter[0].ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("ReviewState = %q, want AUTO_ACCEPTED", edgesAfter[0].ReviewState)
	}
}

// TestHeuristicSpatialTemporalResolverSameFormatBurstProducesNoEdges backs
// #162: a continuous-drive burst of same-format frames (all raw, here) must
// produce zero candidates -- with no raw->export role asymmetry, there is
// no principled way to pick a direction, so the resolver must decline
// rather than link them arbitrarily.
func TestHeuristicSpatialTemporalResolverSameFormatBurstProducesNoEdges(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	phash := int64(0x0000000000000000)

	var last sqlcgen.MediaNode
	for i := 0; i < 6; i++ {
		capturedAt := t0.Add(time.Duration(i) * 200 * time.Millisecond)
		last = seedNode(t, database, locationID, nodeFixture{
			Path: fmt.Sprintf("/burst_%d.arw", i), FileName: fmt.Sprintf("burst_%d.arw", i), FileExt: "arw",
			CameraSerial: "SERIAL_BURST", LensModel: "FE 85mm F1.4 GM", CapturedAt: &capturedAt, PHash: &phash,
		})
	}

	engine := NewEngine(database, nil, HeuristicSpatialTemporalResolver{})
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(last))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("got %d edges among 6 same-format (.arw) burst siblings, want 0: %+v", len(edges), edges)
	}
}

// TestHeuristicSpatialTemporalResolverPicksSingleBestRawMatch backs #162: a
// mixed-format burst (several raw candidates in-window for one export
// child) must not still produce a small cross-linked mesh -- the resolver
// picks exactly one match, the lowest Hamming distance.
func TestHeuristicSpatialTemporalResolverPicksSingleBestRawMatch(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Second)
	phashChild := int64(0x0000000000000000)
	phashClosest := int64(0x0000000000000003)  // Hamming distance 2
	phashMiddle := int64(0x000000000000000F)   // Hamming distance 4
	phashFarthest := int64(0x00000000000000FF) // Hamming distance 8

	seedNode(t, database, locationID, nodeFixture{
		Path: "/parent_middle.arw", FileName: "parent_middle.arw", FileExt: "arw",
		CameraSerial: "SERIAL_MULTI", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t0, PHash: &phashMiddle,
	})
	parentClosest := seedNode(t, database, locationID, nodeFixture{
		Path: "/parent_closest.arw", FileName: "parent_closest.arw", FileExt: "arw",
		CameraSerial: "SERIAL_MULTI", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t0, PHash: &phashClosest,
	})
	seedNode(t, database, locationID, nodeFixture{
		Path: "/parent_farthest.arw", FileName: "parent_farthest.arw", FileExt: "arw",
		CameraSerial: "SERIAL_MULTI", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t0, PHash: &phashFarthest,
	})

	child := seedNode(t, database, locationID, nodeFixture{
		Path: "/child.jpg", FileName: "child.jpg", FileExt: "jpg",
		CameraSerial: "SERIAL_MULTI", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t1, PHash: &phashChild,
	})

	engine := NewEngine(database, nil, HeuristicSpatialTemporalResolver{})
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(child))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges among 3 in-window raw candidates, want 1 (single best match): %+v", len(edges), edges)
	}
	if edges[0].SourceNodeID != parentClosest.ID {
		t.Errorf("SourceNodeID = %d, want %d (lowest Hamming distance)", edges[0].SourceNodeID, parentClosest.ID)
	}
}

// TestHeuristicSpatialTemporalResolverFullTieBreaksOnLowestParentID backs
// #162: two RAW parents that tie on confidence, Hamming distance, AND time
// delta -- a real scenario for burst siblings sharing lens/pHash/timestamp,
// not a hypothetical -- must resolve to a stable winner rather than falling
// back to whatever order the underlying SQL scan happens to return rows in
// (ListTier3Candidates has no ORDER BY). Run with -count=1 so this can't
// pass by coincidentally matching a cached, order-dependent result.
func TestHeuristicSpatialTemporalResolverFullTieBreaksOnLowestParentID(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database)

	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Second)
	phash := int64(0x0000000000000000)

	parentA := seedNode(t, database, locationID, nodeFixture{
		Path: "/parent_a.arw", FileName: "parent_a.arw", FileExt: "arw",
		CameraSerial: "SERIAL_TIE", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t0, PHash: &phash,
	})
	parentB := seedNode(t, database, locationID, nodeFixture{
		Path: "/parent_b.arw", FileName: "parent_b.arw", FileExt: "arw",
		CameraSerial: "SERIAL_TIE", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t0, PHash: &phash,
	})
	child := seedNode(t, database, locationID, nodeFixture{
		Path: "/child.jpg", FileName: "child.jpg", FileExt: "jpg",
		CameraSerial: "SERIAL_TIE", LensModel: "FE 85mm F1.4 GM", CapturedAt: &t1, PHash: &phash,
	})

	wantWinner := parentA.ID
	if parentB.ID < wantWinner {
		wantWinner = parentB.ID
	}

	engine := NewEngine(database, nil, HeuristicSpatialTemporalResolver{})
	edges, _, err := engine.ResolveAndCommit(ctx, asGraphNode(child))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges among 2 fully-tied raw candidates, want 1: %+v", len(edges), edges)
	}
	if edges[0].SourceNodeID != wantWinner {
		t.Errorf("SourceNodeID = %d, want %d (lowest parent node ID, the final tie-break)", edges[0].SourceNodeID, wantWinner)
	}
}
