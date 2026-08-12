package graph

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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
}

// fixtureFilenameStem is a deliberate duplicate of pipeline's filenameStem
// (internal/pipeline/commit.go) -- small enough that copying it here is
// simpler and safer than restructuring package boundaries just to share it,
// and it keeps these fixtures behaving exactly like real ingested nodes
// without reintroducing the import cycle seedNode's doc comment explains.
var fixtureVersionSuffixRe = regexp.MustCompile(`(?i)(_edit|_proxy|_v\d+|-\d+| copy|\(\d+\))+$`)

func fixtureFilenameStem(fileName string) string {
	stem := fileName
	if i := strings.LastIndex(stem, "."); i > 0 {
		stem = stem[:i]
	}
	stem = strings.ToLower(strings.TrimSpace(stem))
	for {
		stripped := fixtureVersionSuffixRe.ReplaceAllString(stem, "")
		if stripped == stem {
			break
		}
		stem = stripped
	}
	return stem
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
		FilenameStem:       nullString(fixtureFilenameStem(f.FileName)),
	}
	if f.CapturedAt != nil {
		params.CapturedAtUnix = sql.NullInt64{Int64: f.CapturedAt.Unix(), Valid: true}
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
	edges, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
	if err != nil {
		t.Fatalf("ResolveAndCommit: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
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
	edges, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
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
	edges, err := engine.ResolveAndCommit(ctx, asGraphNode(childRow))
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
	edges, err := engine.ResolveAndCommit(ctx, asGraphNode(a))
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
	edges, err := engine.ResolveAndCommit(ctx, asGraphNode(b))
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
