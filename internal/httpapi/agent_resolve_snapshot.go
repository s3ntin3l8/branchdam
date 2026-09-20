package httpapi

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
)

// ResolveSnapshot is a complete, successful database read. A missing member
// is authoritative only when this request commits; transport/query failures
// never deactivate server edges.
type ResolveSnapshotTimeline struct {
	TimelineID   string          `json:"timelineId"`
	NodeUUID     string          `json:"nodeUuid"`
	FilePath     string          `json:"filePath"`
	DisplayName  string          `json:"displayName"`
	EvidenceJSON json.RawMessage `json:"evidenceJson"`
}

type ResolveSnapshotMembership struct {
	TimelineID     string          `json:"timelineId"`
	MediaFilePath  string          `json:"mediaFilePath"`
	SourceNodeUUID string          `json:"sourceNodeUuid,omitempty"`
	EvidenceJSON   json.RawMessage `json:"evidenceJson,omitempty"`
}

type ResolveSnapshotInput struct {
	Body struct {
		AgentID                 string                      `json:"agentId" required:"true"`
		ScopeID                 string                      `json:"scopeId" required:"true"`
		Timelines               []ResolveSnapshotTimeline   `json:"timelines" required:"true"`
		Memberships             []ResolveSnapshotMembership `json:"memberships" required:"true"`
		LegacyTimelineNodeUUIDs []string                    `json:"legacyTimelineNodeUuids,omitempty"`
		RetireScopeID           string                      `json:"retireScopeId,omitempty"`
	}
}

type ResolveSnapshotOutput struct {
	Body ResolveSnapshotResult
}

type ResolveSnapshotResult struct {
	Created           int `json:"created"`
	Refreshed         int `json:"refreshed"`
	Removed           int `json:"removed"`
	Unchanged         int `json:"unchanged"`
	Unresolved        int `json:"unresolved"`
	ReviewedConflicts int `json:"reviewedConflicts"`
}

const maxResolveSnapshotMemberships = 10_000

// MaxResolveSnapshotBodyBytes overrides huma's default 1 MiB per-operation
// body cap (huma.Operation.ensureMaxBodyBytes), which a legitimate snapshot
// at the documented maxResolveSnapshotMemberships cap already exceeds -- the
// 1k/10k benchmarks this limit was raised for (issue #448) hit a 413 before
// this endpoint's own validation ever ran. Sized for 10,000 timelines plus
// 10,000 memberships with several KB of client evidence JSON each, well
// under the request still being rejected outright by something unbounded.
const MaxResolveSnapshotBodyBytes = 64 * 1024 * 1024

type resolveSnapshotConflict struct{ message string }

func (e resolveSnapshotConflict) Error() string { return e.message }

func validResolveScopeID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 32 && strings.ToLower(id) == id
}

