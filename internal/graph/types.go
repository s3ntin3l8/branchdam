// Package graph resolves lineage edges between media nodes. Increment 1
// registers two Tier-2 resolvers (XMPOriginalDocumentIDResolver and
// FilenameStemResolver); Tier 1 (project introspection) and Tier 3
// (heuristic spatial-temporal matching) are a Register call away later,
// not a schema change -- the Resolver interface and the tier field on
// every edge already accommodate them.
package graph

import (
	"context"
	"time"
)

// Node is the subset of a media_nodes row resolvers need to compare a
// child against candidate parents. Deliberately narrower than
// sqlcgen.MediaNode -- resolvers work with plain Go values, not generated
// database types, so they stay testable without a database.
type Node struct {
	ID                 int64
	FilePath           string
	FileName           string
	FileExt            string
	OriginalDocumentID string
	DocumentID         string
	CapturedAt         *time.Time
	CameraModel        string
	FilenameStem       string
	PHash              *int64
	CameraSerial       string
	LensModel          string
}

// Candidate is one resolver's proposed edge between two nodes, not yet
// persisted. Engine merges candidates from every resolver, applies the
// confidence threshold, and commits survivors.
type Candidate struct {
	ParentID   int64
	ChildID    int64
	Rel        string // relationship_type
	Confidence float64
	Tier       int
	Resolver   string
	Evidence   map[string]any
}

// Lookup is the read-only view into live nodes a Resolver needs. Backed by
// db.Reader in production (see lookup.go); a fake in tests never touches a
// database.
type Lookup interface {
	// ByOriginalDocumentID returns live nodes whose document_id matches --
	// the xmpOriginalDocumentID resolver's candidate-parent source.
	ByOriginalDocumentID(ctx context.Context, documentID string) ([]Node, error)
	// ByFilenameStem returns live nodes sharing a normalized filename stem
	// -- the filenameStem resolver's candidate-parent source.
	ByFilenameStem(ctx context.Context, stem string) ([]Node, error)
	// BySpatialTemporal returns candidate parents sharing camera_serial within a
	// window of capturedAt, excluding excludeID.
	BySpatialTemporal(ctx context.Context, cameraSerial string, capturedAt time.Time, window time.Duration, excludeID int64) ([]Node, error)
	// ByPath returns the live node at exact file_path, or nil if not found.
	ByPath(ctx context.Context, filePath string) (*Node, error)
	// ByFileName returns live nodes sharing exact file_name.
	ByFileName(ctx context.Context, fileName string) ([]Node, error)
}

// Resolver proposes lineage edges for one child node. Resolve must not
// mutate the database -- Engine owns all writes, inside one transaction,
// after merging every resolver's candidates.
type Resolver interface {
	Name() string
	Tier() int
	Resolve(ctx context.Context, child Node, lookup Lookup) ([]Candidate, error)
}
