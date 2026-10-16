package db

import (
	"context"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// Values of full_hash_provenance.source (00041_full_hash_provenance.sql).
const (
	// HashSourceServer marks a full_hash the server computed from the bytes
	// itself: a scan, an upload it streamed, or internal/rehash's re-read.
	// Only these authorise Tier-1 pruning and node-status "verified".
	HashSourceServer = "SERVER"
	// HashSourceAgent marks a full_hash a paired device asserted (an agent
	// event or POST /api/v1/agent/rebase). It authorises nothing until
	// internal/rehash re-reads the file and matches it.
	HashSourceAgent = "AGENT"
)

// RecordFullHashSource records that nodeID's full_hash was just set to
// fullHash by source (HashSourceServer or HashSourceAgent). Call it inside
// the same transaction as the write of a new full_hash value. A nil or
// non-64-character hash is a no-op, mirroring the schema's own CHECK, so
// call sites can pass the pointer they just wrote without re-checking it.
func RecordFullHashSource(ctx context.Context, q *sqlcgen.Queries, nodeID int64, fullHash *string, source string) error {
	if fullHash == nil || len(*fullHash) != 64 {
		return nil
	}
	return q.RecordFullHashProvenance(ctx, sqlcgen.RecordFullHashProvenanceParams{
		NodeID:   nodeID,
		FullHash: *fullHash,
		Source:   source,
	})
}