func (s *Server) handleResolveSnapshot(ctx context.Context, in *ResolveSnapshotInput) (*ResolveSnapshotOutput, error) {
	p, ok := auth.From(ctx)
	if !ok || p.Kind != auth.KindMachine {
		return nil, huma.Error403Forbidden("agent machine principal required", nil)
	}
	b := in.Body
	// Cross-check body.agentId against the Principal -- paired devices
	// cannot submit a Resolve snapshot attributed to another device.
	// The env-bootstrap path is exempt (no per-device claim to
	// mismatch against; legacy shared-secret holder can drive any
	// agent_id); issue #453 PR C narrowed the cross-talk via per-
	// device HMAC signing. PR F will retire the carve-out entirely
	// when the env-var field is removed.
	if p.Name != "env-bootstrap" && b.AgentID != p.Name {
		return nil, huma.Error403Forbidden("agent id mismatch", nil)
	}
	if !validResolveScopeID(b.ScopeID) || (b.RetireScopeID != "" && !validResolveScopeID(b.RetireScopeID)) {
		return nil, huma.Error400BadRequest("scopeId must be a lowercase SHA-256 hex digest", nil)
	}
	if len(b.Memberships) > maxResolveSnapshotMemberships || len(b.Timelines) > maxResolveSnapshotMemberships || len(b.LegacyTimelineNodeUUIDs) > maxResolveSnapshotMemberships {
		return nil, huma.Error413RequestEntityTooLarge("Resolve snapshot exceeds 10000 entries; no edges changed", nil)
	}
	if s.guard == nil {
		return nil, huma.Error409Conflict("virtual storage guard is not configured", nil)
	}

	timelines := make(map[string]ResolveSnapshotTimeline, len(b.Timelines))
	timelineNodes := make(map[string]bool, len(b.Timelines))
	for _, tl := range b.Timelines {
		if tl.TimelineID == "" || tl.DisplayName == "" || tl.FilePath == "" || !json.Valid(tl.EvidenceJSON) {
			return nil, huma.Error400BadRequest("invalid Resolve timeline", nil)
		}
		timelineUUID, err := uuid.Parse(tl.NodeUUID)
		if err != nil {
			return nil, huma.Error400BadRequest("invalid Resolve timeline nodeUuid", err)
		}
		tl.NodeUUID = timelineUUID.String()
		if _, exists := timelines[tl.TimelineID]; exists {
			return nil, huma.Error400BadRequest("duplicate Resolve timelineId", nil)
		}
		if timelineNodes[tl.NodeUUID] {
			return nil, huma.Error400BadRequest("duplicate Resolve timeline nodeUuid", nil)
		}
		timelineNodes[tl.NodeUUID] = true
		loc, err := s.guard.Resolve(tl.FilePath)
		if err != nil || !loc.IsVirtual {
			return nil, huma.Error409Conflict("Resolve timeline path must resolve to virtual storage", err)
		}
		timelines[tl.TimelineID] = tl
	}
	protected := make(map[string]map[string]bool, len(timelines))
	desired := make(map[string]map[string]ResolveSnapshotMembership, len(timelines))
	resolvedSources := make(map[string]map[string]bool, len(timelines))
	seen := make(map[string]bool, len(b.Memberships))
	for _, m := range b.Memberships {
		if _, exists := timelines[m.TimelineID]; !exists || m.MediaFilePath == "" {
			return nil, huma.Error400BadRequest("membership references an unknown timeline or empty path", nil)
		}
		key := m.TimelineID + "\x00" + m.MediaFilePath
		if seen[key] {
			return nil, huma.Error400BadRequest("duplicate Resolve membership", nil)
		}
		seen[key] = true
		if m.SourceNodeUUID == "" {
			if protected[m.TimelineID] == nil {
				protected[m.TimelineID] = make(map[string]bool)
			}
			protected[m.TimelineID][m.MediaFilePath] = true
			continue
		}
		sourceUUID, err := uuid.Parse(m.SourceNodeUUID)
		if err != nil || !json.Valid(m.EvidenceJSON) {
			return nil, huma.Error400BadRequest("resolved membership needs UUID and JSON evidence", err)
		}
		m.SourceNodeUUID = sourceUUID.String()
		if resolvedSources[m.TimelineID] == nil {
			resolvedSources[m.TimelineID] = make(map[string]bool)
		}
		if resolvedSources[m.TimelineID][m.SourceNodeUUID] {
			return nil, huma.Error400BadRequest("duplicate Resolve source membership for timeline", nil)
		}
		resolvedSources[m.TimelineID][m.SourceNodeUUID] = true
		var ev struct {
			MediaFilePath string `json:"mediaFilePath"`
			TimelineID    string `json:"timelineId"`
		}
		if err := json.Unmarshal(m.EvidenceJSON, &ev); err != nil || ev.MediaFilePath != m.MediaFilePath || ev.TimelineID != m.TimelineID {
			return nil, huma.Error400BadRequest("Resolve evidence path/timeline does not match membership", err)
		}
		if desired[m.TimelineID] == nil {
			desired[m.TimelineID] = make(map[string]ResolveSnapshotMembership)
		}
		desired[m.TimelineID][m.MediaFilePath] = m
	}

	out := &ResolveSnapshotOutput{}
	out.Body.Unresolved = len(b.Memberships) - len(seenResolved(desired))
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		sourcesByUUID, err := batchResolveSourceNodes(ctx, q, desired)
		if err != nil {
			return err
		}
		targets := make(map[int64]string, len(timelines))
		for _, tl := range b.Timelines {
			node, err := q.GetMediaNodeByUUID(ctx, tl.NodeUUID)
			inserted := false
			if errors.Is(err, sql.ErrNoRows) {
				loc, resolveErr := s.guard.Resolve(tl.FilePath)
				if resolveErr != nil || !loc.IsVirtual {
					return resolveSnapshotConflict{message: "Resolve virtual storage location unavailable"}
				}
				node, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
					NodeUuid: tl.NodeUUID, StorageLocationID: loc.ID,
					FilePath: tl.FilePath, FileName: tl.DisplayName, FileExt: "",
					IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
				})
				inserted = err == nil
			}
			if err != nil {
				return fmt.Errorf("resolve timeline %s: %w", tl.NodeUUID, err)
			}
			if node.FilePath != tl.FilePath {
				return resolveSnapshotConflict{message: "Resolve node UUID belongs to a different virtual path"}
			}
			owned, err := q.ResolveTimelineScopeOwner(ctx, sqlcgen.ResolveTimelineScopeOwnerParams{TimelineNodeID: node.ID, AgentID: b.AgentID, ScopeID: b.ScopeID})
			if err != nil {
				return err
			}
			if !owned && !inserted {
				// A pre-snapshot virtual node must have been created by this
				// agent's processed virtual-node event before it can be claimed.
				created, checkErr := q.AgentCreatedVirtualNode(ctx, sqlcgen.AgentCreatedVirtualNodeParams{AgentID: b.AgentID, NodeUuid: tl.NodeUUID})
				if checkErr != nil {
					return checkErr
				}
				if !created {
					return resolveSnapshotConflict{message: "Resolve virtual node is not owned by this agent"}
				}
			}
			if err := q.RegisterResolveTimelineScope(ctx, sqlcgen.RegisterResolveTimelineScopeParams{AgentID: b.AgentID, ScopeID: b.ScopeID, TimelineNodeID: node.ID}); err != nil {
				return err
			}
			if err := q.UpdateResolveTimelineDisplayName(ctx, sqlcgen.UpdateResolveTimelineDisplayNameParams{ID: node.ID, FileName: tl.DisplayName}); err != nil {
				return err
			}
			// Keep synchronous snapshot evidence in a distinct slot from the
			// legacy async drainer so an already-queued virtual-node event cannot
			// race and overwrite the authoritative snapshot metadata.
			if err := q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{NodeID: node.ID, Source: "resolve_snapshot_evidence", Key: "evidence_json", Value: string(tl.EvidenceJSON)}); err != nil {
				return err
			}
			targets[node.ID] = tl.TimelineID
		}
		for _, nodeUUID := range b.LegacyTimelineNodeUUIDs {
			if _, err := uuid.Parse(nodeUUID); err != nil {
				return resolveSnapshotConflict{message: "invalid legacy timeline UUID"}
			}
			node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
			if err != nil {
				return err
			}
			if _, current := targets[node.ID]; current {
				continue
			}
			created, err := q.AgentCreatedVirtualNode(ctx, sqlcgen.AgentCreatedVirtualNodeParams{AgentID: b.AgentID, NodeUuid: nodeUUID})
			if err != nil {
				return err
			}
			if !created {
				return resolveSnapshotConflict{message: "legacy timeline node is not owned by this agent"}
			}
			if err := q.RegisterResolveTimelineScope(ctx, sqlcgen.RegisterResolveTimelineScopeParams{AgentID: b.AgentID, ScopeID: b.ScopeID, TimelineNodeID: node.ID}); err != nil {
				return err
			}
		}
		ids, err := q.ListResolveTimelineScopes(ctx, sqlcgen.ListResolveTimelineScopesParams{AgentID: b.AgentID, ScopeID: b.ScopeID})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := reconcileResolveTimeline(ctx, q, id, targets[id], desired[targets[id]], protected[targets[id]], sourcesByUUID, &out.Body); err != nil {
				return err
			}
		}
		if b.RetireScopeID != "" && b.RetireScopeID != b.ScopeID {
			oldIDs, listErr := q.ListResolveTimelineScopes(ctx, sqlcgen.ListResolveTimelineScopesParams{AgentID: b.AgentID, ScopeID: b.RetireScopeID})
			if listErr != nil {
				return listErr
			}
			for _, id := range oldIDs {
				if _, current := targets[id]; current {
					continue
				}
				if err := reconcileResolveTimeline(ctx, q, id, "", nil, nil, sourcesByUUID, &out.Body); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		var conflict resolveSnapshotConflict
		if errors.As(err, &conflict) {
			return nil, huma.Error409Conflict(conflict.message, nil)
		}
		return nil, huma.Error500InternalServerError("Resolve snapshot rolled back", err)
	}
	return out, nil
}

