package prune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	sizeBytes  int64
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
	sizeBytes := spec.sizeBytes
	if sizeBytes == 0 {
		sizeBytes = 100
	}
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-" + spec.path, StorageLocationID: spec.locationID,
			FilePath: spec.path, FileName: filepath.Base(spec.path), FileExt: "jpg",
			SizeBytes: sizeBytes, MtimeUnix: spec.mtimeUnix, FullHash: spec.fullHash,
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
//
// The candidate node is seeded with a real verified Tier-3 ancestor so
// Execute's own re-eligibility check (which now runs before Guard.Remove)
// doesn't short-circuit with ErrNoLongerEligible before ever reaching the
// symlink -- this test is specifically about the Guard call, not the
// eligibility re-check (that's TestExecuteAbortsWhenNoLongerEligible).
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
	// Execute's file-freshness re-check compares against the symlink's OWN
	// Lstat (never followed), so the candidate must carry that, not the
	// target's -- otherwise the freshness check itself would refuse before
	// ever reaching Guard, defeating this test's actual point.
	linkInfo, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: tier1ID, Name: "t1", RootPath: tier1Root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false},
		{ID: tier3ID, Name: "t3", RootPath: tier3Root, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})

	tier3Stat, err := os.Lstat(tier3File)
	if err != nil {
		t.Fatalf("lstat tier3 file: %v", err)
	}

	master := seedNode(t, database, nodeSpec{
		locationID: tier3ID,
		path:       tier3File,
		sizeBytes:  tier3Stat.Size(),
		mtimeUnix:  tier3Stat.ModTime().Unix(),
		fullHash:   hash64("escape"),
	})
	candidateNode := seedNode(t, database, nodeSpec{locationID: tier1ID, path: linkPath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, candidateNode.ID, "AUTO_ACCEPTED")

	candidate := Candidate{
		NodeID: candidateNode.ID, FilePath: linkPath, FileName: "escape-hatch.jpg", StorageLocationID: tier1ID,
		MtimeUnix: linkInfo.ModTime().Unix(), SizeBytes: linkInfo.Size(),
	}
	results := Execute(context.Background(), database, guard, []Candidate{candidate}, cutoffUnix)

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

// TestExecuteAbortsWhenNoLongerEligible proves the TOCTOU fix: a candidate
// whose verified Tier-3 ancestor is gone by the time Execute runs (e.g. the
// master went MISSING between the dry-run and the confirm click) is
// refused with ErrNoLongerEligible, and its file is never touched --
// exactly the scenario the verified-hash gate exists to prevent.
func TestExecuteAbortsWhenNoLongerEligible(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "proxy.jpg")
	if err := os.WriteFile(filePath, []byte("proxy content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

	// A candidate with NO verified ancestor at all -- as if Plan's own
	// snapshot became stale (master went MISSING, full_hash cleared, etc.)
	// by the time Execute runs.
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})

	results := Execute(context.Background(), database, guard, []Candidate{
		{NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID},
	}, cutoffUnix)

	if len(results) != 1 || results[0].Purged {
		t.Fatalf("results = %+v, want a single refused (not purged) result", results)
	}
	if !errors.Is(results[0].Err, ErrNoLongerEligible) {
		t.Errorf("err = %v, want ErrNoLongerEligible", results[0].Err)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("file gone after a refused purge (stat err = %v), want it to survive", err)
	}
	after, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("lifecycle_state = %q, want ACTIVE (unchanged)", after.LifecycleState)
	}
}

