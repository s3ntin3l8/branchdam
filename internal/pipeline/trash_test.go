// Tests for the .trash/ filename-uniqueness invariant. moveToTrash embeds
// the node_uuid in the trash filename so two nodes that trash the same
// rel_path never clobber each other; restoreFromTrash computes the
// deterministic trash path from (rel_path, node_uuid) and restores
// exactly that node's bytes.
//
// These tests exercise moveToTrash and restoreFromTrash directly without
// needing a full DB/tier setup -- a temp dir + a fake Guard constructed
// with a single location is sufficient.

package pipeline

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// testLogger returns a discarding slog logger -- trash tests don't assert
// on log output, just on disk + error paths.
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTrashTestGuard(t *testing.T, root string) *storage.Guard {
	t.Helper()
	guard := storage.NewGuard(nil)
	guard.ReloadLocations([]storage.Location{
		{ID: 1, Name: "staging", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: false},
	})
	return guard
}

func TestMoveToTrash_EmbedsNodeUUIDInFilename(t *testing.T) {
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	// Create a source file at /tmp/.../IMG_0001.JPG.
	srcDir := filepath.Join(root, "2026", "08")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	src := filepath.Join(srcDir, "IMG_0001.JPG")
	require.NoError(t, os.WriteFile(src, []byte("bytes-A"), 0o644))

	// Trash it with node-uuid-A.
	const uuidA = "018d3b2f-7630-7e50-9844-3d96e9592491"
	require.NoError(t, moveToTrash(guard, root, src, uuidA, testLogger()))

	// Source is gone.
	_, err := os.Stat(src)
	require.True(t, os.IsNotExist(err), "source file must be moved out of its original location")

	// Trash file is at .trash/2026/08/IMG_0001.<full uuidA>.JPG.
	expected := filepath.Join(root, ".trash", "2026", "08", "IMG_0001."+uuidA+".JPG")
	got, err := os.ReadFile(expected)
	require.NoError(t, err)
	require.Equal(t, []byte("bytes-A"), got)
}

func TestMoveToTrash_TwoNodesSameRelPathDoNotClobberEachOther(t *testing.T) {
	// Hermes review (warning, trash.go:333): if file B is re-ingested at
	// the same rel_path as a still-trashed file A and then B is trashed,
	// restoring B must not pick up A's bytes. Embed node_uuid in the
	// trash filename so each node's copy is uniquely recoverable.
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	srcDir := filepath.Join(root, "2026", "08")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	src := filepath.Join(srcDir, "IMG_9999.JPG")
	require.NoError(t, os.WriteFile(src, []byte("node-B bytes"), 0o644))

	// Pre-existing A copy at the canonical .trash/IMG_9999.JPG path.
	trashRoot := filepath.Join(root, ".trash", "2026", "08")
	require.NoError(t, os.MkdirAll(trashRoot, 0o755))
	preExisting := filepath.Join(trashRoot, "IMG_9999.JPG")
	require.NoError(t, os.WriteFile(preExisting, []byte("node-A bytes"), 0o644))

	// Trash node B -- must not clobber the pre-existing file.
	const uuidB = "cafef00d-1234-5678-9abc-def012345678"
	require.NoError(t, moveToTrash(guard, root, src, uuidB, testLogger()))

	// A is still there untouched.
	aData, err := os.ReadFile(preExisting)
	require.NoError(t, err)
	require.Equal(t, []byte("node-A bytes"), aData, "pre-existing trash copy must not be clobbered")

	// B is at its uuid-suffixed path.
	bPath := filepath.Join(trashRoot, "IMG_9999."+uuidB+".JPG")
	bData, err := os.ReadFile(bPath)
	require.NoError(t, err)
	require.Equal(t, []byte("node-B bytes"), bData)
}

func TestRestoreFromTrash_RestoresCorrectNodeFromUUIDSuffix(t *testing.T) {
	// Reverse direction of the collision test: restore node B without
	// touching node A's still-trashed copy.
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	trashRoot := filepath.Join(root, ".trash", "2026", "08")
	require.NoError(t, os.MkdirAll(trashRoot, 0o755))

	const uuidA = "018d3b2f-7630-7e50-9844-3d96e9592491"
	const uuidB = "cafef00d-1234-5678-9abc-def012345678"

	// A is trashed (still on disk).
	trashA := filepath.Join(trashRoot, "IMG_9999."+uuidA[:8]+".JPG")
	require.NoError(t, os.WriteFile(trashA, []byte("node-A bytes"), 0o644))
	// B is trashed too.
	trashB := filepath.Join(trashRoot, "IMG_9999."+uuidB[:8]+".JPG")
	require.NoError(t, os.WriteFile(trashB, []byte("node-B bytes"), 0o644))

	// Original path for B's restore. Original path for A is empty -- A
	// stays in .trash/ until its own restore is requested.
	origB := filepath.Join(root, "2026", "08", "IMG_9999.JPG")
	require.NoError(t, os.MkdirAll(filepath.Dir(origB), 0o755))

	require.NoError(t, restoreFromTrash(guard, origB, uuidB, testLogger(), nil))

	// B's bytes are at the original path.
	got, err := os.ReadFile(origB)
	require.NoError(t, err)
	require.Equal(t, []byte("node-B bytes"), got, "restoreFromTrash must restore exactly node-B's bytes, not node-A's")

	// A's trash copy is untouched.
	aData, err := os.ReadFile(trashA)
	require.NoError(t, err)
	require.Equal(t, []byte("node-A bytes"), aData)
}

