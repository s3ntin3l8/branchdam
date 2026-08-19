package prune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prune.db")
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

func seedLocation(t *testing.T, database *db.DB, name, rootPath, tier string, readOnly, prunable bool) int64 {
	t.Helper()
	ro, pr := int64(0), int64(0)
	if readOnly {
		ro = 1
	}
	if prunable {
		pr = 1
	}
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: name, RootPath: rootPath, Tier: tier, ReadOnly: ro, Prunable: pr,
		})
		id = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location %s: %v", name, err)
	}
	return id
}

// nodeSpec is the subset of InsertMediaNodeParams a test cares about;
// everything else takes a sensible fixture default.
type nodeSpec struct {
	locationID int64
	path       string
	mtimeUnix  int64
	fullHash   *string
	lifecycle  string // defaults to ACTIVE
}

func seedNode(t *testing.T, database *db.DB, spec nodeSpec) sqlcgen.MediaNode {
	t.Helper()
	lifecycle := spec.lifecycle
	if lifecycle == "" {
		lifecycle = "ACTIVE"
	}
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-" + spec.path, StorageLocationID: spec.locationID,
			FilePath: spec.path, FileName: filepath.Base(spec.path), FileExt: "jpg",
			SizeBytes: 100, MtimeUnix: spec.mtimeUnix, FullHash: spec.fullHash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		if lifecycle != "ACTIVE" {
			if err := q.MarkNodeMissing(context.Background(), n.ID); err != nil {
				return err
			}
			if lifecycle == "ARCHIVED" {
				if err := q.ArchiveMediaNode(context.Background(), n.ID); err != nil {
					return err
				}
			}
			n, err = q.GetMediaNodeByID(context.Background(), n.ID)
			if err != nil {
				return err
			}
		}
		node = n
		return nil
	})
	if err != nil {
		t.Fatalf("seed node %s: %v", spec.path, err)
	}
	return node
}

// seedEdge creates a PROXY_OF edge (ancestor=source, descendant=target).
// REJECTED is not a valid initial review_state -- the schema's own CHECK
// requires reviewed_at whenever review_state is CONFIRMED/REJECTED -- so a
// REJECTED edge is created AUTO_ACCEPTED first, then rejected via
// RejectMediaEdge, matching how the edge lifecycle actually works in
// production (nothing ever inserts a REJECTED row directly).
func seedEdge(t *testing.T, database *db.DB, ancestorID, descendantID int64, reviewState string) {
	t.Helper()
	initial := reviewState
	if initial == "REJECTED" {
		initial = "AUTO_ACCEPTED"
	}
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		edge, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: ancestorID, TargetNodeID: descendantID, RelationshipType: "PROXY_OF",
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: initial,
		})
		if err != nil {
			return err
		}
		if reviewState == "REJECTED" {
			_, err = q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{ID: edge.ID})
		}
		return err
	})
	if err != nil {
		t.Fatalf("seed edge %d->%d: %v", ancestorID, descendantID, err)
	}
}

func hash64(seed string) *string {
	h := seed
	for len(h) < 64 {
		h += "0"
	}
	h = h[:64]
	return &h
}

const oldMtime = 1000
const cutoffUnix = 2000 // anything with mtimeUnix < cutoffUnix is "past TTL"

