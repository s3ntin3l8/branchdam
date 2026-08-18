package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/probe"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.db")
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

func seedLocation(t *testing.T, database *db.DB, tier string, readOnly bool) int64 {
	t.Helper()
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		ro := int64(0)
		if readOnly {
			ro = 1
		}
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     tier + "-" + t.Name(),
			RootPath: t.TempDir(),
			Tier:     tier,
			ReadOnly: ro,
			Prunable: 0,
		})
		if err != nil {
			return err
		}
		id = loc.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed storage location: %v", err)
	}
	return id
}

func mustGetLiveNode(t *testing.T, database *db.DB, path string) sqlcgen.MediaNode {
	t.Helper()
	node, err := database.Reader.GetLiveNodeByPath(context.Background(), path)
	if err != nil {
		t.Fatalf("GetLiveNodeByPath(%q): %v", path, err)
	}
	return node
}

// TestVersionCollisionArchivesOldAndPreservesEdges is T5 (build plan): a
// re-export over an existing filename must create a new node, archive the
// old one, and -- critically -- leave any edge attached to the old node
// intact, proving fix #6 (RESTRICT, not CASCADE) actually holds.
func TestVersionCollisionArchivesOldAndPreservesEdges(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	stats, err := Commit(ctx, database, locationID, []Result{
		{Path: "/exports/render_v1.jpg", FileName: "render_v1.jpg", FileExt: "jpg", Size: 100, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa"},
	})
	if err != nil {
		t.Fatalf("Commit (initial insert): %v", err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("stats = %+v, want Inserted=1", stats)
	}
	node1 := mustGetLiveNode(t, database, "/exports/render_v1.jpg")

	// A second, unrelated node to be node1's CHILD via a DERIVED_FROM edge --
	// node1 is the PARENT (source_node_id) here, matching how a master RAW
	// (or in this case, an earlier export standing in for one) would have a
	// child.
	childStats, err := Commit(ctx, database, locationID, []Result{
		{Path: "/exports/render_v1_proxy.jpg", FileName: "render_v1_proxy.jpg", FileExt: "jpg", Size: 10, ModTime: time.Now(), FastHash: "cccccccccccccccc"},
	})
	if err != nil || childStats.Inserted != 1 {
		t.Fatalf("Commit (child node): stats=%+v err=%v", childStats, err)
	}
	child := mustGetLiveNode(t, database, "/exports/render_v1_proxy.jpg")

	var edgeID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		edge, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID:     node1.ID,
			TargetNodeID:     child.ID,
			RelationshipType: "PROXY_OF",
			Confidence:       0.9,
			Tier:             2,
			Resolver:         "test-fixture",
			EvidenceJson:     "{}",
			ReviewState:      "AUTO_ACCEPTED",
		})
		edgeID = edge.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	// Re-export over the same path with different content -- the version
	// collision.
	stats, err = Commit(ctx, database, locationID, []Result{
		{Path: "/exports/render_v1.jpg", FileName: "render_v1.jpg", FileExt: "jpg", Size: 200, ModTime: time.Now(), FastHash: "bbbbbbbbbbbbbbbb"},
	})
	if err != nil {
		t.Fatalf("Commit (version collision): %v", err)
	}
	if stats.VersionCollisions != 1 {
		t.Fatalf("stats = %+v, want VersionCollisions=1", stats)
	}

	node2 := mustGetLiveNode(t, database, "/exports/render_v1.jpg")
	if node2.ID == node1.ID {
		t.Fatal("the live node at the path is still the old node -- no new row was inserted")
	}
	if node2.FastHash == nil || *node2.FastHash != "bbbbbbbbbbbbbbbb" {
		t.Errorf("new live node fast_hash = %v, want bbbbbbbbbbbbbbbb", node2.FastHash)
	}

	// The old node must still exist (not deleted), archived, and pointing
	// at its successor.
	archived, dbErr := database.Reader.GetMediaNodeByID(ctx, node1.ID)
	if dbErr != nil {
		t.Fatalf("old node not found after archiving: %v", dbErr)
	}
	if archived.LifecycleState != "ARCHIVED" {
		t.Errorf("old node lifecycle_state = %q, want ARCHIVED", archived.LifecycleState)
	}
	if !archived.SupersededBy.Valid || archived.SupersededBy.Int64 != node2.ID {
		t.Errorf("old node superseded_by = %v, want %d", archived.SupersededBy, node2.ID)
	}

	// The load-bearing assertion: the edge attached to the OLD (now
	// archived) node must still exist. fix #6 -- RESTRICT, never CASCADE --
	// is what this proves.
	edge, err := database.Reader.GetMediaEdge(ctx, edgeID)
	if err != nil {
		t.Fatalf("edge on archived node was deleted (fix #6 violated): %v", err)
	}
	if edge.SourceNodeID != node1.ID {
		t.Errorf("edge source_node_id = %d, want %d (unchanged)", edge.SourceNodeID, node1.ID)
	}

	// A third write to the same path must succeed too -- the partial
	// unique index permits it because node2 is still live and node1 stays
	// archived, not because uniqueness stopped being enforced.
	stats, err = Commit(ctx, database, locationID, []Result{
		{Path: "/exports/render_v1.jpg", FileName: "render_v1.jpg", FileExt: "jpg", Size: 300, ModTime: time.Now(), FastHash: "dddddddddddddddd"},
	})
	if err != nil {
		t.Fatalf("Commit (second version collision): %v", err)
	}
	if stats.VersionCollisions != 1 {
		t.Fatalf("stats = %+v, want VersionCollisions=1", stats)
	}
}

