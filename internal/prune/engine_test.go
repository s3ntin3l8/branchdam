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
	// hashSource is the full_hash_provenance source recorded for fullHash:
	// db.HashSourceServer (the default, as a scan records it) or
	// db.HashSourceAgent (asserted by a paired device, #505).
	hashSource string
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
		hashSource := spec.hashSource
		if hashSource == "" {
			hashSource = db.HashSourceServer
		}
		if err := db.RecordFullHashSource(context.Background(), q, n.ID, spec.fullHash, hashSource); err != nil {
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
	seedEdgeRel(t, database, ancestorID, descendantID, "PROXY_OF", reviewState)
}

// seedEdgeRel is seedEdge with an explicit relationship_type.
func seedEdgeRel(t *testing.T, database *db.DB, ancestorID, descendantID int64, rel, reviewState string) {
	t.Helper()
	initial := reviewState
	if initial == "REJECTED" || initial == "CONFIRMED" {
		initial = "AUTO_ACCEPTED"
	}
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		edge, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: ancestorID, TargetNodeID: descendantID, RelationshipType: rel,
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: initial,
		})
		if err != nil {
			return err
		}
		switch reviewState {
		case "REJECTED":
			_, err = q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{ID: edge.ID})
		case "CONFIRMED":
			_, err = q.ConfirmMediaEdge(context.Background(), sqlcgen.ConfirmMediaEdgeParams{ID: edge.ID})
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
			// #505: a hash a paired device only asserted is not verified,
			// however well-formed it is.
			name: "ancestor full_hash is agent-asserted",
			buildAncestor: func(t *testing.T, database *db.DB, tier3LocID int64) (int64, string) {
				n := seedNode(t, database, nodeSpec{locationID: tier3LocID, path: "/archive/e.jpg", mtimeUnix: oldMtime, fullHash: hash64("ee"), hashSource: db.HashSourceAgent})
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

	// A MISSING intermediate is a file the last scan could not find; the
	// chain no longer describes anything on disk, so it must not authorise
	// a purge any more than an ARCHIVED one does. Both the Plan query and
	// Execute's re-check query must agree.
	t.Run("MISSING intermediate: chain is not eligible", func(t *testing.T) {
		database, tier1ID, candidate := buildChain(t, "MISSING")
		got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, c := range got {
			if c.NodeID == candidate.ID {
				t.Fatalf("candidate %d wrongly eligible through a MISSING intermediate: %+v", candidate.ID, got)
			}
		}
		ancestors, err := database.Reader.ListVerifiedTier3Ancestors(context.Background(), candidate.ID)
		if err != nil {
			t.Fatalf("ListVerifiedTier3Ancestors: %v", err)
		}
		if len(ancestors) != 0 {
			t.Fatalf("ListVerifiedTier3Ancestors walked through a MISSING intermediate: %+v", ancestors)
		}
	})
}

// TestPlanOnlyFollowsCacheRelationships proves only the cache relationships
// (a copy or rendition of the master's bytes) make a Tier-1 node prunable.
// PROJECT_SIDECAR makes a project file or .xmp the CHILD of the media it
// references, at confidence 1.00 (graph.ProjectSidecarResolver), and a
// FINAL_EXPORT child is finished work: neither is recoverable from the
// Tier-3 master, so neither edge may authorise deleting the child. Execute's
// re-check (ListVerifiedTier3Ancestors) must agree with Plan.
func TestPlanOnlyFollowsCacheRelationships(t *testing.T) {
	tests := []struct {
		rel      string
		eligible bool
	}{
		{"PROXY_OF", true},
		{"DERIVED_FROM", true},
		{"DUPLICATE_OF", true},
		{"PROJECT_SIDECAR", false},
		{"FINAL_EXPORT", false},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			database := openTestDB(t)
			tier1ID := seedLocation(t, database, "t1", t.TempDir(), "TIER1_LOCAL_SCRATCH", false, true)
			tier3ID := seedLocation(t, database, "t3", t.TempDir(), "TIER3_MASTER_ARCHIVE", true, false)

			master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: "/archive/master.jpg", mtimeUnix: oldMtime, fullHash: hash64("rel")})
			candidate := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/child.jpg", mtimeUnix: oldMtime})
			seedEdgeRel(t, database, master.ID, candidate.ID, tt.rel, "CONFIRMED")

			got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			planned := len(got) == 1 && got[0].NodeID == candidate.ID
			if planned != tt.eligible {
				t.Fatalf("Plan = %+v, want eligible=%v for a %s child", got, tt.eligible, tt.rel)
			}
			ancestors, err := database.Reader.ListVerifiedTier3Ancestors(context.Background(), candidate.ID)
			if err != nil {
				t.Fatalf("ListVerifiedTier3Ancestors: %v", err)
			}
			if (len(ancestors) == 1) != tt.eligible {
				t.Fatalf("ListVerifiedTier3Ancestors = %+v, want verified ancestor=%v over %s", ancestors, tt.eligible, tt.rel)
			}
		})
	}
}

