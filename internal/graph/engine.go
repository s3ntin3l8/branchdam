package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// AutoAcceptThresholdForTier returns the confidence threshold for auto-accepting
// a candidate edge based on its tier: >=0.85 for Tier 3, >=0.90 for Tier 1 and 2.
// Exported so internal/agent can derive an agent-attached edge's review_state the
// same way every other resolver does, rather than accepting one from the payload.
func AutoAcceptThresholdForTier(tier int) float64 {
	if tier == 3 {
		return 0.85
	}
	return 0.90
}

const (
	// NeedsReviewFloor is the confidence below which a candidate edge is
	// discarded outright rather than queued for review. Exported for the
	// same reason as AutoAcceptThresholdForTier.
	NeedsReviewFloor = 0.50
)

// Engine runs every registered Resolver against a child node, merges their
// candidates, and commits survivors -- the only thing in this package that
// touches the database.
type Engine struct {
	resolvers []Resolver
	db        *db.DB
	lookup    Lookup
	log       *slog.Logger
}

// NewEngine builds an Engine. resolvers are tried in the order given;
// increment 1 registers XMPOriginalDocumentIDResolver and
// FilenameStemResolver (both Tier 2) -- Tier 1/3 resolvers are a Register
// call away later, not a schema change.
func NewEngine(database *db.DB, log *slog.Logger, resolvers ...Resolver) *Engine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{
		resolvers: resolvers,
		db:        database,
		lookup:    NewLookup(database.Reader),
		log:       log,
	}
}

