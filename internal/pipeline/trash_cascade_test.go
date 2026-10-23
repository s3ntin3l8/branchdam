package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// cascadeEnv is one writable location holding a master and its exports.
type cascadeEnv struct {
	db    *db.DB
	guard *storage.Guard
	root  string
	locID int64
}

func newCascadeEnv(t *testing.T) *cascadeEnv {
	t.Helper()
	return newCascadeEnvTier(t, "TIER2_EXPORTS")
}

// newCascadeEnvTier builds the same env as newCascadeEnv but on a location
// of the given tier, so a test can place a master and its exports on a
// Tier-3 archive. Tier 3 is writable by default (AGENTS.md invariant 3),
// which is exactly what makes the tier worth testing.
func newCascadeEnvTier(t *testing.T, tier string) *cascadeEnv {
	t.Helper()
	ctx := context.Background()
	database := openTestDB(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	name := "cascade-" + tier
	var loc sqlcgen.StorageLocation
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{Name: name, RootPath: root, Tier: tier})
		return err
	}))
	guard := storage.NewGuard([]storage.Location{{ID: loc.ID, Name: name, RootPath: root, Tier: tier}})
	return &cascadeEnv{db: database, guard: guard, root: root, locID: loc.ID}
}

// node writes rel under the root and inserts an ACTIVE node for it.
func (e *cascadeEnv) node(t *testing.T, rel string) sqlcgen.MediaNode {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(e.root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(rel), 0o644))
	var n sqlcgen.MediaNode
	require.NoError(t, e.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: uuid.New().String(), StorageLocationID: e.locID, FilePath: path, FileName: filepath.Base(path),
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	}))
	return n
}

// edge links parent -> child. CONFIRMED and REJECTED are reached the way
// production reaches them: insert, then review.
func (e *cascadeEnv) edge(t *testing.T, parentID, childID int64, rel, resolver, reviewState string) {
	t.Helper()
	ctx := context.Background()
	initial := reviewState
	if initial == "CONFIRMED" || initial == "REJECTED" {
		initial = "NEEDS_REVIEW"
	}
	require.NoError(t, e.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		created, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parentID, TargetNodeID: childID, RelationshipType: rel,
			Confidence: 0.8, Tier: 2, Resolver: resolver, EvidenceJson: "{}", ReviewState: initial,
		})
		if err != nil {
			return err
		}
		switch reviewState {
		case "CONFIRMED":
			_, err = q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{ID: created.ID})
		case "REJECTED":
			_, err = q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{ID: created.ID})
		}
		return err
	}))
}

func (e *cascadeEnv) state(t *testing.T, id int64) string {
	t.Helper()
	n, err := e.db.Reader.GetMediaNodeByID(context.Background(), id)
	require.NoError(t, err)
	return n.LifecycleState
}

func requireOnDisk(t *testing.T, n sqlcgen.MediaNode, want bool) {
	t.Helper()
	_, err := os.Stat(n.FilePath)
	if want {
		require.NoError(t, err, "%s must still be at its original path", n.FileName)
	} else {
		require.True(t, os.IsNotExist(err), "%s must have been moved off its original path", n.FileName)
	}
}

// TestTrashAsset_CascadesOnlyOverReviewedExportEdges: trashing a master with
// keepExports=false may only take exports along AUTO_ACCEPTED or CONFIRMED
// FINAL_EXPORT / immich_export edges. A NEEDS_REVIEW edge is an unreviewed
// guess and a REJECTED edge is a human saying "not linked"; their children
// (and their remote sync state) must survive untouched.
func TestTrashAsset_CascadesOnlyOverReviewedExportEdges(t *testing.T) {
	ctx := context.Background()
	env := newCascadeEnv(t)

	master := env.node(t, "master.tif")
	auto := env.node(t, "auto.jpg")
	confirmed := env.node(t, "confirmed.jpg")
	immich := env.node(t, "immich.jpg")
	needsReview := env.node(t, "needs_review.jpg")
	rejected := env.node(t, "rejected.jpg")
	immichUnreviewed := env.node(t, "immich_unreviewed.jpg")

	env.edge(t, master.ID, auto.ID, "FINAL_EXPORT", "test", "AUTO_ACCEPTED")
	env.edge(t, master.ID, confirmed.ID, "FINAL_EXPORT", "test", "CONFIRMED")
	env.edge(t, master.ID, immich.ID, "DERIVED_FROM", "immich_export", "AUTO_ACCEPTED")
	env.edge(t, master.ID, needsReview.ID, "FINAL_EXPORT", "test", "NEEDS_REVIEW")
	env.edge(t, master.ID, rejected.ID, "FINAL_EXPORT", "test", "REJECTED")
	env.edge(t, master.ID, immichUnreviewed.ID, "DERIVED_FROM", "immich_export", "NEEDS_REVIEW")

	require.NoError(t, env.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{NodeID: rejected.ID, Remote: "IMMICH", SyncStatus: "PUSHED"})
		return err
	}))

	_, err := TrashAsset(ctx, env.db, env.guard, testLogger(), master.ID, false)
	require.NoError(t, err)

	require.Equal(t, "TRASHED", env.state(t, master.ID))
	for _, n := range []sqlcgen.MediaNode{auto, confirmed, immich} {
		require.Equal(t, "TRASHED", env.state(t, n.ID), "%s is linked by a reviewed edge and must be cascaded", n.FileName)
		requireOnDisk(t, n, false)
	}
	for _, n := range []sqlcgen.MediaNode{needsReview, rejected, immichUnreviewed} {
		require.Equal(t, "ACTIVE", env.state(t, n.ID), "%s is not linked by a reviewed edge and must survive", n.FileName)
		requireOnDisk(t, n, true)
	}
	_, err = env.db.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: rejected.ID, Remote: "IMMICH"})
	require.NoError(t, err, "a REJECTED export's remote sync state must not be purged with the master")
}

