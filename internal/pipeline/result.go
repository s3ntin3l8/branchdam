// Package pipeline turns indexer.Records and computed hashes into
// media_nodes/media_edges rows. It owns the ONLY write access to those
// tables outside migrations: internal/graph (edge resolution) and
// internal/httpapi both go through Commit, never sqlcgen directly.
package pipeline

import "time"

// Result is one file's fully-computed index data, ready to commit. It is
// produced by a worker job (indexer.Walk/Watch -> workers.Pool -> hashing +
// probe), entirely OUTSIDE any database transaction -- Commit only reads and
// writes rows given an already-resolved Result, so the single writer
// connection is never held during file I/O or subprocess execution.
type Result struct {
	Path     string
	FileName string
	FileExt  string
	Size     int64
	ModTime  time.Time

	FastHash string // always populated -- xxHash64, 16 hex chars
	FullHash string // empty means "not computed for this pass" (see docs/schema.md fix #8's policy)
	PHash    *int64

	// Promoted EXIF fields -- exactly the set the schema has columns for
	// (docs/schema.md's media_nodes). CameraMake is deliberately absent:
	// only camera_model is a promoted column (it's what the Tier-2
	// "same camera" resolver signal uses); Make lives in node_metadata
	// overflow once that's wired up, not here. Empty/nil when probe.Exif
	// wasn't run or found nothing -- Commit never requires these to be
	// present.
	OriginalDocumentID string
	DocumentID         string
	DerivedFromID      string
	CapturedAt         *time.Time
	CameraModel        string
}

// Stats summarizes what a Commit call actually did, for scan_jobs progress
// reporting and test assertions.
type Stats struct {
	Inserted          int // brand new nodes
	Touched           int // same content at the same path, no new row
	VersionCollisions int // docs/schema.md fix #3: old archived, new inserted
	Moved             int // Pillar 5: MISSING node's path rebased
}