// TestExecuteAbortsWhenTier3AncestorUnreachable proves issue #246: a candidate
// whose Tier-3 ancestor's DB row says ACTIVE and carries a verified full_hash,
// but whose file on disk is unreachable (e.g. empty-mount / stale NFS handle /
// deleted master), is refused with ErrAncestorUnreachable and its Tier-1 file is
// never deleted -- preventing data loss when the DB is out of sync with disk.
func TestExecuteAbortsWhenTier3AncestorUnreachable(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "cache.jpg")
	if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fileInfo, err := os.Lstat(filePath)
	if err != nil {
		t.Fatalf("lstat file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	tier3Root := t.TempDir()
	tier3Path := filepath.Join(tier3Root, "unreachable_master.jpg") // NOT created on disk
	tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

	master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: tier3Path, mtimeUnix: oldMtime, fullHash: hash64("unreachable")})
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, node.ID, "AUTO_ACCEPTED")

	results := Execute(context.Background(), database, guard, []Candidate{
		{
			NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID,
			MtimeUnix: fileInfo.ModTime().Unix(), SizeBytes: fileInfo.Size(),
		},
	}, cutoffUnix)

	if len(results) != 1 || results[0].Purged {
		t.Fatalf("results = %+v, want candidate refused (not purged)", results)
	}
	if !errors.Is(results[0].Err, ErrAncestorUnreachable) {
		t.Errorf("err = %v, want ErrAncestorUnreachable", results[0].Err)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("Tier-1 cache file missing after refused purge: %v", err)
	}
	after, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("lifecycle_state = %q, want ACTIVE (must not be marked MISSING when purge refused)", after.LifecycleState)
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
	fileInfo, err := os.Lstat(filePath)
	if err != nil {
		t.Fatalf("lstat file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	tier3Root := t.TempDir()
	tier3Path := filepath.Join(tier3Root, "master.jpg")
	if err := os.WriteFile(tier3Path, []byte("master content"), 0o644); err != nil {
		t.Fatalf("write master file: %v", err)
	}
	tier3Stat, err := os.Lstat(tier3Path)
	if err != nil {
		t.Fatalf("lstat tier3 file: %v", err)
	}
	tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

	master := seedNode(t, database, nodeSpec{
		locationID: tier3ID,
		path:       tier3Path,
		sizeBytes:  tier3Stat.Size(),
		mtimeUnix:  tier3Stat.ModTime().Unix(),
		fullHash:   hash64("purge"),
	})
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, node.ID, "AUTO_ACCEPTED")

	beforeCount := countMediaNodes(t, database)

	// Execute's file-freshness re-check compares the candidate's
	// MtimeUnix/SizeBytes against a fresh Lstat, so a candidate built by
	// hand (rather than returned from Plan) must carry the file's real
	// values, not the DB row's -- see TestExecuteRefusesWhenFileChangedSincePlan
	// for what happens when they deliberately don't match.
	results := Execute(context.Background(), database, guard, []Candidate{
		{
			NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID,
			MtimeUnix: fileInfo.ModTime().Unix(), SizeBytes: fileInfo.Size(),
		},
	}, cutoffUnix)

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

// TestExecuteRefusesWhenFileChangedSincePlan proves the file-freshness
// re-check: a candidate whose on-disk (mtime, size) no longer matches what
// Plan recorded -- e.g. the cache file was regenerated with fresh content
// moments before Execute runs, and no scan has observed that yet -- is
// refused with ErrFileChangedSincePlan and never deleted. media_nodes.mtime_unix
// is only as fresh as the last scan/sweep, so the DB-side eligibility
// re-check alone (TestExecuteAbortsWhenNoLongerEligible) can't catch this;
// only a fresh Lstat immediately before Guard.Remove can.
func TestExecuteRefusesWhenFileChangedSincePlan(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "cache.jpg")
	if err := os.WriteFile(filePath, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	tier3Root := t.TempDir()
	tier3Path := filepath.Join(tier3Root, "master.jpg")
	if err := os.WriteFile(tier3Path, []byte("master content"), 0o644); err != nil {
		t.Fatalf("write master file: %v", err)
	}
	tier3Stat, err := os.Lstat(tier3Path)
	if err != nil {
		t.Fatalf("lstat tier3 file: %v", err)
	}
	tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

	master := seedNode(t, database, nodeSpec{
		locationID: tier3ID,
		path:       tier3Path,
		sizeBytes:  tier3Stat.Size(),
		mtimeUnix:  tier3Stat.ModTime().Unix(),
		fullHash:   hash64("stale"),
	})
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, node.ID, "AUTO_ACCEPTED")

	// The file on disk is regenerated with different content -- a longer
	// byte length is enough to guarantee a size mismatch regardless of
	// filesystem mtime granularity.
	if err := os.WriteFile(filePath, []byte("regenerated content, much longer now"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}

	// The candidate carries the STALE (pre-regeneration) mtime/size, as if
	// it came from a Plan call whose snapshot predates the regeneration --
	// exactly the race window Execute's freshness check exists to close.
	results := Execute(context.Background(), database, guard, []Candidate{
		{NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID, MtimeUnix: oldMtime, SizeBytes: 13},
	}, cutoffUnix)

	if len(results) != 1 || results[0].Purged {
		t.Fatalf("results = %+v, want a single refused (not purged) result", results)
	}
	if !errors.Is(results[0].Err, ErrFileChangedSincePlan) {
		t.Errorf("err = %v, want ErrFileChangedSincePlan", results[0].Err)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "regenerated content, much longer now" {
		t.Errorf("file content = %q, want the regenerated content to survive untouched", got)
	}

	after, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("lifecycle_state = %q, want ACTIVE (unchanged -- a refused purge must not mark the node MISSING)", after.LifecycleState)
	}
}

// TestExecuteMarksMissingWhenFileAlreadyGone proves the other side of the
// freshness check: a candidate whose file vanished on its own (not via
// Guard.Remove -- e.g. deleted out-of-band) is not treated as an error.
// Nothing is left for Guard to remove, but the node's record is stale
// either way, so it still lands in MISSING -- self-healing, matching how
// the ordinary MISSING sweep treats a vanished file elsewhere in this
// codebase.
func TestExecuteMarksMissingWhenFileAlreadyGone(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "cache.jpg")
	if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	tier3Root := t.TempDir()
	tier3Path := filepath.Join(tier3Root, "master.jpg")
	if err := os.WriteFile(tier3Path, []byte("master content"), 0o644); err != nil {
		t.Fatalf("write master file: %v", err)
	}
	tier3Stat, err := os.Lstat(tier3Path)
	if err != nil {
		t.Fatalf("lstat tier3 file: %v", err)
	}
	tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

	master := seedNode(t, database, nodeSpec{
		locationID: tier3ID,
		path:       tier3Path,
		sizeBytes:  tier3Stat.Size(),
		mtimeUnix:  tier3Stat.ModTime().Unix(),
		fullHash:   hash64("gone"),
	})
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, node.ID, "AUTO_ACCEPTED")

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	results := Execute(context.Background(), database, guard, []Candidate{
		{NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID, MtimeUnix: oldMtime, SizeBytes: 13},
	}, cutoffUnix)

	if len(results) != 1 || !results[0].Purged || results[0].Err != nil {
		t.Fatalf("results = %+v, want a single successful purge (already-gone is not an error)", results)
	}
	after, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "MISSING" {
		t.Errorf("lifecycle_state = %q, want MISSING", after.LifecycleState)
	}
}

