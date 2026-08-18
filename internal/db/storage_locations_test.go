package db

import (
	"context"
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

// TestStorageLocationTierCheckEnforced proves the DB-level CHECK from
// docs/schema.md fix #1 (Tier 3 must be read_only) is real, independent of
// any application-level discipline about setting it correctly.
func TestStorageLocationTierCheckEnforced(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "bad-archive",
			RootPath: t.TempDir(),
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0, // violates CHECK (tier <> 'TIER3_MASTER_ARCHIVE' OR read_only = 1)
			Prunable: 0,
		})
		return err
	})
	if err == nil {
		t.Fatal("insert of a writable TIER3_MASTER_ARCHIVE location succeeded, want a CHECK constraint failure")
	}
}