// TestPlanNeverSelectsProjectFilesOrSidecars proves project files and .xmp
// sidecars on a prunable Tier-1 location are never candidates, even when a
// verified Tier-3 master links to them. The PROJECT_SIDECAR case is the
// shape the resolvers actually produce; the PROXY_OF/DERIVED_FROM cases
// cover a mislabelled (manual or agent-attached) edge. A plain proxy in the
// same setup stays eligible, so the exclusion is not vacuous.
func TestPlanNeverSelectsProjectFilesOrSidecars(t *testing.T) {
	tests := []struct {
		file string
		rel  string
	}{
		{"timeline.drp", "PROJECT_SIDECAR"},
		{"IMG_0001.xmp", "PROJECT_SIDECAR"},
		{"cut.fcpxml", "PROJECT_SIDECAR"},
		{"timeline.drp", "PROXY_OF"},
		{"IMG_0001.CR3.XMP", "DERIVED_FROM"},
		{"reel.EDL", "DERIVED_FROM"},
		{"edit.prproj", "DUPLICATE_OF"},
		{"shot.dam.json", "DERIVED_FROM"},
	}
	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.rel, func(t *testing.T) {
			database := openTestDB(t)
			tier1ID := seedLocation(t, database, "t1", t.TempDir(), "TIER1_LOCAL_SCRATCH", false, true)
			tier3ID := seedLocation(t, database, "t3", t.TempDir(), "TIER3_MASTER_ARCHIVE", true, false)

			master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: "/archive/IMG_0001.CR3", mtimeUnix: oldMtime, fullHash: hash64("proj")})
			project := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/" + tt.file, mtimeUnix: oldMtime})
			proxy := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/IMG_0001_proxy.mp4", mtimeUnix: oldMtime})
			seedEdgeRel(t, database, master.ID, project.ID, tt.rel, "CONFIRMED")
			seedEdgeRel(t, database, master.ID, proxy.ID, "PROXY_OF", "CONFIRMED")

			got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(got) != 1 || got[0].NodeID != proxy.ID {
				t.Fatalf("Plan = %+v, want only the proxy %d (never %s %d)", got, proxy.ID, tt.file, project.ID)
			}
		})
	}
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

