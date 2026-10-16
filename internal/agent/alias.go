package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// LookupNodeByUUID resolves nodeUUID to its media_nodes row. A UUID with no
// row of its own falls back to node_uuid_aliases: when a device's
// EVENT_NODE_CREATED was content-deduplicated onto an existing node, the
// device's UUID was recorded there as an alias of that node (see
// recordContentAlias). Only an alias recorded by agentID itself is
// followed -- another device's alias resolves exactly like an unknown UUID.
//
// aliased reports that the returned node was reached through an alias, i.e.
// it is the canonical node, not one the device created under nodeUUID.
// Callers must not mutate it on the device's behalf (rebase, move and delete
// treat an aliased UUID as an acknowledged no-op). A UUID that is neither a
// node nor a followable alias returns sql.ErrNoRows.
func LookupNodeByUUID(ctx context.Context, q *sqlcgen.Queries, agentID, nodeUUID string) (node sqlcgen.MediaNode, aliased bool, err error) {
	node, err = q.GetMediaNodeByUUID(ctx, nodeUUID)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return node, false, err
	}
	alias, aliasErr := q.GetNodeUUIDAlias(ctx, nodeUUID)
	if aliasErr != nil {
		if errors.Is(aliasErr, sql.ErrNoRows) {
			return sqlcgen.MediaNode{}, false, sql.ErrNoRows
		}
		return sqlcgen.MediaNode{}, false, fmt.Errorf("lookup node uuid alias: %w", aliasErr)
	}
	if alias.AgentID != agentID {
		return sqlcgen.MediaNode{}, false, sql.ErrNoRows
	}
	node, err = q.GetMediaNodeByUUID(ctx, alias.NodeUuid)
	if err != nil {
		return sqlcgen.MediaNode{}, false, fmt.Errorf("lookup aliased node %s for %s: %w", alias.NodeUuid, nodeUUID, err)
	}
	return node, true, nil
}

// recordContentAlias records aliasUUID (the device's UUID for content the
// server already holds) as an alias of the existing node canonicalUUID.
// First writer wins; recording the same alias twice is a no-op.
func recordContentAlias(ctx context.Context, q *sqlcgen.Queries, aliasUUID, canonicalUUID, agentID string) error {
	if aliasUUID == canonicalUUID {
		return nil
	}
	if err := q.InsertNodeUUIDAlias(ctx, sqlcgen.InsertNodeUUIDAliasParams{
		AliasUuid: aliasUUID,
		NodeUuid:  canonicalUUID,
		AgentID:   agentID,
	}); err != nil {
		return fmt.Errorf("record node uuid alias %s -> %s: %w", aliasUUID, canonicalUUID, err)
	}
	return nil
}