// TestFastHashCollisionDoesNotMerge is T1 (spec 9.5): two distinct files at
// different live paths that happen to share a fast_hash must NOT be merged
// into one node. Commit never looks at fast_hash to decide whether two live
// nodes are "the same" -- only file_path (version collision) and
// MISSING-node fast_hash matches (move detection) trigger a merge/rebase.
// This is the regression guard for that design: submitting two Results with
// an identical fast_hash at different paths must produce two live nodes,
// each keeping its own (here: different) full_hash.
func TestFastHashCollisionDoesNotMerge(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	const sharedFastHash = "1111111111111111"
	// full_hash has a length CHECK (64 hex chars, docs/schema.md fix #8) --
	// these stand in for real BLAKE3-256 digests, just distinct from each
	// other, which is all this test needs.
	fullHashA := strings.Repeat("a", 64)
	fullHashB := strings.Repeat("b", 64)
	stats, err := Commit(ctx, database, locationID, []Result{
		{Path: "/a/one.jpg", FileName: "one.jpg", FileExt: "jpg", Size: 111, ModTime: time.Now(), FastHash: sharedFastHash, FullHash: fullHashA},
		{Path: "/b/two.jpg", FileName: "two.jpg", FileExt: "jpg", Size: 222, ModTime: time.Now(), FastHash: sharedFastHash, FullHash: fullHashB},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if stats.Inserted != 2 {
		t.Fatalf("stats = %+v, want Inserted=2 (both files, not merged)", stats)
	}

	one := mustGetLiveNode(t, database, "/a/one.jpg")
	two := mustGetLiveNode(t, database, "/b/two.jpg")
	if one.ID == two.ID {
		t.Fatal("the two files were merged into a single node")
	}
	if one.FastHash == nil || two.FastHash == nil || *one.FastHash != *two.FastHash {
		t.Fatalf("fixture invariant broken: both nodes should share fast_hash %q", sharedFastHash)
	}
	if one.FullHash == nil || two.FullHash == nil || *one.FullHash == *two.FullHash {
		t.Error("full_hash did not distinguish the two files -- escalation must have run per-file, with each file's real content")
	}
}

func TestNeedsFullHashPolicy(t *testing.T) {
	cases := []struct {
		name         string
		policy       string
		tierReadOnly bool
		hasCollision bool
		want         bool
	}{
		{"always, no signal", "always", false, false, true},
		{"never, tier3", "never", true, true, false},
		{"default tier3", "tier3_and_collision", true, false, true},
		{"default collision", "tier3_and_collision", false, true, true},
		{"default neither", "tier3_and_collision", false, false, false},
		{"unknown policy behaves like default", "bogus", true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsFullHash(c.policy, c.tierReadOnly, c.hasCollision); got != c.want {
				t.Errorf("needsFullHash(%q, %v, %v) = %v, want %v", c.policy, c.tierReadOnly, c.hasCollision, got, c.want)
			}
		})
	}
}

// TestSameContentTouchesNotDuplicates: re-scanning a file whose content
// hasn't changed must not create a second node.
func TestSameContentTouchesNotDuplicates(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	for i := 0; i < 2; i++ {
		stats, err := Commit(ctx, database, locationID, []Result{
			{Path: "/x/stable.jpg", FileName: "stable.jpg", FileExt: "jpg", Size: 50, ModTime: time.Now(), FastHash: "ffffffffffffffff"},
		})
		if err != nil {
			t.Fatalf("Commit (pass %d): %v", i, err)
		}
		if i == 0 && stats.Inserted != 1 {
			t.Fatalf("pass 0: stats = %+v, want Inserted=1", stats)
		}
		if i == 1 && stats.Touched != 1 {
			t.Fatalf("pass 1: stats = %+v, want Touched=1", stats)
		}
	}

	node := mustGetLiveNode(t, database, "/x/stable.jpg")
	if node.ID == 0 {
		t.Fatal("no live node found")
	}
}

// TestMissingNodeRebasesOnMove is Pillar 5: a node marked MISSING whose
// fast_hash reappears at a new path is rebased in place -- same id, same
// node_uuid -- rather than creating a new node.
func TestMissingNodeRebasesOnMove(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	_, err := Commit(ctx, database, locationID, []Result{
		{Path: "/old/place.jpg", FileName: "place.jpg", FileExt: "jpg", Size: 77, ModTime: time.Now(), FastHash: "eeeeeeeeeeeeeeee"},
	})
	if err != nil {
		t.Fatalf("Commit (initial): %v", err)
	}
	original := mustGetLiveNode(t, database, "/old/place.jpg")

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkNodeMissing(ctx, original.ID)
	}); err != nil {
		t.Fatalf("mark missing: %v", err)
	}

	stats, err := Commit(ctx, database, locationID, []Result{
		{Path: "/new/place.jpg", FileName: "place.jpg", FileExt: "jpg", Size: 77, ModTime: time.Now(), FastHash: "eeeeeeeeeeeeeeee"},
	})
	if err != nil {
		t.Fatalf("Commit (after move): %v", err)
	}
	if stats.Moved != 1 {
		t.Fatalf("stats = %+v, want Moved=1", stats)
	}

	moved := mustGetLiveNode(t, database, "/new/place.jpg")
	if moved.ID != original.ID {
		t.Errorf("moved node id = %d, want %d (same row, rebased)", moved.ID, original.ID)
	}
	if moved.NodeUuid != original.NodeUuid {
		t.Errorf("moved node_uuid = %q, want %q (identity must survive a move)", moved.NodeUuid, original.NodeUuid)
	}
	if moved.LifecycleState != "ACTIVE" {
		t.Errorf("moved node lifecycle_state = %q, want ACTIVE", moved.LifecycleState)
	}
}