// TestSameAncestorsDetectsAChangedAncestor covers the two ways the
// verified-ancestor set can change under a purge.
//
// Membership: the Tier-3 master was archived and replaced, so the id is no
// longer in the set. Values: it is the SAME node, but a concurrent scan's
// TouchMediaNode refreshed its mtime_unix (or the size, or the location)
// while leaving the id and the SERVER provenance in place -- so the
// on-disk verification the purge did a moment earlier compared against
// values the row no longer carries, and the purge must give up.
//
// The value branch is reachable, not theoretical: an unchanged master that
// a scan merely re-touches is exactly that case.
func TestSameAncestorsDetectsAChangedAncestor(t *testing.T) {
	base := sqlcgen.ListVerifiedTier3AncestorsRow{
		ID:                7,
		FilePath:          "/archive/master.mov",
		StorageLocationID: 3,
		MtimeUnix:         1000,
		SizeBytes:         4096,
	}
	changed := func(mutate func(r *sqlcgen.ListVerifiedTier3AncestorsRow)) []sqlcgen.ListVerifiedTier3AncestorsRow {
		row := base
		mutate(&row)
		return []sqlcgen.ListVerifiedTier3AncestorsRow{row}
	}
	cases := []struct {
		name  string
		fresh []sqlcgen.ListVerifiedTier3AncestorsRow
		want  bool
	}{
		{"unchanged", []sqlcgen.ListVerifiedTier3AncestorsRow{base}, true},
		{"archived and replaced", changed(func(r *sqlcgen.ListVerifiedTier3AncestorsRow) { r.ID = 8 }), false},
		{"same node, refreshed mtime", changed(func(r *sqlcgen.ListVerifiedTier3AncestorsRow) { r.MtimeUnix = 1001 }), false},
		{"same node, changed size", changed(func(r *sqlcgen.ListVerifiedTier3AncestorsRow) { r.SizeBytes = 4097 }), false},
		{"same node, moved path", changed(func(r *sqlcgen.ListVerifiedTier3AncestorsRow) { r.FilePath = "/archive/other.mov" }), false},
		{"same node, another location", changed(func(r *sqlcgen.ListVerifiedTier3AncestorsRow) { r.StorageLocationID = 4 }), false},
		{"no ancestors left", nil, false},
		{"an extra ancestor appeared", []sqlcgen.ListVerifiedTier3AncestorsRow{base, {ID: 8, FilePath: "/archive/other.mov", StorageLocationID: 3, MtimeUnix: 1000, SizeBytes: 4096}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameAncestors([]sqlcgen.ListVerifiedTier3AncestorsRow{base}, tc.fresh); got != tc.want {
				t.Errorf("sameAncestors = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExecuteAbortsWhenTheAncestorIsArchivedDuringTheFilesystemWork proves
// the second half of the prune gate.
//
// Splitting the prune into phases -- so the writer connection is no longer
// held across the filesystem work -- reopened a window the single
// transaction had closed: a concurrent scan can archive the Tier-3 master
// and insert its successor (the version-collision path internal/pipeline
// .Commit takes when the master's bytes changed under it) while the purge
// is verifying the master's file on disk.
//
// That archive is precisely what makes the gate fail closed: the archived
// row leaves ListVerifiedTier3Ancestors, so the candidate's edge chain no
// longer reaches a live, verified Tier-3 master. Without the authoritative
// re-read, the purge would unlink the Tier-1 cache -- possibly the last
// copy of the bytes the successor no longer holds -- after the master it
// was verified against had already been archived.
//
// The archive is injected through the afterVerify seam: the exact moment
// the phase split opened.
func TestExecuteAbortsWhenTheAncestorIsArchivedDuringTheFilesystemWork(t *testing.T) {
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
		fullHash:   hash64("master"),
	})
	candidateNode := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, candidateNode.ID, "AUTO_ACCEPTED")

	if got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix); err != nil || len(got) != 1 {
		t.Fatalf("Plan = %+v, %v; want one candidate", got, err)
	}

	// The concurrent write the phase split has to survive: the master is
	// archived while the purge is verifying the disk.
	var archiveErr error
	archived := false
	results := execute(context.Background(), database, guard, []Candidate{{
		NodeID: candidateNode.ID, FilePath: candidateNode.FilePath, FileName: candidateNode.FileName,
		StorageLocationID: tier1ID, MtimeUnix: fileInfo.ModTime().Unix(), SizeBytes: fileInfo.Size(),
	}}, cutoffUnix, func() {
		archiveErr = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			if err := q.ArchiveMediaNode(context.Background(), master.ID); err != nil {
				return err
			}
			archived = true
			return nil
		})
	})
	if archiveErr != nil {
		t.Fatalf("archive master: %v", archiveErr)
	}
	if !archived {
		t.Fatal("the afterVerify seam never ran -- the test did not exercise the window")
	}

	if len(results) != 1 || results[0].Purged {
		t.Fatalf("results = %+v, want a single refused (not purged) result", results)
	}
	if !errors.Is(results[0].Err, ErrNoLongerEligible) {
		t.Errorf("err = %v, want ErrNoLongerEligible", results[0].Err)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("Tier-1 cache file missing after refused purge (stat err = %v), want it to survive", err)
	}
	after, err := database.Reader.GetMediaNodeByID(context.Background(), candidateNode.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("lifecycle_state = %q, want ACTIVE (unchanged)", after.LifecycleState)
	}
}

// TestExecuteRefusesAgentAssertedAncestor covers #505 at Execute time: a
// live Tier-3 ancestor whose full_hash was only asserted by a paired device
// -- or whose server-computed provenance no longer matches the node's
// current hash -- does not authorise the purge, even though the hash is a
// well-formed 64-character value on an ACTIVE master.
func TestExecuteRefusesAgentAssertedAncestor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		// rewrite replaces the node's full_hash after its provenance was
		// recorded, without recording a new one.
		rewrite bool
	}{
		{name: "agent-asserted", source: db.HashSourceAgent},
		{name: "server row vouches for an older hash", source: db.HashSourceServer, rewrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			archive := t.TempDir()
			filePath := filepath.Join(root, "proxy.jpg")
			if err := os.WriteFile(filePath, []byte("proxy content"), 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
			tier3ID := seedLocation(t, database, "t3", archive, "TIER3_MASTER_ARCHIVE", true, false)
			guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH"}})

			master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: filepath.Join(archive, "master.jpg"), mtimeUnix: oldMtime, fullHash: hash64("asserted"), hashSource: tc.source})
			if tc.rewrite {
				if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
					return q.UpdateMediaNodeFullHash(context.Background(), sqlcgen.UpdateMediaNodeFullHashParams{ID: master.ID, FullHash: hash64("rewritten")})
				}); err != nil {
					t.Fatalf("rewrite full_hash: %v", err)
				}
			}
			node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
			seedEdge(t, database, master.ID, node.ID, "AUTO_ACCEPTED")

			if got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix); err != nil || len(got) != 0 {
				t.Fatalf("Plan = %+v, %v; want no candidates", got, err)
			}
			results := Execute(context.Background(), database, guard, []Candidate{
				{NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID},
			}, cutoffUnix)
			if len(results) != 1 || results[0].Purged || !errors.Is(results[0].Err, ErrNoLongerEligible) {
				t.Fatalf("results = %+v, want a single ErrNoLongerEligible refusal", results)
			}
			if _, err := os.Stat(filePath); err != nil {
				t.Errorf("file gone after a refused purge (stat err = %v)", err)
			}
		})
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

