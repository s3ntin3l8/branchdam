package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// autoAcceptThreshold and needsReviewFloor are the build plan's confidence
// thresholds: >=0.90 is auto-accepted, [0.50, 0.90) lands in the audit
// queue (review_state=NEEDS_REVIEW), and anything below 0.50 is dropped --
// never even written, per the build plan ("< 0.50 dropped, never
// persisted").
const (
	autoAcceptThreshold = 0.90
	needsReviewFloor    = 0.50
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
// human CONFIRMED/REJECTED decision (media_edges_resolve.sql). Returns the
// edges actually committed (empty if every candidate was below
// needsReviewFloor or would have closed a cycle).
func (e *Engine) ResolveAndCommit(ctx context.Context, child Node) ([]sqlcgen.MediaEdge, error) {
	var all []Candidate
	for _, r := range e.resolvers {
		candidates, err := r.Resolve(ctx, child, e.lookup)
		if err != nil {
			return nil, fmt.Errorf("resolver %s: %w", r.Name(), err)
		}
		all = append(all, candidates...)
	}

	merged := mergeCandidates(all)

	var committed []sqlcgen.MediaEdge
	err := e.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, c := range merged {
			if c.Confidence < needsReviewFloor {
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

			evidenceJSON, err := json.Marshal(c.Evidence)
			if err != nil {
				return fmt.Errorf("marshal evidence: %w", err)
			}

			reviewState := "NEEDS_REVIEW"
			if c.Confidence >= autoAcceptThreshold {
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
				return fmt.Errorf("upsert edge %d->%d: %w", c.ParentID, c.ChildID, err)
			}
			committed = append(committed, edge)
		}

		if len(committed) > 0 {
			status := "NEEDS_REVIEW"
			for _, edge := range committed {
				if edge.ReviewState == "AUTO_ACCEPTED" || edge.ReviewState == "CONFIRMED" {
					status = "LINKED"
					break
				}
			}
			if err := q.UpdateMediaNodeGraphStatus(ctx, sqlcgen.UpdateMediaNodeGraphStatusParams{
				ID:          child.ID,
				GraphStatus: status,
			}); err != nil {
				return fmt.Errorf("update graph_status: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
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
		}
	}

	out := make([]Candidate, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out
}