func TestRestoreFromTrash_MissingFileReturnsErrAssetTrashFileMissing(t *testing.T) {
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	// No trash copy at all; original path missing too.
	orig := filepath.Join(root, "2026", "08", "IMG_MISSING.JPG")
	err := restoreFromTrash(guard, orig, "018d3b2f-7630-7e50-9844-3d96e9592499", testLogger(), nil)
	require.ErrorIs(t, err, ErrAssetTrashFileMissing)
}

func TestMoveToTrash_MissingSourceFileIsOK(t *testing.T) {
	// The DB row's TRASHED state is the durable record of intent. If the
	// source file is missing (user moved/deleted it manually between
	// scan and trash), log a warning and return nil -- the tx still
	// flips the row to TRASHED.
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	missing := filepath.Join(root, "2026", "08", "GONE.JPG")
	// file does NOT exist on disk
	require.NoError(t, moveToTrash(guard, root, missing, "018d3b2f-7630-7e50-9844-3d96e95924aa", testLogger()))
	// no panic, no error, no trash copy created
	trashPath := filepath.Join(root, ".trash", "2026", "08", "GONE."+"018d3b2f"[:8]+".JPG")
	_, err := os.Stat(trashPath)
	require.True(t, os.IsNotExist(err))
}

func TestRestoreFromTrash_LogicalOnlyTrashIsNoop(t *testing.T) {
	// Tier 3 / read-only / virtual tiers: the bytes never moved during
	// trash, so the original path always still has the file. Restore
	// is a logical-only no-op (the DB row flips to ACTIVE; no disk
	// move). Confirms the "original exists, no trash copy" branch.
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	srcDir := filepath.Join(root, "2026", "08")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	orig := filepath.Join(srcDir, "TIER3.JPG")
	require.NoError(t, os.WriteFile(orig, []byte("tier3 protected bytes"), 0o644))

	// No trash copy on disk -- logical-only trash scenario.
	err := restoreFromTrash(guard, orig, "018d3b2f-7630-7e50-9844-3d96e95924aa", testLogger(), nil)
	require.NoError(t, err)

	// Original is untouched.
	data, err := os.ReadFile(orig)
	require.NoError(t, err)
	require.Equal(t, []byte("tier3 protected bytes"), data)
}

func TestRestoreFromTrash_BothCopiesExistReturnsErrAssetAlreadyExists(t *testing.T) {
	// A re-ingest or manual copy may have placed new bytes at the
	// original path while a trash copy still exists. Refuse to silently
	// clobber either copy: this is the "ambiguous restore" branch.
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	srcDir := filepath.Join(root, "2026", "08")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	orig := filepath.Join(srcDir, "BOTH.JPG")
	require.NoError(t, os.WriteFile(orig, []byte("new bytes from re-ingest"), 0o644))

	uuidShort := "cafef00d"[:8]
	trashPath := filepath.Join(root, ".trash", "2026", "08", "BOTH."+uuidShort+".JPG")
	require.NoError(t, os.MkdirAll(filepath.Dir(trashPath), 0o755))
	require.NoError(t, os.WriteFile(trashPath, []byte("trashed bytes"), 0o644))

	err := restoreFromTrash(guard, orig, "cafef00d-1234-5678-9abc-def012345678", testLogger(), nil)
	require.ErrorIs(t, err, ErrAssetAlreadyExists)

	// Neither copy is clobbered.
	origData, err := os.ReadFile(orig)
	require.NoError(t, err)
	require.Equal(t, []byte("new bytes from re-ingest"), origData)
	trashData, err := os.ReadFile(trashPath)
	require.NoError(t, err)
	require.Equal(t, []byte("trashed bytes"), trashData)
}

