package db

import (
	"context"
	"fmt"

	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// ListStorageLocations adapts sqlcgen's generated row type into
// storage.StorageLocationRow, which makes *DB satisfy the unexported
// interface storage.LoadGuard needs -- structurally, without internal/storage
// importing this package's generated code. The bool conversion exists
// because SQLite has no native boolean type: read_only is declared INTEGER
// CHECK(x IN (0,1)) in the schema, so sqlc emits int64.
func (d *DB) ListStorageLocations(ctx context.Context) ([]storage.StorageLocationRow, error) {
	rows, err := d.Reader.ListStorageLocations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list storage locations: %w", err)
	}

	out := make([]storage.StorageLocationRow, len(rows))
	for i, r := range rows {
		out[i] = storage.StorageLocationRow{
			ID:       r.ID,
			Name:     r.Name,
			RootPath: r.RootPath,
			Tier:     r.Tier,
			ReadOnly: r.ReadOnly != 0,
		}
	}
	return out, nil
}
