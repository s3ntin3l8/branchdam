package db

import (
	"context"
	"fmt"
)

// livePhashesSQL is the candidate set for GET /api/v1/assets/{id}/similar.
// It is not a sqlc :many query: that would materialise every live hash
// before ranking. ForEachLivePhash streams one row at a time.
const livePhashesSQL = `
SELECT id, node_uuid, file_path, file_name, thumb_state, phash
FROM media_nodes
WHERE lifecycle_state IN ('ACTIVE', 'HIDDEN')
  AND phash IS NOT NULL
`

// LivePhash is one candidate row for a similar-asset scan.
type LivePhash struct {
	ID         int64
	NodeUUID   string
	FilePath   string
	FileName   string
	ThumbState string
	Phash      int64
}

// ForEachLivePhash calls fn for each live node that has a perceptual
// hash. It does not buffer the result set.
func (d *DB) ForEachLivePhash(ctx context.Context, fn func(LivePhash) error) error {
	rows, err := d.reader.QueryContext(ctx, livePhashesSQL)
	if err != nil {
		return fmt.Errorf("list live phashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row LivePhash
		if err := rows.Scan(&row.ID, &row.NodeUUID, &row.FilePath, &row.FileName, &row.ThumbState, &row.Phash); err != nil {
			return fmt.Errorf("scan live phash: %w", err)
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list live phashes: %w", err)
	}
	return nil
}