// TestPlanExcludesUnverifiedNode is #61's primary acceptance criterion: a
// Tier-1 node past its TTL with no live Tier-3 ancestor carrying a verified
// full_hash must never be a candidate. Table-driven over every way
// "unverified" can happen.
func TestPlanExcludesUnverifiedNode(t *testing.T) {
	tests := []struct {
		name          string
		buildAncestor func(t *testing.T, database *db.DB, tier3LocID int64) (ancestorID int64, edgeReview string)
		noAncestor    bool
	}{
		{
			name:       "no ancestor edge at all",
			noAncestor: true,
		},
		{
			name: "ancestor is MISSING",
			buildAncestor: func(t *testing.T, database *db.DB, tier3LocID int64) (int64, string) {
				n := seedNode(t, database, nodeSpec{locationID: tier3LocID, path: "/archive/a.jpg", mtimeUnix: oldMtime, fullHash: hash64("aa"), lifecycle: "MISSING"})
				return n.ID, "AUTO_ACCEPTED"
			},
		},
		{
			name: "ancestor is on Tier 2, not Tier 3",
			buildAncestor: func(t *testing.T, database *db.DB, _ int64) (int64, string) {
				tier2ID := seedLocation(t, database, "t2-"+t.Name(), t.TempDir(), "TIER2_EXPORTS", false, false)
				n := seedNode(t, database, nodeSpec{locationID: tier2ID, path: "/exports/a.jpg", mtimeUnix: oldMtime, fullHash: hash64("bb")})
				return n.ID, "AUTO_ACCEPTED"
			},
		},
		{
			name: "ancestor has no full_hash",
			buildAncestor: func(t *testing.T, database *db.DB, tier3LocID int64) (int64, string) {
				n := seedNode(t, database, nodeSpec{locationID: tier3LocID, path: "/archive/c.jpg", mtimeUnix: oldMtime, fullHash: nil})
				return n.ID, "AUTO_ACCEPTED"
			},
		},
		{
			name: "only linking edge is REJECTED",
			buildAncestor: func(t *testing.T, database *db.DB, tier3LocID int64) (int64, string) {
				n := seedNode(t, database, nodeSpec{locationID: tier3LocID, path: "/archive/d.jpg", mtimeUnix: oldMtime, fullHash: hash64("dd")})
				return n.ID, "REJECTED"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := openTestDB(t)
			tier1ID := seedLocation(t, database, "t1-"+t.Name(), t.TempDir(), "TIER1_LOCAL_SCRATCH", false, true)
			tier3ID := seedLocation(t, database, "t3-"+t.Name(), t.TempDir(), "TIER3_MASTER_ARCHIVE", true, false)

			candidate := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/candidate.jpg", mtimeUnix: oldMtime})

			if !tt.noAncestor {
				ancestorID, review := tt.buildAncestor(t, database, tier3ID)
				seedEdge(t, database, ancestorID, candidate.ID, review)
			}

			got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			for _, c := range got {
				if c.NodeID == candidate.ID {
					t.Fatalf("candidate %d wrongly eligible: %+v", candidate.ID, got)
				}
			}
		})
	}
}

// TestPlanIncludesVerifiedNode is the positive case: a Tier-1 node past its
// TTL with a live Tier-3 ancestor carrying a verified full_hash IS a
// candidate -- proving the exclusion tests above aren't vacuously passing
// because nothing is ever eligible.
func TestPlanIncludesVerifiedNode(t *testing.T) {
	database := openTestDB(t)
	tier1ID := seedLocation(t, database, "t1", t.TempDir(), "TIER1_LOCAL_SCRATCH", false, true)
	tier3ID := seedLocation(t, database, "t3", t.TempDir(), "TIER3_MASTER_ARCHIVE", true, false)

	master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: "/archive/master.jpg", mtimeUnix: oldMtime, fullHash: hash64("verified")})
	candidate := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/proxy.jpg", mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, candidate.ID, "AUTO_ACCEPTED")

	// A node NOT past its TTL must be excluded even with a verified ancestor.
	freshMaster := seedNode(t, database, nodeSpec{locationID: tier3ID, path: "/archive/master2.jpg", mtimeUnix: oldMtime, fullHash: hash64("verified2")})
	freshCandidate := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/fresh.jpg", mtimeUnix: cutoffUnix + 1000})
	seedEdge(t, database, freshMaster.ID, freshCandidate.ID, "AUTO_ACCEPTED")

	got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != candidate.ID {
		t.Fatalf("Plan = %+v, want exactly the verified past-TTL candidate %d", got, candidate.ID)
	}
	if got[0].FilePath != candidate.FilePath {
		t.Errorf("FilePath = %q, want %q", got[0].FilePath, candidate.FilePath)
	}
}

// TestPlanExcludesChainThroughArchivedIntermediate proves a multi-hop
// lineage chain that only connects to a verified Tier-3 master THROUGH an
// ARCHIVED intermediate node is not eligible -- mirroring ListAncestors'
// own exclusion of ARCHIVED nodes from the walk (media_edges.sql). An
// ARCHIVED node is a superseded version; a chain that only reaches the
// master through one no longer represents the file currently on disk. Two
// subtests share one chain shape (candidate <- intermediate <- master) so
// the exclusion isn't proven vacuously: with the intermediate ACTIVE the
// same chain IS eligible.
func TestPlanExcludesChainThroughArchivedIntermediate(t *testing.T) {
	buildChain := func(t *testing.T, intermediateLifecycle string) (database *db.DB, tier1ID int64, candidate sqlcgen.MediaNode) {
		database = openTestDB(t)
		tier1ID = seedLocation(t, database, "t1", t.TempDir(), "TIER1_LOCAL_SCRATCH", false, true)
		tier2ID := seedLocation(t, database, "t2", t.TempDir(), "TIER2_EXPORTS", false, false)
		tier3ID := seedLocation(t, database, "t3", t.TempDir(), "TIER3_MASTER_ARCHIVE", true, false)

		master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: "/archive/master.jpg", mtimeUnix: oldMtime, fullHash: hash64("chain")})
		intermediate := seedNode(t, database, nodeSpec{locationID: tier2ID, path: "/exports/intermediate.jpg", mtimeUnix: oldMtime, lifecycle: intermediateLifecycle})
		candidate = seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/candidate.jpg", mtimeUnix: oldMtime})

		seedEdge(t, database, master.ID, intermediate.ID, "AUTO_ACCEPTED")
		seedEdge(t, database, intermediate.ID, candidate.ID, "AUTO_ACCEPTED")
		return database, tier1ID, candidate
	}

	t.Run("ACTIVE intermediate: chain is eligible", func(t *testing.T) {
		database, tier1ID, candidate := buildChain(t, "ACTIVE")
		got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(got) != 1 || got[0].NodeID != candidate.ID {
			t.Fatalf("Plan = %+v, want exactly candidate %d eligible through an ACTIVE intermediate", got, candidate.ID)
		}
	})

	t.Run("ARCHIVED intermediate: chain is not eligible", func(t *testing.T) {
		database, tier1ID, candidate := buildChain(t, "ARCHIVED")
		got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, c := range got {
			if c.NodeID == candidate.ID {
				t.Fatalf("candidate %d wrongly eligible through an ARCHIVED intermediate: %+v", candidate.ID, got)
			}
		}
	})
}