// ResolveAndCommit runs every resolver against child, merges their
// candidates (max confidence per (parent, child, rel), evidence unioned),
// and commits survivors inside one write transaction. Each candidate is
// cycle-checked against the writer's single connection before insert --
// see docs/schema.md fix #7 -- and the upsert itself never downgrades a
// human CONFIRMED/REJECTED decision (media_edges_resolve.sql). A candidate
// matching an already CONFIRMED/REJECTED edge makes UpsertMediaEdge return
// sql.ErrNoRows (its WHERE-gated DO UPDATE emitted no row); that is not an
// error here -- the edge is re-fetched as-is and included in committed, so
// graph_status recomputation below still accounts for it and one
// human-locked edge can never abort resolution for the rest of the node's
// candidates. Returns the edges actually committed (empty if every
// candidate was below needsReviewFloor or would have closed a cycle) and
// how many of those were genuinely new rows, as opposed to an existing edge
// whose confidence/evidence was merely refreshed -- UpsertMediaEdge's
// RETURNING row looks the same either way, so the caller can't tell the two
// apart without this. Backs scan_jobs.edges_created.
func (e *Engine) ResolveAndCommit(ctx context.Context, child Node) ([]sqlcgen.MediaEdge, int, error) {
	var all []Candidate
	for _, r := range e.resolvers {
		candidates, err := r.Resolve(ctx, child, e.lookup)
		if err != nil {
			return nil, 0, fmt.Errorf("resolver %s: %w", r.Name(), err)
		}
		all = append(all, candidates...)
	}

	merged := mergeCandidates(all)

	var committed []sqlcgen.MediaEdge
	var created int
	err := e.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, c := range merged {
			if c.Confidence < NeedsReviewFloor {
				continue
			}

			wouldCycle, err := q.WouldCreateCycle(ctx, sqlcgen.WouldCreateCycleParams{
				ChildNodeID:  c.ChildID,
				ParentNodeID: c.ParentID,
			})
			if err != nil {
				return fmt.Errorf("cycle check %d->%d: %w", c.ParentID, c.ChildID, err)
			}
			if wouldCycle {
				e.log.Warn("graph: candidate edge would close a cycle, skipping",
					"parent", c.ParentID, "child", c.ChildID, "rel", c.Rel, "resolver", c.Resolver)
				continue
			}

			// Exact, not approximate: media_edges' UNIQUE (source_node_id,
			// target_node_id, relationship_type) constraint -- the same one
			// UpsertMediaEdge's ON CONFLICT target relies on -- guarantees at
			// most one row per key, so this check and the upsert can never
			// disagree about whether the row already existed. A future
			// schema change dropping that constraint would silently bias
			// this count without breaking either query.
			existed, err := q.MediaEdgeExists(ctx, sqlcgen.MediaEdgeExistsParams{
				SourceNodeID:     c.ParentID,
				TargetNodeID:     c.ChildID,
				RelationshipType: c.Rel,
			})
			if err != nil {
				return fmt.Errorf("check edge exists %d->%d: %w", c.ParentID, c.ChildID, err)
			}

			evidenceJSON, err := json.Marshal(c.Evidence)
			if err != nil {
				return fmt.Errorf("marshal evidence: %w", err)
			}

			reviewState := "NEEDS_REVIEW"
			if c.Confidence >= AutoAcceptThresholdForTier(c.Tier) {
				reviewState = "AUTO_ACCEPTED"
			}

			edge, err := q.UpsertMediaEdge(ctx, sqlcgen.UpsertMediaEdgeParams{
				SourceNodeID:     c.ParentID,
				TargetNodeID:     c.ChildID,
				RelationshipType: c.Rel,
				Confidence:       c.Confidence,
				Tier:             int64(c.Tier),
				Resolver:         c.Resolver,
				EvidenceJson:     string(evidenceJSON),
				ReviewState:      reviewState,
			})
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("upsert edge %d->%d: %w", c.ParentID, c.ChildID, err)
				}
				// Human-locked (CONFIRMED/REJECTED): the WHERE-gated DO
				// UPDATE emitted no row. Re-fetch the edge as it stands so
				// it's still counted toward graph_status below, and move on
				// to the next candidate instead of aborting the batch --
				// see ResolveAndCommit's doc comment and
				// media_edges_resolve.sql.
				locked, getErr := q.GetMediaEdgeBySourceTargetRel(ctx, sqlcgen.GetMediaEdgeBySourceTargetRelParams{
					SourceNodeID:     c.ParentID,
					TargetNodeID:     c.ChildID,
					RelationshipType: c.Rel,
				})
				if getErr != nil {
					return fmt.Errorf("get human-locked edge %d->%d: %w", c.ParentID, c.ChildID, getErr)
				}
				committed = append(committed, locked)
				continue
			}
			committed = append(committed, edge)
			if !existed {
				created++
			}
		}

		if len(committed) > 0 {
			// A REJECTED edge is a human decision that this specific
			// candidate is wrong -- it says nothing about whether the node
			// as a whole is linked, so it must not by itself push
			// graph_status to NEEDS_REVIEW (the pre-fix code could never
			// reach this branch with a REJECTED edge in committed at all,
			// since the old UpsertMediaEdge treated a human-locked edge as
			// a hard error and aborted before this point -- see H1). status
			// stays "" (skip the write) only when every committed edge is
			// REJECTED; any non-REJECTED edge sets it to at least
			// NEEDS_REVIEW, and any AUTO_ACCEPTED/CONFIRMED edge wins
			// outright regardless of order.
			status := ""
			for _, edge := range committed {
				if edge.ReviewState == "AUTO_ACCEPTED" || edge.ReviewState == "CONFIRMED" {
					status = "LINKED"
					break
				}
				if edge.ReviewState != "REJECTED" {
					status = "NEEDS_REVIEW"
				}
			}
			if status != "" {
				if err := q.UpdateMediaNodeGraphStatus(ctx, sqlcgen.UpdateMediaNodeGraphStatusParams{
					ID:          child.ID,
					GraphStatus: status,
				}); err != nil {
					return fmt.Errorf("update graph_status: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return committed, created, nil
}

// mergeCandidates groups candidates by (parent, child, rel), keeping the
// max confidence and unioning evidence under each contributing resolver's
// name -- so the audit UI can show every signal that fired, not just the
// winner.
func mergeCandidates(candidates []Candidate) []Candidate {
	type key struct {
		parent, child int64
		rel           string
	}
	merged := make(map[key]*Candidate)
	order := make([]key, 0, len(candidates))

	for _, c := range candidates {
		k := key{c.ParentID, c.ChildID, c.Rel}
		existing, ok := merged[k]
		if !ok {
			cp := c
			cp.Evidence = map[string]any{c.Resolver: c.Evidence}
			merged[k] = &cp
			order = append(order, k)
			continue
		}
		existing.Evidence[c.Resolver] = c.Evidence
		if c.Confidence > existing.Confidence {
			existing.Confidence = c.Confidence
			existing.Resolver = c.Resolver // the winning resolver's name is what's stored in the resolver column
			existing.Tier = c.Tier
		}
	}

	out := make([]Candidate, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out
}
