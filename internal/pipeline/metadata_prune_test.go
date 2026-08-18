package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// TestPruneArchivedNodeMetadata backs #89: creating N version collisions on one
// path must leave superseded media_nodes archived with their superseded_by
// chains intact, but prune their node_metadata rows so storage stays bounded
// rather than growing monotonically with N.
func TestPruneArchivedNodeMetadata(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedLocation(t, database, "TIER2_EXPORTS", false)

	const nVersions = 5
	const filePath = "/exports/render_loop.jpg"

	var nodeIDs []int64
	for i := 1; i <= nVersions; i++ {
		fastHash := fmt.Sprintf("%016x", i)

		res := Result{
			Path:         filePath,
			FileName:     "render_loop.jpg",
			FileExt:      "jpg",
			Size:         int64(i * 1000),
			ModTime:      time.Now(),
			FastHash:     fastHash,
			Make:         "Sony",
			LensModel:    "FE 24-70mm F2.8 GM",
			SerialNumber: fmt.Sprintf("SN%06d", i),
			ExifRaw: map[string]string{
				"EXIF:ISO": fmt.Sprintf("%d00", i),
			},
		}

		stats, err := Commit(ctx, database, locationID, []Result{res})
		if err != nil {
			t.Fatalf("Commit version %d: %v", i, err)
		}
		if i == 1 && stats.Inserted != 1 {
			t.Fatalf("v1 stats = %+v, want Inserted=1", stats)
		}
		if i > 1 && stats.VersionCollisions != 1 {
			t.Fatalf("v%d stats = %+v, want VersionCollisions=1", i, stats)
		}

		liveNode := mustGetLiveNode(t, database, filePath)
		nodeIDs = append(nodeIDs, liveNode.ID)
	}

	if len(nodeIDs) != nVersions {
		t.Fatalf("created %d nodes, want %d", len(nodeIDs), nVersions)
	}

	// Prior to pruning, every node has 4 metadata rows (Make, LensModel, SerialNumber, EXIF:ISO)
	for i, nid := range nodeIDs {
		rows, err := database.Reader.ListNodeMetadata(ctx, nid)
		if err != nil {
			t.Fatalf("ListNodeMetadata for node %d: %v", nid, err)
		}
		if len(rows) != 4 {
			t.Errorf("node %d (v%d) has %d metadata rows before prune, want 4", nid, i+1, len(rows))
		}
	}

	// Execute pruning of archived node metadata
	var pruned int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		pruned, err = q.PruneArchivedNodeMetadata(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("PruneArchivedNodeMetadata: %v", err)
	}

	// 4 archived nodes * 4 rows each = 16 pruned rows
	expectedPruned := int64((nVersions - 1) * 4)
	if pruned != expectedPruned {
		t.Errorf("pruned = %d rows, want %d", pruned, expectedPruned)
	}

	// Archived nodes (0..nVersions-2) must now have 0 metadata rows
	for i := 0; i < nVersions-1; i++ {
		nid := nodeIDs[i]
		rows, err := database.Reader.ListNodeMetadata(ctx, nid)
		if err != nil {
			t.Fatalf("ListNodeMetadata for archived node %d: %v", nid, err)
		}
		if len(rows) != 0 {
			t.Errorf("archived node %d (v%d) still has %d metadata rows after prune, want 0", nid, i+1, len(rows))
		}
	}

	// The latest live node (nodeIDs[nVersions-1]) must still have all 4 metadata rows
	latestNodeID := nodeIDs[nVersions-1]
	liveRows, err := database.Reader.ListNodeMetadata(ctx, latestNodeID)
	if err != nil {
		t.Fatalf("ListNodeMetadata for live node %d: %v", latestNodeID, err)
	}
	if len(liveRows) != 4 {
		t.Errorf("live node %d has %d metadata rows, want 4", latestNodeID, len(liveRows))
	}

	// All media_nodes rows must remain in the DB and their superseded_by chain intact
	for i := 0; i < nVersions; i++ {
		nid := nodeIDs[i]
		node, err := database.Reader.GetMediaNodeByID(ctx, nid)
		if err != nil {
			t.Fatalf("GetMediaNodeByID(%d): %v", nid, err)
		}
		if i < nVersions-1 {
			if node.LifecycleState != "ARCHIVED" {
				t.Errorf("node %d lifecycle_state = %q, want ARCHIVED", nid, node.LifecycleState)
			}
			expectedSupersededBy := nodeIDs[i+1]
			if !node.SupersededBy.Valid || node.SupersededBy.Int64 != expectedSupersededBy {
				t.Errorf("node %d superseded_by = %v, want %d", nid, node.SupersededBy, expectedSupersededBy)
			}
		} else {
			if node.LifecycleState != "ACTIVE" {
				t.Errorf("latest node %d lifecycle_state = %q, want ACTIVE", nid, node.LifecycleState)
			}
			if node.SupersededBy.Valid {
				t.Errorf("latest node %d has superseded_by = %v, want null", nid, node.SupersededBy)
			}
		}
	}
}
