package graph

import (
	"context"
	"fmt"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// RecomputeStatusFromPersistedEdges re-derives nodeID's graph_status from ALL
// of its current live parent edges (ListEdgesByTarget), not just whichever
// one edge a caller just touched -- a single edge decision says nothing by
// itself about whether the node overall is linked.
//
// This is deliberately NOT the same computation as Engine.ResolveAndCommit's
// inline status update (below, around line 190): that one derives status
// from in-memory candidates mid-resolution, before anything is persisted.
// This one re-derives status from media_edges rows that are already
// committed, and is meant to be called by anything that mutates an edge's
// review_state outside of Engine's own resolve pass -- today that's
// httpapi's confirm/reject/manual-edge handlers and internal/agent's
// applyEdgeAttached. Do not "unify" the two: mixing a mid-resolution,
// candidate-based computation with a persisted-edge one would change
// behavior in ways neither caller intends.
//
// Precedence mirrors Engine.ResolveAndCommit's LINKED/NEEDS_REVIEW rule (an
// AUTO_ACCEPTED or CONFIRMED edge wins outright; a REJECTED edge is never
// enough on its own to justify NEEDS_REVIEW), extended with the
// no-edges-at-all-considered case Engine never has to handle (it only ever
// runs after at least one candidate existed): if every edge is REJECTED, or
// the node has none, the status reverts to UNLINKED rather than being left
// stuck at whatever it was before.
func RecomputeStatusFromPersistedEdges(ctx context.Context, q *sqlcgen.Queries, nodeID int64) error {
	edges, err := q.ListEdgesByTarget(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("list edges for graph_status recompute: %w", err)
	}
	status := "UNLINKED"
	for _, e := range edges {
		if e.ReviewState == "AUTO_ACCEPTED" || e.ReviewState == "CONFIRMED" {
			status = "LINKED"
			break
		}
		if e.ReviewState != "REJECTED" {
			status = "NEEDS_REVIEW"
		}
	}
	return q.UpdateMediaNodeGraphStatus(ctx, sqlcgen.UpdateMediaNodeGraphStatusParams{ID: nodeID, GraphStatus: status})
}