// TestExecuteAbortsWhenTier3AncestorRewritten proves issue #352:
// when a Tier-3 ancestor's file on disk has been modified (mtime or size changed)
// since its node record was verified in the DB, Execute refuses to purge the
// Tier-1 candidate (returning ErrAncestorUnreachable) and leaves the candidate
// file and DB record untouched.
func TestExecuteAbortsWhenTier3AncestorRewritten(t *testing.T) {
	t.Run("size mismatch", func(t *testing.T) {
		database := openTestDB(t)
		root := t.TempDir()
		filePath := filepath.Join(root, "cache.jpg")
		if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		fileInfo, err := os.Lstat(filePath)
		if err != nil {
			t.Fatalf("lstat file: %v", err)
		}

		tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
		tier3Root := t.TempDir()
		tier3Path := filepath.Join(tier3Root, "master.jpg")
		if err := os.WriteFile(tier3Path, []byte("master content"), 0o644); err != nil {
			t.Fatalf("write master file: %v", err)
		}
		tier3Stat, err := os.Lstat(tier3Path)
		if err != nil {
			t.Fatalf("lstat tier3 file: %v", err)
		}
		tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
		guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

		// Seed master with initial on-disk mtime and size.
		master := seedNode(t, database, nodeSpec{
			locationID: tier3ID,
			path:       tier3Path,
			sizeBytes:  tier3Stat.Size(),
			mtimeUnix:  tier3Stat.ModTime().Unix(),
			fullHash:   hash64("rewritten-size"),
		})
		candidateNode := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
		seedEdge(t, database, master.ID, candidateNode.ID, "AUTO_ACCEPTED")

		// Rewrite master file with different byte length.
		if err := os.WriteFile(tier3Path, []byte("modified master content with longer byte length"), 0o644); err != nil {
			t.Fatalf("rewrite master file: %v", err)
		}

		results := Execute(context.Background(), database, guard, []Candidate{
			{
				NodeID: candidateNode.ID, FilePath: candidateNode.FilePath, FileName: candidateNode.FileName,
				StorageLocationID: tier1ID, MtimeUnix: fileInfo.ModTime().Unix(), SizeBytes: fileInfo.Size(),
			},
		}, cutoffUnix)

		if len(results) != 1 || results[0].Purged {
			t.Fatalf("results = %+v, want candidate refused (not purged)", results)
		}
		if !errors.Is(results[0].Err, ErrAncestorUnreachable) {
			t.Errorf("err = %v, want ErrAncestorUnreachable", results[0].Err)
		}
		if _, err := os.Stat(filePath); err != nil {
			t.Errorf("Tier-1 cache file missing after refused purge: %v", err)
		}
		after, err := database.Reader.GetMediaNodeByID(context.Background(), candidateNode.ID)
		if err != nil {
			t.Fatalf("GetMediaNodeByID: %v", err)
		}
		if after.LifecycleState != "ACTIVE" {
			t.Errorf("lifecycle_state = %q, want ACTIVE", after.LifecycleState)
		}
	})

	t.Run("mtime mismatch", func(t *testing.T) {
		database := openTestDB(t)
		root := t.TempDir()
		filePath := filepath.Join(root, "cache.jpg")
		if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		fileInfo, err := os.Lstat(filePath)
		if err != nil {
			t.Fatalf("lstat file: %v", err)
		}

		tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
		tier3Root := t.TempDir()
		tier3Path := filepath.Join(tier3Root, "master.jpg")
		if err := os.WriteFile(tier3Path, []byte("master content"), 0o644); err != nil {
			t.Fatalf("write master file: %v", err)
		}
		tier3Stat, err := os.Lstat(tier3Path)
		if err != nil {
			t.Fatalf("lstat tier3 file: %v", err)
		}
		tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
		guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})

		// Seed master with initial on-disk mtime and size.
		master := seedNode(t, database, nodeSpec{
			locationID: tier3ID,
			path:       tier3Path,
			sizeBytes:  tier3Stat.Size(),
			mtimeUnix:  tier3Stat.ModTime().Unix(),
			fullHash:   hash64("rewritten-mtime"),
		})
		candidateNode := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
		seedEdge(t, database, master.ID, candidateNode.ID, "AUTO_ACCEPTED")

		// Modify master file mtime without changing content/size.
		newTime := tier3Stat.ModTime().Add(10 * time.Second)
		if err := os.Chtimes(tier3Path, newTime, newTime); err != nil {
			t.Fatalf("chtimes master file: %v", err)
		}

		results := Execute(context.Background(), database, guard, []Candidate{
			{
				NodeID: candidateNode.ID, FilePath: candidateNode.FilePath, FileName: candidateNode.FileName,
				StorageLocationID: tier1ID, MtimeUnix: fileInfo.ModTime().Unix(), SizeBytes: fileInfo.Size(),
			},
		}, cutoffUnix)

		if len(results) != 1 || results[0].Purged {
			t.Fatalf("results = %+v, want candidate refused (not purged)", results)
		}
		if !errors.Is(results[0].Err, ErrAncestorUnreachable) {
			t.Errorf("err = %v, want ErrAncestorUnreachable", results[0].Err)
		}
		if _, err := os.Stat(filePath); err != nil {
			t.Errorf("Tier-1 cache file missing after refused purge: %v", err)
		}
		after, err := database.Reader.GetMediaNodeByID(context.Background(), candidateNode.ID)
		if err != nil {
			t.Fatalf("GetMediaNodeByID: %v", err)
		}
		if after.LifecycleState != "ACTIVE" {
			t.Errorf("lifecycle_state = %q, want ACTIVE", after.LifecycleState)
		}
	})
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

