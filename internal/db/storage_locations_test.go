package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// TestListStorageLocationsSatisfiesGuardLoader is the load-bearing test for
// the adapter in storage_locations.go: *DB must structurally satisfy the
// interface storage.LoadGuard needs, end to end against a real migrated
// database, not just against a fake.
func TestListStorageLocationsSatisfiesGuardLoader(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	archiveDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(archiveDir)
	if err != nil {
		t.Fatalf("resolve fixture dir: %v", err)
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "archive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 1,
			Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed storage_locations: %v", err)
	}

	guard, _, err := storage.LoadGuard(ctx, database, nil)
	if err != nil {
		t.Fatalf("LoadGuard(ctx, *db.DB): %v", err)
	}

	loc, err := guard.Resolve(filepath.Join(archiveDir, "DSC001.ARW"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !loc.ReadOnly || loc.Tier != "TIER3_MASTER_ARCHIVE" {
		t.Errorf("Resolve = %+v, want read-only TIER3_MASTER_ARCHIVE", loc)
	}
	if loc.RootPath != resolved {
		t.Errorf("RootPath = %q, want canonicalized %q", loc.RootPath, resolved)
	}

	if err := guard.CheckWrite(filepath.Join(archiveDir, "new.jpg")); err == nil {
		t.Fatal("CheckWrite against the DB-seeded archive location succeeded, want a read-only refusal")
	}
}

// TestListStorageLocationsIncludesDisabledLocations pins the narrow,
// verified semantics of a UI "enabled: false" override (docs/operations.md):
// disabling a location sets is_active = 0 via the same path M6 uses for a
// vanished mount, but storage.StorageLocationRow carries no IsActive field
// at all (see this file's ListStorageLocations), so LoadGuard structurally
// cannot filter on it -- reads through Guard.OpenRead must keep working on
// a location an operator disabled from the UI, exactly as they do for one
// that's merely display-inactive after a self-healed mount failure.
func TestListStorageLocationsIncludesDisabledLocations(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve fixture dir: %v", err)
	}

	var id int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "scratch",
			RootPath: dir,
			Tier:     "TIER1_LOCAL_SCRATCH",
			ReadOnly: 0,
			Prunable: 0,
		})
		id = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed storage_locations: %v", err)
	}

	// Disable it the same way an "enabled": false override's next-boot seed
	// would (main.go never calls SetStorageLocationActive directly for this
	// case, but the effect on is_active is identical to M6's own use of it).
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.SetStorageLocationActive(ctx, sqlcgen.SetStorageLocationActiveParams{ID: id, IsActive: 0})
	}); err != nil {
		t.Fatalf("SetStorageLocationActive: %v", err)
	}

	guard, skipped, err := storage.LoadGuard(ctx, database, nil)
	if err != nil {
		t.Fatalf("LoadGuard: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none -- a disabled-but-resolvable location must not be excluded from the Guard", skipped)
	}

	if _, err := guard.OpenRead(filepath.Join(dir, "already-indexed.jpg")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenRead on a disabled location returned %v, want either success or ErrNotExist for the missing test file -- never ErrUnknownLocation/ErrReadOnlyTier", err)
	}
	loc, err := guard.Resolve(filepath.Join(dir, "x.jpg"))
	if err != nil {
		t.Fatalf("Resolve on a disabled location failed: %v, want it to still resolve -- enabled:false doesn't remove a location from the Guard", err)
	}
	if loc.RootPath != resolved {
		t.Errorf("RootPath = %q, want canonicalized %q", loc.RootPath, resolved)
	}
}

// TestStorageLocationTierCheckEnforced proves that migration 00012 permits
// writable TIER3_MASTER_ARCHIVE locations while still enforcing the DB-level
// CHECK that only TIER1_LOCAL_SCRATCH locations can be marked prunable.
func TestStorageLocationTierCheckEnforced(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	// Writable TIER3_MASTER_ARCHIVE is permitted
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "writable-archive",
			RootPath: t.TempDir(),
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("insert of writable TIER3_MASTER_ARCHIVE failed: %v", err)
	}

	// Prunable non-Tier-1 is rejected by CHECK (tier = 'TIER1_LOCAL_SCRATCH' OR prunable = 0)
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "bad-prunable-archive",
			RootPath: t.TempDir(),
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 1, // violates CHECK (tier = 'TIER1_LOCAL_SCRATCH' OR prunable = 0)
		})
		return err
	})
	if err == nil {
		t.Fatal("insert of a prunable TIER3_MASTER_ARCHIVE location succeeded, want a CHECK constraint failure")
	}
}