func seenResolved(desired map[string]map[string]ResolveSnapshotMembership) map[string]bool {
	out := make(map[string]bool)
	for tl, members := range desired {
		for path := range members {
			out[tl+"\x00"+path] = true
		}
	}
	return out
}

// batchResolveSourceNodes resolves every distinct SourceNodeUUID referenced
// anywhere in desired in a single query, instead of the one-row-per-membership
// round trip reconcileResolveTimeline used before (issue #448). Resolution is
// still exclusively by node_uuid: a membership's SourceNodeUUID is client
// input, so it is only ever used as an IN-list lookup key here, never as (or
// to derive) a trusted internal media_nodes.id.
func batchResolveSourceNodes(ctx context.Context, q *sqlcgen.Queries, desired map[string]map[string]ResolveSnapshotMembership) (map[string]sqlcgen.GetMediaNodesByUUIDsRow, error) {
	uuidSet := make(map[string]bool)
	for _, members := range desired {
		for _, m := range members {
			uuidSet[m.SourceNodeUUID] = true
		}
	}
	if len(uuidSet) == 0 {
		return nil, nil
	}
	uuids := make([]string, 0, len(uuidSet))
	for id := range uuidSet {
		uuids = append(uuids, id)
	}
	encoded, err := json.Marshal(uuids)
	if err != nil {
		return nil, err
	}
	rows, err := q.GetMediaNodesByUUIDs(ctx, string(encoded))
	if err != nil {
		return nil, err
	}
	byUUID := make(map[string]sqlcgen.GetMediaNodesByUUIDsRow, len(rows))
	for _, row := range rows {
		byUUID[row.NodeUuid] = row
	}
	return byUUID, nil
}