// UUIDv7s minted within the same ~65s window share their first 8 hex chars.
// The old 8-char suffix gave both nodes the same trash name, so the second
// rename silently overwrote the first node's bytes.
func TestMoveToTrash_UUIDsSharingEightCharPrefixDoNotClobber(t *testing.T) {
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)
	const uuidA = "018d3b2f-7630-7e50-9844-3d96e9592491"
	const uuidB = "018d3b2f-aaaa-7e50-9844-3d96e95924bb" // same first 8 chars

	dir := filepath.Join(root, "2026")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	src := filepath.Join(dir, "IMG.JPG")

	require.NoError(t, os.WriteFile(src, []byte("A"), 0o644))
	require.NoError(t, moveToTrash(guard, root, src, uuidA, testLogger()))
	require.NoError(t, os.WriteFile(src, []byte("B"), 0o644)) // re-ingested at the same path
	require.NoError(t, moveToTrash(guard, root, src, uuidB, testLogger()))

	a, err := os.ReadFile(filepath.Join(root, ".trash", "2026", "IMG."+uuidA+".JPG"))
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(root, ".trash", "2026", "IMG."+uuidB+".JPG"))
	require.NoError(t, err)
	require.Equal(t, []byte("A"), a, "first node's trash copy must survive")
	require.Equal(t, []byte("B"), b)
}

// Restoring a node trashed under the legacy 8-char naming still works.
func TestRestoreFromTrash_FindsLegacyShortSuffixCopy(t *testing.T) {
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)
	const u = "018d3b2f-7630-7e50-9844-3d96e9592491"
	legacy := filepath.Join(root, ".trash", "2026", "IMG."+u[:8]+".JPG")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	require.NoError(t, os.WriteFile(legacy, []byte("old-format"), 0o644))
	orig := filepath.Join(root, "2026", "IMG.JPG")
	require.NoError(t, os.MkdirAll(filepath.Dir(orig), 0o755))

	require.NoError(t, restoreFromTrash(guard, orig, u, testLogger(), nil))
	got, err := os.ReadFile(orig)
	require.NoError(t, err)
	require.Equal(t, []byte("old-format"), got)
}

// When a later step of TrashAsset fails, the transaction rolls back the row
// changes -- the file move must be undone too, or the node stays ACTIVE in
// the DB with its bytes stranded in .trash/.
func TestTrashAsset_FailureAfterMoveRestoresFile(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	var nodeID int64
	path := filepath.Join(root, "2026", "clip.mov")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("clip bytes"), 0o644))
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{Name: "staging", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH"})
		if err != nil {
			return err
		}
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "018d3b2f-7630-7e50-9844-3d96e9592491", StorageLocationID: loc.ID, FilePath: path, FileName: "clip.mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		nodeID = n.ID
		return err
	}))
	// Test-only fault injection: make the final MarkNodeTrashed step fail.
	_, err := database.ExecInTx(ctx, `CREATE TRIGGER fail_trash BEFORE UPDATE OF lifecycle_state ON media_nodes
WHEN NEW.lifecycle_state = 'TRASHED' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)

	_, err = TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.Error(t, err)

	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr, "the file must be back at its original path after the failed trash")
	require.Equal(t, []byte("clip bytes"), got)
	entries, _ := os.ReadDir(filepath.Join(root, ".trash", "2026"))
	require.Empty(t, entries, "nothing may be left stranded in .trash/")
}

// A restore whose later DB step fails must put the file back into .trash/:
// the row is still TRASHED, so its bytes belong there.
func TestRestoreTrashedAsset_FailureAfterMoveReturnsFileToTrash(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	root := t.TempDir()
	guard := newTrashTestGuard(t, root)

	var nodeID int64
	path := filepath.Join(root, "2026", "clip.mov")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("clip bytes"), 0o644))
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{Name: "staging", RootPath: root, Tier: "TIER1_LOCAL_SCRATCH"})
		if err != nil {
			return err
		}
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "018d3b2f-7630-7e50-9844-3d96e9592491", StorageLocationID: loc.ID, FilePath: path, FileName: "clip.mov",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		nodeID = n.ID
		return err
	}))
	_, err := TrashAsset(ctx, database, guard, testLogger(), nodeID, false)
	require.NoError(t, err)

	_, err = database.ExecInTx(ctx, `CREATE TRIGGER fail_untrash BEFORE UPDATE OF lifecycle_state ON media_nodes
WHEN NEW.lifecycle_state = 'ACTIVE' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)

	_, err = RestoreTrashedAsset(ctx, database, guard, testLogger(), nodeID)
	require.Error(t, err)

	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "restored file must be moved back into .trash/ when the restore rolls back")
	entries, _ := os.ReadDir(filepath.Join(root, ".trash", "2026"))
	require.Len(t, entries, 1)
}
