package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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
		// passes the pass's seen-but-uncertain set, falling back to this
		// sentinel when empty -- sqlc substitutes NULL for an empty slice and
		// file_path NOT IN (NULL) is unknown (false), so an empty list would
		// silently sweep nothing.
		n, err = q.MarkUnseenNodesMissing(ctx, sqlcgen.MarkUnseenNodesMissingParams{
			StorageLocationID: locA, BeforeUnix: 9, KeepActive: []string{""},
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
	if err := persistMetadata(ctx, database.ReaderQueriesForTest(), node.ID, "internal", kv, 3, log); err != nil {
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