// Only a CONFIRMED or AUTO_ACCEPTED edge may connect a Tier-1 cache to the
// Tier-3 master that authorises deleting it. A NEEDS_REVIEW edge is a guess
// nobody has looked at: a 0.6 filename-stem match to an unrelated RAW must
// never be what lets prune delete the only copy of an edit (AGENTS.md
// invariant 7).
func TestPlanRequiresConfirmedOrAutoAcceptedEdge(t *testing.T) {
	for _, tc := range []struct {
		review   string
		eligible bool
	}{
		{"CONFIRMED", true},
		{"AUTO_ACCEPTED", true},
		{"NEEDS_REVIEW", false},
		{"REJECTED", false},
	} {
		t.Run(tc.review, func(t *testing.T) {
			database := openTestDB(t)
			tier1ID := seedLocation(t, database, "t1", t.TempDir(), "TIER1_LOCAL_SCRATCH", false, true)
			tier3ID := seedLocation(t, database, "t3", t.TempDir(), "TIER3_MASTER_ARCHIVE", true, false)
			master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: "/archive/m.jpg", mtimeUnix: oldMtime, fullHash: hash64("v")})
			candidate := seedNode(t, database, nodeSpec{locationID: tier1ID, path: "/scratch/p.jpg", mtimeUnix: oldMtime})
			seedEdge(t, database, master.ID, candidate.ID, tc.review)

			got, err := Plan(context.Background(), database.Reader, tier1ID, cutoffUnix)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if tc.eligible && len(got) != 1 {
				t.Fatalf("%s edge: Plan = %+v, want the candidate", tc.review, got)
			}
			if !tc.eligible && len(got) != 0 {
				t.Fatalf("%s edge: Plan = %+v, want no candidates", tc.review, got)
			}
			ancestors, err := database.Reader.ListVerifiedTier3Ancestors(context.Background(), candidate.ID)
			if err != nil {
				t.Fatalf("ListVerifiedTier3Ancestors: %v", err)
			}
			if tc.eligible && len(ancestors) != 1 {
				t.Fatalf("%s edge: ancestors = %+v, want the master", tc.review, ancestors)
			}
			if !tc.eligible && len(ancestors) != 0 {
				t.Fatalf("%s edge: ancestors = %+v, want none (Execute's re-verify must agree with Plan)", tc.review, ancestors)
			}
		})
	}
}

// A candidate that fails the on-disk checks (here: unreachable Tier-3 master)
// must be rejected without ever waiting for the single writer connection, so
// slow stat calls on a dead mount never queue behind / hold the write lock.
func TestExecuteRejectsOnDiskFailuresWithoutWriterLock(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "cache.jpg")
	if err := os.WriteFile(filePath, []byte("cache content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileInfo, _ := os.Lstat(filePath)
	tier1ID := seedLocation(t, database, "t1", root, "TIER1_LOCAL_SCRATCH", false, true)
	tier3Root := t.TempDir()
	tier3ID := seedLocation(t, database, "t3", tier3Root, "TIER3_MASTER_ARCHIVE", true, false)
	guard := storage.NewGuard([]storage.Location{{ID: tier1ID, Name: "t1", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH"}})
	master := seedNode(t, database, nodeSpec{locationID: tier3ID, path: filepath.Join(tier3Root, "gone.jpg"), mtimeUnix: oldMtime, fullHash: hash64("gone")})
	node := seedNode(t, database, nodeSpec{locationID: tier1ID, path: filePath, mtimeUnix: oldMtime})
	seedEdge(t, database, master.ID, node.ID, "AUTO_ACCEPTED")

	// Occupy the only writer connection for the whole Execute call.
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = database.InTx(context.Background(), func(*sqlcgen.Queries) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { close(release); <-done }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := Execute(ctx, database, guard, []Candidate{{
		NodeID: node.ID, FilePath: node.FilePath, FileName: node.FileName, StorageLocationID: tier1ID,
		MtimeUnix: fileInfo.ModTime().Unix(), SizeBytes: fileInfo.Size(),
	}}, cutoffUnix)
	if len(results) != 1 || !errors.Is(results[0].Err, ErrAncestorUnreachable) {
		t.Fatalf("results = %+v, want ErrAncestorUnreachable without touching the writer", results)
	}
}