// TestExecuteSymlinkEscapeRefused transposes
// storage.TestSymlinkEscapeRefused onto the pruning executor: a Tier-1
// path that is actually a symlink into Tier 3 must be refused by
// Guard.CheckWrite before any syscall, proving Execute genuinely routes
// through Guard rather than calling os.Remove directly. The point is
// proving the *executor*, not re-proving Guard (already proven).
func TestExecuteSymlinkEscapeRefused(t *testing.T) {
	database := openTestDB(t)
	tier1Root := t.TempDir()
	tier3Root := t.TempDir()

	tier3File := filepath.Join(tier3Root, "master.jpg")
	if err := os.WriteFile(tier3File, []byte("master content"), 0o644); err != nil {
		t.Fatalf("write tier3 file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", tier1Root, "TIER1_LOCAL_SCRATCH", false, true)
	tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)

	// A Tier-1 path that is actually a symlink into Tier 3.
	linkPath := filepath.Join(tier1Root, "escape-hatch.jpg")
	if err := os.Symlink(tier3File, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: tier1ID, Name: "t1", RootPath: tier1Root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false},
		{ID: tier3ID, Name: "t3", RootPath: tier3Root, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})

	candidate := Candidate{NodeID: 1, FilePath: linkPath, FileName: "escape-hatch.jpg", StorageLocationID: tier1ID}
	results := Execute(context.Background(), database, guard, []Candidate{candidate})

	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly 1", results)
	}
	if results[0].Purged {
		t.Fatal("Execute purged a symlink escape into Tier 3, want refused")
	}
	var roErr *storage.ErrReadOnlyTier
	if !errors.As(results[0].Err, &roErr) {
		t.Fatalf("err = %v (%T), want *storage.ErrReadOnlyTier", results[0].Err, results[0].Err)
	}
	if _, statErr := os.Stat(tier3File); statErr != nil {
		t.Errorf("tier3 master file gone (stat err = %v), want it to survive", statErr)
	}
}

// TestExecutePurgesFileAndMarksMissing proves the happy path: the file is
// deleted from disk, the node lands in MISSING (never deleted -- rows are
// never deleted, matching this repo's invariant), and superseded_by chains
// (irrelevant here, but the row count itself) stay intact.
func TestExecutePurgesFileAndMarksMissing(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "cache.jpg")
	if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})

	beforeCount := countMediaNodes(t, database)

	results := Execute(context.Background(), database, guard, []Candidate{
		{NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID},
	})

	if len(results) != 1 || !results[0].Purged || results[0].Err != nil {
		t.Fatalf("results = %+v, want a single successful purge", results)
	}
	if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after purge (stat err = %v)", err)
	}

	after, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "MISSING" {
		t.Errorf("lifecycle_state = %q, want MISSING", after.LifecycleState)
	}

	afterCount := countMediaNodes(t, database)
	if afterCount != beforeCount {
		t.Errorf("media_nodes row count changed from %d to %d, want unchanged (rows are never deleted)", beforeCount, afterCount)
	}
}

// countMediaNodes counts every non-ARCHIVED row -- ListMediaNodes excludes
// only ARCHIVED (docs/schema.md), so a node purged to MISSING still counts,
// which is exactly what proves Execute marks rows MISSING rather than
// deleting them.
func countMediaNodes(t *testing.T, database *db.DB) int {
	t.Helper()
	rows, err := database.Reader.ListMediaNodes(context.Background(), sqlcgen.ListMediaNodesParams{Limit: 1000, Offset: 0})
	if err != nil {
		t.Fatalf("ListMediaNodes: %v", err)
	}
	return len(rows)
}
