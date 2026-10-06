package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

var (
	// ErrNotNodeOwner is fatal: a paired device may only rebase, move or
	// delete nodes it created itself. Server-scanned nodes, web uploads and
	// other devices' nodes (created_by_agent_id NULL or different) are out
	// of bounds even though the device can name them by UUID.
	ErrNotNodeOwner = errors.New("agent: node was not created by this device")

	// ErrHashConflict is fatal: a device tried to replace a node's
	// existing full_hash with a different value. A content hash identifies
	// the bytes; it is set once and never re-asserted by an agent.
	ErrHashConflict = errors.New("agent: refusing to overwrite an existing full_hash with a different value")
)

var (
	nodeUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	fullHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidNodeUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
// Agent-supplied UUIDs end up in file names (trash suffix, thumbnail cache
// shards) and must never carry path separators or other free text.
func ValidNodeUUID(s string) bool { return nodeUUIDRe.MatchString(s) }

// ValidFullHash reports whether s is a lowercase 64-char hex BLAKE3-256.
func ValidFullHash(s string) bool { return fullHashRe.MatchString(s) }

// RequireNodeOwner returns nil only when nodeUUID was created by agentID.
// A node with no recorded creator is not owned by any device.
func RequireNodeOwner(ctx context.Context, q *sqlcgen.Queries, agentID, nodeUUID string) error {
	creator, err := q.GetNodeCreator(ctx, nodeUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No creator row: not a device-created node (or it doesn't
			// exist at all -- callers look the node up separately).
			return fmt.Errorf("%w: node uuid %s", ErrNotNodeOwner, nodeUUID)
		}
		return fmt.Errorf("lookup node creator: %w", err)
	}
	if creator == "" || creator != agentID {
		return fmt.Errorf("%w: node uuid %s", ErrNotNodeOwner, nodeUUID)
	}
	return nil
}

// FullHashUpdate decides what to do with an agent-supplied full_hash for an
// existing node: apply it (existing is empty), skip it (identical), or
// reject it (malformed, or it would replace a different existing value).
func FullHashUpdate(existing *string, incoming string) (apply bool, err error) {
	if !ValidFullHash(incoming) {
		return false, fmt.Errorf("%w: fullHash must be 64 lowercase hex characters", ErrMalformedPayload)
	}
	if existing == nil || *existing == "" {
		return true, nil
	}
	if *existing == incoming {
		return false, nil
	}
	return false, ErrHashConflict
}