// TestExecuteSurfacesPlanFailureForLocation proves the pre-compute error
// path: when Plan fails for a storage location (transient DB error, schema
// drift, etc.), every candidate from that location must surface the real
// error in Result.Err -- not be silently mislabeled ErrNoLongerEligible.
// For a disk-reclaiming operation, the difference matters: ErrNoLongerEligible
// tells the caller the row is no longer eligible and the prune is
// appropriate-on-paper-but-deferred; a propagated Plan error tells the
// caller the prune is being skipped because of a downstream problem they
// need to investigate. Swallowing it as ErrNoLongerEligible masks systematic
// failures and was the behavior Hermes flagged on the pre-compute refactor.
//
// The test forces Plan to fail by closing the DB before Execute is called --
// after Close, every Query on either the reader or writer pool returns
// "sql: database is closed". Without the fix, the candidate falls through
// to InTx which itself errors with the writer's "begin transaction:
// database is closed" -- a different (writer-side) error than the Plan
// failure, and one that doesn't identify the pre-compute as the source.
// With the fix, the candidate's Result.Err is the wrapped Plan error from
// the pre-compute, identifiable by its "plan eligibility for location"
// message and the underlying DB-closed cause.
func TestExecuteSurfacesPlanFailureForLocation(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "cache.jpg")
	if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false}})
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	results := Execute(context.Background(), database, guard, []Candidate{
		{NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID, MtimeUnix: oldMtime, SizeBytes: 13},
	}, cutoffUnix)

	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly 1", results)
	}
	if results[0].Purged {
		t.Fatal("candidate purged despite a Plan failure, want refused")
	}
	if !strings.Contains(results[0].Err.Error(), "plan eligibility for location") {
		t.Fatalf("err = %q, want it to wrap the Plan error with a location-scoped message so callers can see where the failure originated", results[0].Err)
	}
	if !strings.Contains(results[0].Err.Error(), "closed") {
		t.Errorf("err = %q, want it to surface the underlying DB error (got a closed DB, so \"closed\" should appear in the message)", results[0].Err)
	}
}
