package httpapi

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// syncGuardFromDB rebuilds the server's Guard from the storage_locations
// rows. Production builds the Guard and those rows from the same config, so
// they always agree; tests that insert rows straight into the DB must sync
// the Guard themselves. (Uploads used to "work" without this only because
// the engine fell back to raw os calls for paths the Guard did not know --
// which is exactly the escape hatch that was removed.)
func syncGuardFromDB(t *testing.T, srv *Server, database *db.DB) {
	t.Helper()
	rows, err := database.Reader.ListStorageLocations(context.Background())
	if err != nil {
		t.Fatalf("list storage locations: %v", err)
	}
	locs := make([]storage.Location, 0, len(rows))
	for _, r := range rows {
		root, err := filepath.EvalSymlinks(r.RootPath)
		if err != nil {
			continue
		}
		locs = append(locs, storage.Location{ID: r.ID, Name: r.Name, RootPath: filepath.Clean(root), Tier: r.Tier, ReadOnly: r.ReadOnly != 0})
	}
	srv.guard.ReloadLocations(locs)
}