// TestRebasedNodeBackfillsMetadata backs #86: a node indexed before
// exiftool/ffprobe were on PATH, then later moved, must gain its metadata
// on the rebase pass -- not stay permanently metadata-less just because
// RebaseMissingNodePath (unlike insertNewNode) never called persistMetadata
// before this fix.
func TestRebasedNodeBackfillsMetadata(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	// First pass: no probe data at all, as if exiftool/ffprobe were absent.
	_, err := Commit(ctx, database, locationID, []Result{
		{Path: "/old/place.jpg", FileName: "place.jpg", FileExt: "jpg", Size: 77, ModTime: time.Now(), FastHash: "eeeeeeeeeeeeeeee"},
	})
	if err != nil {
		t.Fatalf("Commit (initial, probe-less): %v", err)
	}
	original := mustGetLiveNode(t, database, "/old/place.jpg")
	if rows, err := database.Reader.ListNodeMetadata(ctx, original.ID); err != nil {
		t.Fatalf("ListNodeMetadata (initial): %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("initial metadata rows = %d, want 0", len(rows))
	}

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkNodeMissing(ctx, original.ID)
	}); err != nil {
		t.Fatalf("mark missing: %v", err)
	}

	// Second pass: same content at a new path, now with probe data -- the
	// tools were installed in between, or this is the first pass to reach
	// this file after they were.
	stats, err := Commit(ctx, database, locationID, []Result{
		{
			Path: "/new/place.jpg", FileName: "place.jpg", FileExt: "jpg", Size: 77, ModTime: time.Now(), FastHash: "eeeeeeeeeeeeeeee",
			Make: "CANON", ExifRaw: map[string]string{"EXIF:ISO": "100"},
		},
	})
	if err != nil {
		t.Fatalf("Commit (after move): %v", err)
	}
	if stats.Moved != 1 {
		t.Fatalf("stats = %+v, want Moved=1", stats)
	}

	rows, err := database.Reader.ListNodeMetadata(ctx, original.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata (after move): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("metadata rows after rebase = %d, want 2 (EXIF:Make, EXIF:ISO)", len(rows))
	}
}