func reconcileResolveTimeline(ctx context.Context, q *sqlcgen.Queries, targetID int64, timelineID string, desired map[string]ResolveSnapshotMembership, protected map[string]bool, sources map[string]sqlcgen.GetMediaNodesByUUIDsRow, stats *ResolveSnapshotResult) error {
	existing, err := q.ListResolveEdgesForTimeline(ctx, targetID)
	if err != nil {
		return err
	}
	bySource := make(map[int64]sqlcgen.ListResolveEdgesForTimelineRow, len(existing))
	for _, edge := range existing {
		bySource[edge.SourceNodeID] = edge
	}
	keep := make(map[int64]bool, len(desired))
	// Lazily populated on the first new edge this timeline needs: the target
	// is fixed for this whole call, and a new edge only adds an INCOMING edge
	// to it, which cannot change what is reachable FORWARD from it -- so one
	// descendant walk covers every candidate parent checked below instead of
	// re-running the cycle CTE per candidate.
	var descendantsOfTarget map[int64]bool
	for _, m := range desired {
		src, ok := sources[m.SourceNodeUUID]
		if !ok {
			return resolveSnapshotConflict{message: "Resolve source node missing on server; no edges changed"}
		}
		if src.LifecycleState != "ACTIVE" && src.LifecycleState != "HIDDEN" {
			return resolveSnapshotConflict{message: "Resolve source node is not live; no edges changed"}
		}
		keep[src.ID] = true
		evidenceJSON := string(m.EvidenceJSON)
		if edge, found := bySource[src.ID]; found {
			if edge.Resolver != "resolve_project_db" {
				return resolveSnapshotConflict{message: "matching edge is owned by another resolver"}
			}
			if edge.ReviewState == "CONFIRMED" || edge.ReviewState == "REJECTED" {
				if edge.EvidenceJson != evidenceJSON || edge.IsActive == 0 {
					stats.ReviewedConflicts++
				}
				continue
			}
			if edge.EvidenceJson == evidenceJSON && edge.IsActive == 1 {
				stats.Unchanged++
				continue
			}
			if err := q.RefreshResolveEdge(ctx, sqlcgen.RefreshResolveEdgeParams{ID: edge.ID, EvidenceJson: evidenceJSON}); err != nil {
				return err
			}
			stats.Refreshed++
			continue
		}
		if descendantsOfTarget == nil {
			descendantIDs, cycleErr := q.DescendantNodeIDs(ctx, targetID)
			if cycleErr != nil {
				return cycleErr
			}
			descendantsOfTarget = make(map[int64]bool, len(descendantIDs))
			for _, id := range descendantIDs {
				descendantsOfTarget[id] = true
			}
		}
		if descendantsOfTarget[src.ID] {
			return resolveSnapshotConflict{message: "Resolve edge would create a lineage cycle"}
		}
		if _, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: src.ID, TargetNodeID: targetID, RelationshipType: "PROJECT_SIDECAR",
			Confidence: 1, Tier: 1, Resolver: "resolve_project_db", EvidenceJson: evidenceJSON,
			ReviewState: "AUTO_ACCEPTED",
		}); err != nil {
			return err
		}
		stats.Created++
	}
	for _, edge := range existing {
		if edge.IsActive == 0 || edge.Resolver != "resolve_project_db" || keep[edge.SourceNodeID] {
			continue
		}
		var ev struct {
			MediaFilePath  string   `json:"mediaFilePath"`
			MediaFilePaths []string `json:"mediaFilePaths"`
		}
		if json.Unmarshal([]byte(edge.EvidenceJson), &ev) != nil {
			return resolveSnapshotConflict{message: "existing Resolve edge has malformed evidence; refusing to remove it"}
		}
		protectedByAlias := protected[ev.MediaFilePath]
		if !protectedByAlias {
			for _, alias := range ev.MediaFilePaths {
				if protected[alias] {
					protectedByAlias = true
					break
				}
			}
		}
		if timelineID != "" && protectedByAlias {
			continue
		}
		if edge.ReviewState == "CONFIRMED" || edge.ReviewState == "REJECTED" {
			stats.ReviewedConflicts++
			continue
		}
		if err := q.InactivateResolveEdge(ctx, edge.ID); err != nil {
			return err
		}
		stats.Removed++
	}
	return graph.RecomputeStatusFromPersistedEdges(ctx, q, targetID)
}