// TestRestoreTrashedAsset_RestoresOnlyCascadedExports: restoring a master
// brings back exactly the exports its own trash cascaded, never an export
// the user trashed on its own -- before the master, or after the export had
// already come back once.
func TestRestoreTrashedAsset_RestoresOnlyCascadedExports(t *testing.T) {
	ctx := context.Background()
	env := newCascadeEnv(t)
	log := testLogger()

	master := env.node(t, "master.tif")
	cascaded := env.node(t, "cascaded.jpg")
	separate := env.node(t, "separate.jpg")
	env.edge(t, master.ID, cascaded.ID, "FINAL_EXPORT", "test", "AUTO_ACCEPTED")
	env.edge(t, master.ID, separate.ID, "FINAL_EXPORT", "test", "CONFIRMED")

	// The user trashes one export on its own, then trashes the master.
	_, err := TrashAsset(ctx, env.db, env.guard, log, separate.ID, false)
	require.NoError(t, err)
	_, err = TrashAsset(ctx, env.db, env.guard, log, master.ID, false)
	require.NoError(t, err)
	require.Equal(t, "TRASHED", env.state(t, cascaded.ID))

	_, err = RestoreTrashedAsset(ctx, env.db, env.guard, log, master.ID)
	require.NoError(t, err)
	require.Equal(t, "ACTIVE", env.state(t, master.ID))
	require.Equal(t, "ACTIVE", env.state(t, cascaded.ID), "the export the master's trash took must come back with it")
	requireOnDisk(t, cascaded, true)
	require.Equal(t, "TRASHED", env.state(t, separate.ID), "an export trashed on its own must stay trashed")
	requireOnDisk(t, separate, false)

	// A spent cascade must not resurrect a later, independent trash: trash
	// the master again (cascading the export), restore the export on its
	// own, trash it on its own, then restore the master.
	_, err = TrashAsset(ctx, env.db, env.guard, log, master.ID, false)
	require.NoError(t, err)
	require.Equal(t, "TRASHED", env.state(t, cascaded.ID))
	_, err = RestoreTrashedAsset(ctx, env.db, env.guard, log, cascaded.ID)
	require.NoError(t, err)
	_, err = TrashAsset(ctx, env.db, env.guard, log, cascaded.ID, false)
	require.NoError(t, err)

	_, err = RestoreTrashedAsset(ctx, env.db, env.guard, log, master.ID)
	require.NoError(t, err)
	require.Equal(t, "ACTIVE", env.state(t, master.ID))
	require.Equal(t, "TRASHED", env.state(t, cascaded.ID), "an export re-trashed on its own after its cascade was undone must stay trashed")
	requireOnDisk(t, cascaded, false)

	open, err := env.db.Reader.ListOpenTrashCascadeExports(ctx, master.ID)
	require.NoError(t, err)
	require.Empty(t, open, "every cascade must be closed once its export is back")
}

// TestTrashAsset_Tier3ExportIsNotPhysicallyMoved: a Tier-3 archive is
// writable by default (AGENTS.md invariant 3), so the export cascade must
// not read "not read-only" as "safe to rename into .trash/".
//
// The master's own trash path refuses to write into a Tier-3 archive -- the
// row is marked TRASHED and the bytes stay. The export path must refuse
// too, or a user-initiated trash physically relocates bytes inside the
// master archive, which is the one thing the master's path refuses to do.
func TestTrashAsset_Tier3ExportIsNotPhysicallyMoved(t *testing.T) {
	ctx := context.Background()
	env := newCascadeEnvTier(t, "TIER3_MASTER_ARCHIVE")
	log := testLogger()

	master := env.node(t, "master.tif")
	export := env.node(t, "export.jpg")
	env.edge(t, master.ID, export.ID, "FINAL_EXPORT", "test", "CONFIRMED")

	_, err := TrashAsset(ctx, env.db, env.guard, log, master.ID, false)
	require.NoError(t, err)

	require.Equal(t, "TRASHED", env.state(t, master.ID))
	// The export's bytes must still be where they were.
	requireOnDisk(t, export, true)
	// And a row whose bytes were never moved must not claim to be trashed:
	// there is nothing in .trash/ for a restore to bring back.
	require.Equal(t, "ACTIVE", env.state(t, export.ID),
		"an export whose bytes were never moved must not be marked trashed")

	open, err := env.db.Reader.ListOpenTrashCascadeExports(ctx, master.ID)
	require.NoError(t, err)
	require.Empty(t, open, "a cascade that never moved anything must not be recorded")
}