// TestMarkUnseenNodesMissingScopedToLocation: the set-based MISSING sweep is
// scoped by storage_location_id -- a scan of one mount must never touch
// another mount's nodes, even when both are stale.
func TestMarkUnseenNodesMissingScopedToLocation(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	// Distinct tiers so the UNIQUE(name) constraint in storage_locations is
	// satisfied -- seedLocation names rows "tier-" + test name.
	locA := seedLocation(t, database, "TIER2_EXPORTS", false)
	locB := seedLocation(t, database, "PROJECTS", false)
	if _, err := Commit(ctx, database, locA, []Result{
		{Path: "/a/node.jpg", FileName: "node.jpg", FileExt: "jpg", Size: 1, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa"},
	}); err != nil {
		t.Fatalf("Commit A: %v", err)
	}
	if _, err := Commit(ctx, database, locB, []Result{
		{Path: "/b/node.jpg", FileName: "node.jpg", FileExt: "jpg", Size: 1, ModTime: time.Now(), FastHash: "bbbbbbbbbbbbbbbb"},
	}); err != nil {
		t.Fatalf("Commit B: %v", err)
	}
	// Backdate both nodes so a sweep at before_unix=9 would catch them.
	if _, err := database.ExecInTx(ctx, "UPDATE media_nodes SET last_seen_at = 1"); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}

	var n int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		// KeepActive is this call's "exclude these paths" list; production
		// passes the pass's seen-but-uncertain set marshaled as a JSON array string
		// into json_each(?3).
		jsonKeepActive, _ := json.Marshal([]string{""})
		n, err = q.MarkUnseenNodesMissing(ctx, sqlcgen.MarkUnseenNodesMissingParams{
			StorageLocationID: locA, LastSeenAt: 9, JsonEach: string(jsonKeepActive),
		})
		return err
	}); err != nil {
		t.Fatalf("MarkUnseenNodesMissing: %v", err)
	}
	if n != 1 {
		t.Fatalf("affected rows = %d, want 1 (only location A's stale node)", n)
	}
	if got := mustGetLiveNode(t, database, "/a/node.jpg"); got.LifecycleState != "MISSING" {
		t.Errorf("A node = %q, want MISSING", got.LifecycleState)
	}
	if got := mustGetLiveNode(t, database, "/b/node.jpg"); got.LifecycleState != "ACTIVE" {
		t.Errorf("B node = %q, want ACTIVE (untouched)", got.LifecycleState)
	}
}

func TestFilenameStem(t *testing.T) {
	cases := map[string]string{
		"DSC01234.ARW":        "dsc01234",
		"DSC01234_edited.jpg": "dsc01234_edited", // not a recognized suffix -- only the exact patterns below are stripped
		"render_v1_proxy.jpg": "render",
		"IMG_0001-2.jpg":      "img_0001",
		"IMG_0001 copy.jpg":   "img_0001",
		"IMG_0001(1).jpg":     "img_0001",
		"plain.jpg":           "plain",
		"no_extension_at_all": "no_extension_at_all",

		// H3 regression: a camera's own hyphen-numbered default naming
		// (Sony DSC-NNNN, some IMG-NNNN variants) must NOT collapse to a
		// bare "dsc"/"img" shared by every file in the shoot -- the -\d+
		// suffix branch is bounded to 1-2 digits precisely so these stay
		// distinct. See versionSuffixRe's doc comment.
		"DSC-0001.JPG": "dsc-0001",
		"IMG-1234.jpg": "img-1234",
		// The genuine "-N" duplicate-index case (OS auto-renaming, always
		// small) is still stripped -- 1-2 digits is the intended bound, not
		// a regression in disguise.
		"photo-2.jpg":  "photo",
		"photo-12.jpg": "photo",
		// 3+ digits after a hyphen reads as a camera serial/frame number,
		// not a duplicate index, and is deliberately left alone.
		"photo-123.jpg": "photo-123",

		// Known, accepted residual case (not covered by this fix): an
		// unpadded 1-2 digit hyphen-numbering scheme -- a camera counter
		// that never reaches 3 digits, or a human numbering a batch
		// "trip-1.jpg".."trip-45.jpg" -- still reads as the "-N duplicate
		// index" pattern and collapses. See versionSuffixRe's doc comment;
		// this is a documented trade-off, not an oversight.
		"DSC-01.JPG": "dsc",
		"DSC-99.JPG": "dsc",
	}
	for in, want := range cases {
		if got := filenameStem(in); got != want {
			t.Errorf("filenameStem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommitPersistsExifMetadataExactly(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	gpsLat := -33.915
	gpsLong := 18.411
	result := Result{
		Path: "/exports/shot.jpg", FileName: "shot.jpg", FileExt: "jpg",
		Size: 100, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa",
		Make:         "SONY",
		LensModel:    "FE 24-70mm F2.8 GM",
		SerialNumber: "1234567",
		GPSLatitude:  &gpsLat,
		GPSLongitude: &gpsLong,
		ExifRaw: map[string]string{
			"EXIF:ISO":          "100",
			"EXIF:JunkTag":      "must-not-persist",
			"XMP:Rating":        "5",
			"EXIF:Particularly": "also-must-not-persist",
		},
	}
	if _, err := Commit(ctx, database, locationID, []Result{result}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	node := mustGetLiveNode(t, database, "/exports/shot.jpg")

	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Source+"\x00"+r.Key] = r.Value
	}
	want := map[string]string{
		"exiftool\x00EXIF:Make":              "SONY",
		"exiftool\x00EXIF:LensModel":         "FE 24-70mm F2.8 GM",
		"exiftool\x00EXIF:SerialNumber":      "1234567",
		"exiftool\x00Composite:GPSLatitude":  "-33.915",
		"exiftool\x00Composite:GPSLongitude": "18.411",
		"exiftool\x00EXIF:ISO":               "100",
		"exiftool\x00XMP:Rating":             "5",
	}
	if len(got) != len(want) {
		t.Fatalf("metadata rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["exiftool\x00EXIF:JunkTag"]; ok {
		t.Error("EXIF:JunkTag persisted, want the allowlist to drop it")
	}
}

// TestPersistExifMetadataWritesExiftoolRows backs #157: the inherit-metadata
// endpoint rewrites a child's file in place, so after the write succeeds the
// node's node_metadata must be backfilled from the file's current EXIF --
// otherwise the DB store stays stale (empty) until the next scan and a second
// inherit call re-plans from stale values. The rows must be keyed source=
// 'exiftool' with the same grouped tag names loadTagSet reads
// (EXIF:Make, EXIF:LensModel, EXIF:SerialNumber, EXIF:ISO,
// Composite:GPSLatitude, ...), and the raw-tag allowlist must still apply.
func TestPersistExifMetadataWritesExiftoolRows(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	if _, err := Commit(ctx, database, locationID, []Result{
		{Path: "/exports/inherited.jpg", FileName: "inherited.jpg", FileExt: "jpg", Size: 100, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa"},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	node := mustGetLiveNode(t, database, "/exports/inherited.jpg")

	gpsLat, gpsLong := -33.9151, 18.4115
	exif := &probe.ExifResult{
		Make:         "SONY",
		LensModel:    "FE 24-70mm",
		SerialNumber: "123",
		// Mirrors real probe.Exif output: the Composite GPS coordinates land
		// in the typed fields, while Raw also carries the same strings.
		GPSLatitude:  &gpsLat,
		GPSLongitude: &gpsLong,
		Raw: map[string]string{
			"EXIF:ISO":               "100",
			"EXIF:JunkTag":           "must-not-persist",
			"Composite:GPSLatitude":  "-33.9151",
			"Composite:GPSLongitude": "18.4115",
		},
	}
	if err := PersistExifMetadata(ctx, database, node.ID, exif, nil); err != nil {
		t.Fatalf("PersistExifMetadata: %v", err)
	}

	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Source+"\x00"+r.Key] = r.Value
	}
	want := map[string]string{
		"exiftool\x00EXIF:Make":              "SONY",
		"exiftool\x00EXIF:LensModel":         "FE 24-70mm",
		"exiftool\x00EXIF:SerialNumber":      "123",
		"exiftool\x00EXIF:ISO":               "100",
		"exiftool\x00Composite:GPSLatitude":  "-33.9151",
		"exiftool\x00Composite:GPSLongitude": "18.4115",
	}
	if len(got) != len(want) {
		t.Fatalf("metadata rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["exiftool\x00EXIF:JunkTag"]; ok {
		t.Error("EXIF:JunkTag persisted, want the allowlist to drop it")
	}
}

func TestIsVideoExt(t *testing.T) {
	video := []string{"mp4", "mov", "mkv", "m2ts"}
	for _, ext := range video {
		if !isVideoExt(ext) {
			t.Errorf("isVideoExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"MP4", ".mkv", ".MP4"} {
		if !isVideoExt(ext) {
			t.Errorf("isVideoExt(%q) = false, want true (gate must normalize case/dot)", ext)
		}
	}
	notVideo := []string{"jpg", "arw", "mp3", "", "."}
	for _, ext := range notVideo {
		if isVideoExt(ext) {
			t.Errorf("isVideoExt(%q) = true, want false", ext)
		}
	}
}

func floatPtr(f float64) *float64 { return &f }

func TestCommitPersistsFFProbeMetadata(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	result := Result{
		Path: "/exports/clip.mp4", FileName: "clip.mp4", FileExt: "mp4",
		Size: 200, ModTime: time.Now(), FastHash: "bbbbbbbbbbbbbbbb",
		FFProbe: &probe.FFProbeResult{
			FormatName: "mov,mp4,m4a,3gp,3g2,mj2", DurationSeconds: floatPtr(1.0),
			VideoCodec: "h264", AudioCodec: "aac", Width: 320, Height: 240,
		},
	}
	if _, err := Commit(ctx, database, locationID, []Result{result}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	node := mustGetLiveNode(t, database, "/exports/clip.mp4")

	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Key] = r.Value
	}
	want := map[string]string{
		"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration_seconds": "1",
		"video_codec": "h264", "audio_codec": "aac", "width": "320", "height": "240",
	}
	if len(got) != len(want) {
		t.Fatalf("ffprobe rows = %d, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got[k], v)
		}
	}

	// A second Commit with FFProbe nil persists no ffprobe rows at all.
	if _, err := Commit(ctx, database, locationID, []Result{{
		Path: "/exports/photo.jpg", FileName: "photo.jpg", FileExt: "jpg",
		Size: 50, ModTime: time.Now(), FastHash: "cccccccccccccccc",
	}}); err != nil {
		t.Fatalf("Commit (photo): %v", err)
	}
	photo := mustGetLiveNode(t, database, "/exports/photo.jpg")
	prows, err := database.Reader.ListNodeMetadata(ctx, photo.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata (photo): %v", err)
	}
	for _, r := range prows {
		if r.Source == "ffprobe" {
			t.Errorf("photo node has an ffprobe row: %s=%s", r.Key, r.Value)
		}
	}
}

func TestPersistMetadataCapTruncates(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)
	if _, err := Commit(ctx, database, locationID, []Result{
		{Path: "/cap.jpg", FileName: "cap.jpg", FileExt: "jpg", Size: 1, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa"},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	node := mustGetLiveNode(t, database, "/cap.jpg")
	var scrubbed bytes.Buffer
	log := slog.New(slog.NewTextHandler(&scrubbed, &slog.HandlerOptions{Level: slog.LevelDebug}))

	kv := map[string]string{"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5"}
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return persistMetadata(ctx, q, node.ID, "internal", kv, 3, log)
	}); err != nil {
		t.Fatalf("persistMetadata: %v", err)
	}
	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (cap)", len(rows))
	}
	if rows[0].Key != "k1" || rows[2].Key != "k3" {
		t.Errorf("sorted truncation wrong: keys = %q %q %q", rows[0].Key, rows[1].Key, rows[2].Key)
	}
	if !strings.Contains(scrubbed.String(), "overflow dropped") {
		t.Error("expected a DEBUG overflow log line")
	}
}

func TestSameContentTouchDoesNotDuplicateMetadata(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	result := Result{
		Path: "/stable.jpg", FileName: "stable.jpg", FileExt: "jpg",
		Size: 50, ModTime: time.Now(), FastHash: "ffffffffffffffff",
		Make: "CANON",
		ExifRaw: map[string]string{
			"EXIF:ISO":   "100",
			"XMP:Rating": "5",
		},
	}
	for i := 0; i < 2; i++ {
		stats, err := Commit(ctx, database, locationID, []Result{result})
		if err != nil {
			t.Fatalf("Commit (pass %d): %v", i, err)
		}
		if i == 0 && stats.Inserted != 1 {
			t.Fatalf("pass 0: stats = %+v, want Inserted=1", stats)
		}
		if i == 1 && stats.Touched != 1 {
			t.Fatalf("pass 1: stats = %+v, want Touched=1", stats)
		}
	}

	node := mustGetLiveNode(t, database, "/stable.jpg")
	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("metadata rows = %d, want 3 (EXIF:Make, EXIF:ISO, XMP:Rating -- one set, not duplicated)", len(rows))
	}
}

// TestTouchBackfillsMetadataForProbelessFirstScan backs #86: a node first
// indexed while exiftool/ffprobe were absent from PATH stays metadata-less
// forever under its unchanged fast_hash unless the touched branch itself
// persists metadata -- installing the binaries and rescanning must
// backfill it, not require a content change. Unlike
// TestSameContentTouchDoesNotDuplicateMetadata (which starts with metadata
// present both passes and so passes regardless of whether the touched
// branch does anything), this starts with none and asserts the second pass
// adds it.
func TestTouchBackfillsMetadataForProbelessFirstScan(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	bare := Result{Path: "/stable.jpg", FileName: "stable.jpg", FileExt: "jpg", Size: 50, ModTime: time.Now(), FastHash: "ffffffffffffffff"}
	stats, err := Commit(ctx, database, locationID, []Result{bare})
	if err != nil {
		t.Fatalf("Commit (initial, probe-less): %v", err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("stats = %+v, want Inserted=1", stats)
	}
	node := mustGetLiveNode(t, database, "/stable.jpg")
	if rows, err := database.Reader.ListNodeMetadata(ctx, node.ID); err != nil {
		t.Fatalf("ListNodeMetadata (initial): %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("initial metadata rows = %d, want 0 (simulating a probe-less first scan)", len(rows))
	}

	withProbeData := bare
	withProbeData.Make = "CANON"
	withProbeData.ExifRaw = map[string]string{"EXIF:ISO": "100"}
	stats, err = Commit(ctx, database, locationID, []Result{withProbeData})
	if err != nil {
		t.Fatalf("Commit (touched, with probe data): %v", err)
	}
	if stats.Touched != 1 {
		t.Fatalf("stats = %+v, want Touched=1", stats)
	}
	if stats.MetadataWritten != 2 {
		t.Fatalf("stats = %+v, want MetadataWritten=2 (EXIF:Make, EXIF:ISO -- the backfill pass genuinely writes)", stats)
	}

	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata (after touched backfill): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("metadata rows after backfill = %d, want 2 (EXIF:Make, EXIF:ISO)", len(rows))
	}
}

// TestTouchWithUnchangedMetadataWritesNothing pins the all-unchanged case: a
// touched (unchanged fast_hash) node whose derived metadata is identical to
// what's already stored must report zero rows written on that pass, and its
// values must stay correct. Stats.MetadataWritten is the oracle --
// node_metadata has no updated_at column and #85 removed the raw-handle test
// escape hatch, so there is no other way to observe "no write happened" from
// outside the package.
//
// This test alone does NOT discriminate pre-#105 code from the fix: a
// counter that's simply never wired up would also read 0 here. The actual
// regression guards -- verified to fail against pre-#105's commit.go/
// watcher.go -- are TestTouchWithChangedMetadataWritesOnlyTheDelta (proves
// only the genuinely-changed key is written, not the whole set) and
// TestTouchBackfillsMetadataForProbelessFirstScan's MetadataWritten==2
// assertion (proves a real backfill pass is counted at all). This test's
// job is the mirror-image sanity check once those two establish the counter
// is real: an unchanged pass through the same reconcileAllMetadata code
// path is 0, not some nonzero leftover.
func TestTouchWithUnchangedMetadataWritesNothing(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	result := Result{
		Path: "/stable.jpg", FileName: "stable.jpg", FileExt: "jpg",
		Size: 50, ModTime: time.Now(), FastHash: "ffffffffffffffff",
		Make:    "CANON",
		ExifRaw: map[string]string{"EXIF:ISO": "100"},
	}
	stats, err := Commit(ctx, database, locationID, []Result{result})
	if err != nil {
		t.Fatalf("Commit (pass 1, insert): %v", err)
	}
	// insertNewNode still goes through persistAllMetadata unconditionally (a
	// brand-new node can never have prior rows, so a pre-read/diff would be
	// pure overhead) -- MetadataWritten only accumulates on the
	// reconcileAllMetadata (touched/rebase) paths, so it's 0 here regardless.
	if stats.Inserted != 1 || stats.MetadataWritten != 0 {
		t.Fatalf("pass 1: stats = %+v, want Inserted=1, MetadataWritten=0", stats)
	}

	// Pass 2: identical Result, same content, same metadata -- the ordinary
	// "re-scan an unchanged file" case.
	stats, err = Commit(ctx, database, locationID, []Result{result})
	if err != nil {
		t.Fatalf("Commit (pass 2, touched): %v", err)
	}
	if stats.Touched != 1 {
		t.Fatalf("pass 2: stats = %+v, want Touched=1", stats)
	}
	if stats.MetadataWritten != 0 {
		t.Fatalf("pass 2: stats = %+v, want MetadataWritten=0 (nothing changed)", stats)
	}

	node := mustGetLiveNode(t, database, "/stable.jpg")
	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("metadata rows = %d, want 2 (values still correct)", len(rows))
	}
}

// TestTouchWithChangedMetadataWritesOnlyTheDelta: a genuinely changed value
// (e.g. an XMP:Rating edited outside this pipeline) must still be written on
// a touched pass -- #105's diff must not become a "skip metadata on touch"
// regression of #86's fix. Only the changed key should count toward
// MetadataWritten, not the whole set.
func TestTouchWithChangedMetadataWritesOnlyTheDelta(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	first := Result{
		Path: "/rated.jpg", FileName: "rated.jpg", FileExt: "jpg",
		Size: 50, ModTime: time.Now(), FastHash: "ffffffffffffffff",
		Make:    "CANON",
		ExifRaw: map[string]string{"EXIF:ISO": "100", "XMP:Rating": "3"},
	}
	if stats, err := Commit(ctx, database, locationID, []Result{first}); err != nil {
		t.Fatalf("Commit (pass 1, insert): %v", err)
	} else if stats.Inserted != 1 || stats.MetadataWritten != 0 {
		// insertNewNode uses persistAllMetadata, not the counted
		// reconcileAllMetadata path -- see TestTouchWithUnchangedMetadataWritesNothing.
		t.Fatalf("pass 1: stats = %+v, want Inserted=1, MetadataWritten=0", stats)
	}

	changed := first
	changed.ExifRaw = map[string]string{"EXIF:ISO": "100", "XMP:Rating": "5"} // only the rating changed
	stats, err := Commit(ctx, database, locationID, []Result{changed})
	if err != nil {
		t.Fatalf("Commit (pass 2, touched, changed rating): %v", err)
	}
	if stats.Touched != 1 {
		t.Fatalf("pass 2: stats = %+v, want Touched=1", stats)
	}
	if stats.MetadataWritten != 1 {
		t.Fatalf("pass 2: stats = %+v, want MetadataWritten=1 (only XMP:Rating changed)", stats)
	}

	node := mustGetLiveNode(t, database, "/rated.jpg")
	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	got := make(map[string]string, len(rows))
	for _, r := range rows {
		got[r.Key] = r.Value
	}
	if got["XMP:Rating"] != "5" {
		t.Errorf("XMP:Rating = %q, want %q (updated)", got["XMP:Rating"], "5")
	}
	if got["EXIF:ISO"] != "100" {
		t.Errorf("EXIF:ISO = %q, want %q (unchanged)", got["EXIF:ISO"], "100")
	}
}

// TestCapTruncatedMetadataIsStableAcrossPasses: #105's diff runs on the
// already-sorted-and-capped key set, not the raw kv map -- otherwise a large,
// stable metadata set past metadataCap would spuriously "change" every pass
// as tail keys sort in and out of the capped window. Two identical passes
// with a source over the cap must write nothing on the second.
func TestCapTruncatedMetadataIsStableAcrossPasses(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	raw := make(map[string]string, metadataCap+10)
	for i := 0; i < metadataCap+10; i++ {
		raw[fmt.Sprintf("EXIF:ISO#%03d", i)] = "x" // not a real allowlisted key -- see below
	}
	// exifMetadata only persists the allowlisted tag set, so drive the cap
	// through the allowlist itself isn't practical from Result; exercise
	// reconcileMetadata/persistMetadata directly instead, exactly as
	// TestPersistMetadataCapTruncates does.
	if _, err := Commit(ctx, database, locationID, []Result{
		{Path: "/capstable.jpg", FileName: "capstable.jpg", FileExt: "jpg", Size: 1, ModTime: time.Now(), FastHash: "aaaaaaaaaaaaaaaa"},
	}); err != nil {
		t.Fatalf("Commit (insert): %v", err)
	}
	node := mustGetLiveNode(t, database, "/capstable.jpg")

	var log = slog.New(slog.DiscardHandler)
	written1 := 0
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		prior := map[metadataRowKey]string{}
		written1, err = reconcileMetadata(ctx, q, node.ID, "internal", raw, metadataCap, prior, log)
		return err
	}); err != nil {
		t.Fatalf("reconcileMetadata (pass 1): %v", err)
	}
	if written1 != metadataCap {
		t.Fatalf("pass 1 written = %d, want %d (capped)", written1, metadataCap)
	}

	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	prior := make(map[metadataRowKey]string, len(rows))
	for _, r := range rows {
		prior[metadataRowKey{r.Source, r.Key}] = r.Value
	}

	written2 := 0
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		written2, err = reconcileMetadata(ctx, q, node.ID, "internal", raw, metadataCap, prior, log)
		return err
	}); err != nil {
		t.Fatalf("reconcileMetadata (pass 2): %v", err)
	}
	if written2 != 0 {
		t.Fatalf("pass 2 written = %d, want 0 (identical, capped set is stable)", written2)
	}
}
