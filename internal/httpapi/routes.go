package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/metadata"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/projectfile"
	"github.com/s3ntin3l8/branchdam/internal/prune"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/sync"
)

func (s *Server) registerRoutes(api huma.API) {
	huma.Get(api, "/api/v1/me", s.handleMe)
	huma.Get(api, "/api/v1/config", s.handleConfig)
	huma.Get(api, "/api/v1/config/path-rewrites", s.handleListPathRewrites)

	huma.Get(api, "/api/v1/storage-locations", s.handleListStorageLocations)
	huma.Get(api, "/api/v1/storage-health", s.handleStorageHealth)
	huma.Post(api, "/api/v1/prune", s.handlePrune)

	huma.Get(api, "/api/v1/assets", s.handleListAssets)
	huma.Get(api, "/api/v1/assets/facets", s.handleListAssetFacets)
	huma.Get(api, "/api/v1/assets/{id}", s.handleGetAsset)
	huma.Get(api, "/api/v1/assets/{id}/graph", s.handleAssetGraph)
	huma.Get(api, "/api/v1/assets/{id}/lineage", s.handleAssetLineage)
	huma.Get(api, "/api/v1/assets/{id}/sync-status", s.handleAssetSyncStatus)
	huma.Post(api, "/api/v1/assets/{id}/sync/retry", s.handleSyncRetry)
	huma.Post(api, "/api/v1/assets/{id}/inherit-metadata", s.handleInheritMetadata)

	huma.Get(api, "/api/v1/jobs", s.handleListJobs)

	huma.Post(api, "/api/v1/edges", s.handleCreateEdge)
	huma.Get(api, "/api/v1/edges/audit", s.handleAuditQueue)
	huma.Post(api, "/api/v1/edges/{id}/confirm", s.handleConfirmEdge)
	huma.Post(api, "/api/v1/edges/{id}/reject", s.handleRejectEdge)

	huma.Register(api, huma.Operation{
		Method:        http.MethodPost,
		Path:          "/api/v1/scan",
		OperationID:   "startScan",
		Summary:       "Start a scan of a storage location",
		DefaultStatus: http.StatusAccepted,
	}, s.handleStartScan)
	huma.Get(api, "/api/v1/progress", s.handleProgress)

	huma.Post(api, "/api/v1/agent/hello", s.handleAgentHello)
	huma.Register(api, huma.Operation{
		Method:        http.MethodPost,
		Path:          "/api/v1/agent/handshake",
		OperationID:   "agentHandshake",
		Summary:       "Execute agent synchronization handshake",
		DefaultStatus: http.StatusOK,
	}, s.handleAgentHandshake)
	huma.Register(api, huma.Operation{
		Method:        http.MethodPost,
		Path:          "/api/v1/agent/events",
		OperationID:   "submitAgentEvent",
		Summary:       "Accept a workstation agent event",
		DefaultStatus: http.StatusAccepted,
	}, s.handleAgentEvent)
	huma.Register(api, huma.Operation{
		Method:        http.MethodPost,
		Path:          "/api/v1/agent/rebase",
		OperationID:   "agentRebasePath",
		Summary:       "Rebase staged media node path",
		DefaultStatus: http.StatusOK,
	}, s.handleAgentRebase)
}

// --- /api/v1/me ---

type MeOutput struct {
	Body struct {
		Kind   string   `json:"kind"`
		Name   string   `json:"name,omitempty"`
		Email  string   `json:"email,omitempty"`
		Groups []string `json:"groups,omitempty"`
	}
}

func (s *Server) handleMe(ctx context.Context, _ *struct{}) (*MeOutput, error) {
	out := &MeOutput{}
	if p, ok := auth.From(ctx); ok {
		out.Body.Kind = string(p.Kind)
		out.Body.Name = p.Name
		out.Body.Email = p.Email
		out.Body.Groups = p.Groups
	}
	return out, nil
}

// --- /api/v1/config ---

type PathRewriteDTO struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ConfigOutput struct {
	Body struct {
		Version      string           `json:"version"`
		PathRewrites []PathRewriteDTO `json:"pathRewrites"`
	}
}

func (s *Server) handleConfig(_ context.Context, _ *struct{}) (*ConfigOutput, error) {
	out := &ConfigOutput{}
	out.Body.Version = s.version
	out.Body.PathRewrites = make([]PathRewriteDTO, 0)
	if s.cfg != nil {
		for _, rw := range s.cfg.PathRewrites {
			out.Body.PathRewrites = append(out.Body.PathRewrites, PathRewriteDTO{
				From: rw.From,
				To:   rw.To,
			})
		}
	}
	return out, nil
}

type PathRewritesOutput struct {
	Body []PathRewriteDTO `json:"body"`
}

func (s *Server) handleListPathRewrites(_ context.Context, _ *struct{}) (*PathRewritesOutput, error) {
	out := &PathRewritesOutput{
		Body: make([]PathRewriteDTO, 0),
	}
	if s.cfg != nil {
		for _, rw := range s.cfg.PathRewrites {
			out.Body = append(out.Body, PathRewriteDTO{
				From: rw.From,
				To:   rw.To,
			})
		}
	}
	return out, nil
}

// --- /api/v1/storage-locations ---

type storageLocationDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	RootPath string `json:"rootPath"`
	Tier     string `json:"tier"`
	ReadOnly bool   `json:"readOnly"`
	Prunable bool   `json:"prunable"`
}

type ListStorageLocationsOutput struct {
	Body struct {
		Locations []storageLocationDTO `json:"locations"`
	}
}

func (s *Server) handleListStorageLocations(ctx context.Context, _ *struct{}) (*ListStorageLocationsOutput, error) {
	rows, err := s.db.Reader.ListStorageLocations(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("list storage locations", err)
	}
	out := &ListStorageLocationsOutput{}
	out.Body.Locations = make([]storageLocationDTO, len(rows))
	for i, r := range rows {
		out.Body.Locations[i] = storageLocationDTO{
			ID: r.ID, Name: r.Name, RootPath: r.RootPath, Tier: r.Tier,
			ReadOnly: r.ReadOnly != 0, Prunable: r.Prunable != 0,
		}
	}
	return out, nil
}

// --- /api/v1/assets ---

type assetDTO struct {
	ID                 int64   `json:"id"`
	NodeUUID           string  `json:"nodeUuid"`
	FilePath           string  `json:"filePath"`
	FileName           string  `json:"fileName"`
	FileExt            string  `json:"fileExt"`
	SizeBytes          int64   `json:"sizeBytes"`
	FastHash           *string `json:"fastHash,omitempty"`
	FullHash           *string `json:"fullHash,omitempty"`
	IndexingStatus     string  `json:"indexingStatus"`
	GraphStatus        string  `json:"graphStatus"`
	LifecycleState     string  `json:"lifecycleState"`
	StorageLocationID  int64   `json:"storageLocationId"`
	OriginalDocumentID string  `json:"originalDocumentId,omitempty"`
	CameraModel        string  `json:"cameraModel,omitempty"`
}

func toAssetDTO(n sqlcgen.MediaNode) assetDTO {
	dto := assetDTO{
		ID:                n.ID,
		NodeUUID:          n.NodeUuid,
		FilePath:          n.FilePath,
		FileName:          n.FileName,
		FileExt:           n.FileExt,
		SizeBytes:         n.SizeBytes,
		FastHash:          n.FastHash,
		FullHash:          n.FullHash,
		IndexingStatus:    n.IndexingStatus,
		GraphStatus:       n.GraphStatus,
		LifecycleState:    n.LifecycleState,
		StorageLocationID: n.StorageLocationID,
	}
	if n.OriginalDocumentID.Valid {
		dto.OriginalDocumentID = n.OriginalDocumentID.String
	}
	if n.CameraModel.Valid {
		dto.CameraModel = n.CameraModel.String
	}
	return dto
}

type ListAssetsInput struct {
	Limit             int64  `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Offset            int64  `query:"offset" default:"0" minimum:"0"`
	CameraModel       string `query:"cameraModel"`
	GraphStatus       string `query:"graphStatus"`
	StorageLocationID int64  `query:"storageLocationId"`
	LifecycleState    string `query:"lifecycleState"`
	UnlinkedOnly      bool   `query:"unlinkedOnly"`
}

type ListAssetsOutput struct {
	Body struct {
		Assets []assetDTO `json:"assets"`
		Total  int64      `json:"total"`
	}
}

func (s *Server) handleListAssets(ctx context.Context, in *ListAssetsInput) (*ListAssetsOutput, error) {
	var lifecycleState sql.NullString
	if in.LifecycleState != "" {
		lifecycleState = sql.NullString{String: in.LifecycleState, Valid: true}
	}

	var graphStatus sql.NullString
	if in.UnlinkedOnly {
		graphStatus = sql.NullString{String: "UNLINKED", Valid: true}
	} else if in.GraphStatus != "" {
		graphStatus = sql.NullString{String: in.GraphStatus, Valid: true}
	}

	var cameraModel sql.NullString
	if in.CameraModel != "" {
		cameraModel = sql.NullString{String: in.CameraModel, Valid: true}
	}

	var storageLocationID sql.NullInt64
	if in.StorageLocationID > 0 {
		storageLocationID = sql.NullInt64{Int64: in.StorageLocationID, Valid: true}
	}

	hasFilters := lifecycleState.Valid || graphStatus.Valid || cameraModel.Valid || storageLocationID.Valid

	var rows []sqlcgen.MediaNode
	var total int64
	var err error

	if !hasFilters {
		rows, err = s.db.Reader.ListMediaNodes(ctx, sqlcgen.ListMediaNodesParams{Limit: in.Limit, Offset: in.Offset})
		if err != nil {
			return nil, huma.Error500InternalServerError("list assets", err)
		}
		total, err = s.db.Reader.CountMediaNodesFiltered(ctx, sqlcgen.CountMediaNodesFilteredParams{})
		if err != nil {
			total = int64(len(rows))
		}
	} else {
		params := sqlcgen.ListMediaNodesFilteredParams{
			Limit:             in.Limit,
			Offset:            in.Offset,
			LifecycleState:    lifecycleState,
			CameraModel:       cameraModel,
			GraphStatus:       graphStatus,
			StorageLocationID: storageLocationID,
		}
		rows, err = s.db.Reader.ListMediaNodesFiltered(ctx, params)
		if err != nil {
			return nil, huma.Error500InternalServerError("list filtered assets", err)
		}
		total, err = s.db.Reader.CountMediaNodesFiltered(ctx, sqlcgen.CountMediaNodesFilteredParams{
			LifecycleState:    lifecycleState,
			CameraModel:       cameraModel,
			GraphStatus:       graphStatus,
			StorageLocationID: storageLocationID,
		})
		if err != nil {
			total = int64(len(rows))
		}
	}

	out := &ListAssetsOutput{}
	out.Body.Assets = make([]assetDTO, len(rows))
	for i, r := range rows {
		out.Body.Assets[i] = toAssetDTO(r)
	}
	out.Body.Total = total
	return out, nil
}

// --- /api/v1/assets/facets ---

type AssetFacetsOutput struct {
	Body struct {
		CameraModels []string `json:"cameraModels"`
	}
}

func (s *Server) handleListAssetFacets(ctx context.Context, _ *struct{}) (*AssetFacetsOutput, error) {
	models, err := s.db.Reader.ListCameraModelFacets(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("list camera model facets", err)
	}
	out := &AssetFacetsOutput{}
	out.Body.CameraModels = models
	return out, nil
}

// --- /api/v1/assets/{id} ---

type AssetPathInput struct {
	ID int64 `path:"id"`
}

type GetAssetOutput struct {
	Body assetDTO
}

func (s *Server) handleGetAsset(ctx context.Context, in *AssetPathInput) (*GetAssetOutput, error) {
	node, err := s.db.Reader.GetMediaNodeByID(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get asset", err)
	}
	return &GetAssetOutput{Body: toAssetDTO(node)}, nil
}

// --- /api/v1/assets/{id}/graph ---

type edgeDTO struct {
	ID               int64   `json:"id"`
	SourceNodeID     int64   `json:"sourceNodeId"`
	TargetNodeID     int64   `json:"targetNodeId"`
	RelationshipType string  `json:"relationshipType"`
	Confidence       float64 `json:"confidence"`
	ReviewState      string  `json:"reviewState"`
	Resolver         string  `json:"resolver"`
}

func toEdgeDTO(e sqlcgen.MediaEdge) edgeDTO {
	return edgeDTO{
		ID: e.ID, SourceNodeID: e.SourceNodeID, TargetNodeID: e.TargetNodeID,
		RelationshipType: e.RelationshipType, Confidence: e.Confidence,
		ReviewState: e.ReviewState, Resolver: e.Resolver,
	}
}

type AssetGraphOutput struct {
	Body struct {
		// One hop only in increment 1: direct parents (edges where this
		// node is the target) and direct children (edges where it's the
		// source). Deeper bounded traversal is a follow-up -- the SPA's
		// graph canvas can walk further by re-querying per node it renders.
		Parents  []edgeDTO `json:"parents"`
		Children []edgeDTO `json:"children"`
	}
}

func (s *Server) handleAssetGraph(ctx context.Context, in *AssetPathInput) (*AssetGraphOutput, error) {
	parents, err := s.db.Reader.ListEdgesByTarget(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list parent edges", err)
	}
	children, err := s.db.Reader.ListEdgesBySource(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list child edges", err)
	}
	out := &AssetGraphOutput{}
	out.Body.Parents = make([]edgeDTO, len(parents))
	for i, e := range parents {
		out.Body.Parents[i] = toEdgeDTO(e)
	}
	out.Body.Children = make([]edgeDTO, len(children))
	for i, e := range children {
		out.Body.Children[i] = toEdgeDTO(e)
	}
	return out, nil
}

// --- /api/v1/assets/{id}/lineage ---

// Deprecated: handleAssetGraph returns direct parents/children only (one hop).
// Use /api/v1/assets/{id}/lineage for bounded multi-hop lineage traversal.

type AssetLineageInput struct {
	ID    int64 `path:"id" doc:"Asset node ID"`
	Depth int   `query:"depth" default:"2" minimum:"1" maximum:"5" doc:"Lineage traversal depth (1-5)"`
}

type AssetLineageOutput struct {
	Body struct {
		RootID int64      `json:"rootId"`
		Nodes  []assetDTO `json:"nodes"`
		Edges  []edgeDTO  `json:"edges"`
	}
}

func (s *Server) handleAssetLineage(ctx context.Context, in *AssetLineageInput) (*AssetLineageOutput, error) {
	node, err := s.db.Reader.GetMediaNodeByID(ctx, in.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("asset not found")
		}
		return nil, huma.Error500InternalServerError("get root asset", err)
	}
	if node.LifecycleState == "ARCHIVED" {
		return nil, huma.Error404NotFound("asset not found")
	}

	depth := in.Depth
	if depth < 1 {
		depth = 2
	} else if depth > 5 {
		depth = 5
	}

	nodeIDs := make(map[int64]bool)
	nodeIDs[in.ID] = true

	currentLevel := []int64{in.ID}

	for d := 0; d < depth; d++ {
		if len(currentLevel) == 0 {
			break
		}
		var nextLevel []int64
		for _, nodeID := range currentLevel {
			parents, err := s.db.Reader.ListEdgesByTarget(ctx, nodeID)
			if err != nil {
				return nil, huma.Error500InternalServerError("list parent edges", err)
			}
			for _, p := range parents {
				if p.ReviewState == "REJECTED" {
					continue
				}
				if !nodeIDs[p.SourceNodeID] {
					nodeIDs[p.SourceNodeID] = true
					nextLevel = append(nextLevel, p.SourceNodeID)
				}
			}
			children, err := s.db.Reader.ListEdgesBySource(ctx, nodeID)
			if err != nil {
				return nil, huma.Error500InternalServerError("list child edges", err)
			}
			for _, c := range children {
				if c.ReviewState == "REJECTED" {
					continue
				}
				if !nodeIDs[c.TargetNodeID] {
					nodeIDs[c.TargetNodeID] = true
					nextLevel = append(nextLevel, c.TargetNodeID)
				}
			}
		}
		currentLevel = nextLevel
	}

	var idList []int64
	for id := range nodeIDs {
		idList = append(idList, id)
	}

	jsonBytes, err := json.Marshal(idList)
	if err != nil {
		return nil, huma.Error500InternalServerError("marshal node ids", err)
	}

	nodes, err := s.db.Reader.ListNodesByIDs(ctx, string(jsonBytes))
	if err != nil {
		return nil, huma.Error500InternalServerError("list lineage nodes", err)
	}

	validNodes := make(map[int64]bool, len(nodes))
	outNodes := make([]assetDTO, 0, len(nodes))
	for _, n := range nodes {
		if n.LifecycleState == "ARCHIVED" {
			continue
		}
		validNodes[n.ID] = true
		outNodes = append(outNodes, toAssetDTO(n))
	}

	edges, err := s.db.Reader.ListEdgesForNodes(ctx, string(jsonBytes))
	if err != nil {
		return nil, huma.Error500InternalServerError("list lineage edges", err)
	}

	outEdges := make([]edgeDTO, 0, len(edges))
	for _, e := range edges {
		if !validNodes[e.SourceNodeID] || !validNodes[e.TargetNodeID] {
			continue
		}
		outEdges = append(outEdges, edgeDTO{
			ID:               e.ID,
			SourceNodeID:     e.SourceNodeID,
			TargetNodeID:     e.TargetNodeID,
			RelationshipType: e.RelationshipType,
			Confidence:       e.Confidence,
			ReviewState:      e.ReviewState,
			Resolver:         e.Resolver,
		})
	}

	out := &AssetLineageOutput{}
	out.Body.RootID = in.ID
	out.Body.Nodes = outNodes
	out.Body.Edges = outEdges

	return out, nil
}

// --- /api/v1/assets/{id}/sync-status ---

type syncStateDTO struct {
	Remote        string  `json:"remote"`
	SyncStatus    string  `json:"syncStatus"`
	RemoteAssetID *string `json:"remoteAssetId,omitempty"`
	LastError     *string `json:"lastError,omitempty"`
	LastAttemptAt *int64  `json:"lastAttemptAt,omitempty"`
	RetryCount    int64   `json:"retryCount"`
	// Exhausted is true once a PUSH_FAILED row's retry_count has reached
	// sync.DefaultMaxSyncRetries -- ResetRemoteSyncStateFailed's automatic
	// worker recovery (#182) will no longer re-claim it, so the row is
	// permanently abandoned short of an explicit operator retry via
	// POST /api/v1/assets/{id}/sync/retry.
	//
	// This is always measured against the package default, not a given
	// Worker's effective w.maxRetries -- SetMaxRetries has no production
	// caller today (no config wires a non-default bound to any Worker), so
	// the two can't currently diverge. If that ever changes, this flag needs
	// the effective per-remote bound plumbed in instead of assuming the
	// default; RetryCount is exposed raw precisely so a caller can already
	// compare it against any bound without waiting on that.
	Exhausted bool `json:"exhausted"`
}

type AssetSyncStatusInput struct {
	ID int64 `path:"id"`
}

type AssetSyncStatusOutput struct {
	Body struct {
		Sync []syncStateDTO `json:"sync"`
	}
}

func (s *Server) handleAssetSyncStatus(ctx context.Context, in *AssetSyncStatusInput) (*AssetSyncStatusOutput, error) {
	node, err := s.db.Reader.GetMediaNodeByID(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get asset", err)
	}
	// Matches handleSyncRetry and handleInheritMetadata, both of which 404 an
	// ARCHIVED node -- these three handlers sit adjacent and previously
	// disagreed on this for no reason; an ARCHIVED node's sync history isn't
	// meaningful to surface as if it were live.
	if node.LifecycleState == "ARCHIVED" {
		return nil, huma.Error404NotFound("asset not found")
	}
	rows, err := s.db.Reader.ListRemoteSyncStateByNode(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list sync state", err)
	}
	out := &AssetSyncStatusOutput{}
	out.Body.Sync = make([]syncStateDTO, len(rows))
	for i, r := range rows {
		dto := syncStateDTO{Remote: r.Remote, SyncStatus: r.SyncStatus, RetryCount: r.RetryCount}
		if r.RemoteAssetID.Valid {
			dto.RemoteAssetID = &r.RemoteAssetID.String
		}
		if r.LastError.Valid {
			dto.LastError = &r.LastError.String
		}
		if r.LastAttemptAt.Valid {
			dto.LastAttemptAt = &r.LastAttemptAt.Int64
		}
		dto.Exhausted = r.SyncStatus == "PUSH_FAILED" && r.RetryCount >= sync.DefaultMaxSyncRetries
		out.Body.Sync[i] = dto
	}
	return out, nil
}

// --- /api/v1/assets/{id}/sync/retry ---

type SyncRetryInput struct {
	ID int64 `path:"id"`
}

type SyncRetryOutput struct {
	Body struct {
		Requeued int64 `json:"requeued"`
	}
}

func (s *Server) handleSyncRetry(ctx context.Context, in *SyncRetryInput) (*SyncRetryOutput, error) {
	node, err := s.db.Reader.GetMediaNodeByID(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get asset", err)
	}
	if node.LifecycleState == "ARCHIVED" {
		return nil, huma.Error404NotFound("asset not found")
	}

	var requeued int64
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		// Read + re-claim inside ONE write transaction on the writer pool's
		// single connection. A concurrent ProcessPending that claims the same
		// row (PUSH_FAILED -> PUSHING) cannot interleave between the read and
		// the upsert and be regressed back to PENDING_CLOUD_PUSH (which would
		// duplicate the push) -- the writer connection serializes the two.
		rows, err := q.ListRemoteSyncStateByNode(ctx, in.ID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.SyncStatus != "PUSH_FAILED" {
				continue
			}
			// ManualRetryRemoteSyncState, not UpsertRemoteSyncState: this also
			// resets retry_count to 0, so the operator's explicit retry restores
			// a full automatic-retry budget rather than buying exactly one more
			// attempt before immediately re-hitting ResetRemoteSyncStateFailed's
			// bound on the very next failure (#182).
			if err := q.ManualRetryRemoteSyncState(ctx, sqlcgen.ManualRetryRemoteSyncStateParams{
				NodeID: r.NodeID, Remote: r.Remote,
			}); err != nil {
				return err
			}
			requeued++
		}
		return nil
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("requeue failed sync", err)
	}
	out := &SyncRetryOutput{}
	out.Body.Requeued = requeued
	return out, nil
}

// --- /api/v1/assets/{id}/inherit-metadata ---

type InheritMetadataInput struct {
	ID int64 `path:"id"`
}

type InheritMetadataOutput struct {
	Body struct {
		Inherited map[string]string `json:"inherited"`
	}
}

// handleInheritMetadata copies identity metadata (EXIF/XMP, spec Pillar 4)
// from a node's winning parent edge into the child's file on disk, on demand.
// This is the project's first real filesystem writer: storage.Guard.CheckWrite
// is called BEFORE any exiftool subprocess, and a read-only location (Tier 3
// is always read-only) or a Tier-3-resolved parent edge is refused with 409.
func (s *Server) handleInheritMetadata(ctx context.Context, in *InheritMetadataInput) (*InheritMetadataOutput, error) {
	child, err := s.db.Reader.GetMediaNodeByID(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get asset", err)
	}
	if child.LifecycleState == "ARCHIVED" {
		return nil, huma.Error404NotFound("asset not found")
	}

	// #199: the child of an inherit-metadata write must be a real media file,
	// never a project archive (.drp/.fcpxml/.edl/.dam.json/.prproj). A
	// manual edge created before handleCreateEdge's guard shipped, or a
	// filename-stem/XMP match that lands a project file as the child of a
	// DERIVED_FROM/FINAL_EXPORT/PROXY_OF edge, would otherwise clear
	// pickWinningParent and run exiftool in-place against a container file
	// that isn't an image or video. Refuse before any exiftool subprocess
	// spawns.
	if _, ok := projectfile.GetParser(child.FilePath); ok {
		return nil, huma.Error409Conflict("cannot inherit metadata into a project file")
	}

	// Refuse a write into a read-only location BEFORE spawning exiftool.
	if s.guard != nil {
		if err := s.guard.CheckWrite(child.FilePath); err != nil {
			var roErr *storage.ErrReadOnlyTier
			if errors.As(err, &roErr) {
				return nil, huma.Error409Conflict("cannot inherit metadata into a read-only location (tier " + roErr.Tier + ")")
			}
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
	}

	parents, err := s.db.Reader.ListEdgesByTarget(ctx, child.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list parent edges", err)
	}
	winning := pickWinningParent(parents)
	if winning == nil {
		if hasResolvedButIneligibleParent(parents) {
			return nil, huma.Error409Conflict("cannot inherit from a Tier-3-resolved or non-ancestry (duplicate/sidecar) parent edge")
		}
		return nil, huma.Error409Conflict("asset has no resolved parent edge to inherit from")
	}

	parent, err := s.db.Reader.GetMediaNodeByID(ctx, winning.SourceNodeID)
	if err != nil {
		return nil, huma.Error500InternalServerError("get parent asset", err)
	}
	// A parent that has since been archived or gone missing is not a usable
	// identity source, even though the edge pointing at it may still be
	// AUTO_ACCEPTED/CONFIRMED -- archiving never touches media_edges.
	if parent.LifecycleState == "ARCHIVED" || parent.LifecycleState == "MISSING" {
		return nil, huma.Error409Conflict("parent asset is " + parent.LifecycleState + ", not a usable identity source")
	}

	parentTags, err := loadTagSet(ctx, s.db.Reader, parent)
	if err != nil {
		return nil, huma.Error500InternalServerError("read parent metadata", err)
	}
	childTags, err := loadTagSet(ctx, s.db.Reader, child)
	if err != nil {
		return nil, huma.Error500InternalServerError("read child metadata", err)
	}
	childTags.DerivedFrom = parent.NodeUuid // XMP-xmpMM:DerivedFrom = parent node_uuid

	tags := metadata.Plan(parentTags, childTags)
	// Plan always emits the two identity tags (XMP-dc:Identifier /
	// XMP-xmpMM:DerivedFrom), so this write is idempotent in value: repeating
	// it leaves the file's inheritable metadata unchanged and re-asserts the
	// child's identity tags.

	wctx, cancel := context.WithTimeout(ctx, inheritWriteTimeout)
	defer cancel()
	if err := s.prober.WriteTags(wctx, child.FilePath, tags); err != nil {
		if errors.Is(err, probe.ErrToolUnavailable) {
			return nil, huma.Error503ServiceUnavailable("exiftool not available")
		}
		return nil, huma.Error500InternalServerError("write metadata", err)
	}

	// The write just changed size_bytes/mtime_unix/fast_hash on disk. Unlike
	// the node_metadata backfill below, this update is NOT best-effort: if it
	// doesn't happen, the next scan's commitOne sees a changed fast_hash at
	// this path and treats it as a version collision -- archiving this node
	// and inserting a successor under a new node_uuid, which strands every
	// media_edges row (including a human CONFIRMED/REJECTED review decision)
	// on the now-archived row. A failure here is therefore a hard error, not
	// a warning, even though the file write itself already succeeded.
	if err := s.refreshNodeAfterInPlaceWrite(ctx, child); err != nil {
		return nil, huma.Error500InternalServerError("refresh node after metadata write", err)
	}

	// The file on disk now carries the inherited tags; backfill the child's
	// node_metadata so the DB reflects it. Best-effort -- the write already
	// succeeded, so a re-read/backfill failure is surfaced as a warning, not
	// an error response. Without this, node_metadata stays stale (empty)
	// until the next scan, and a second inherit call would re-plan from
	// stale values instead of seeing the child's own tags (#157).
	// Bounded with its own inheritWriteTimeout: a hung exiftool re-read on a
	// stalled mount must not hang the request after the write already
	// succeeded. Derived from context.WithoutCancel(ctx), not ctx directly:
	// the write itself already committed by this point, so a client
	// disconnecting now must not cancel a backfill that exists specifically
	// to keep the DB in sync with a change that already happened -- the
	// disconnect is exactly the case most likely to leave node_metadata
	// stale, which is the failure this backfill exists to avoid.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), inheritWriteTimeout)
	defer bcancel()
	if exif, err := s.prober.Exif(bctx, child.FilePath); err != nil {
		s.log.Warn("inherit-metadata: re-read child for backfill failed", "nodeID", child.ID, "err", err)
	} else if err := pipeline.PersistExifMetadata(bctx, s.db, child.ID, exif, s.log); err != nil {
		s.log.Warn("inherit-metadata: backfill node_metadata failed", "nodeID", child.ID, "err", err)
	}

	out := &InheritMetadataOutput{}
	out.Body.Inherited = tags
	return out, nil
}

// inheritWriteTimeout bounds the exiftool write subprocess, mirroring the
// scan path's per-file probe deadline -- a hung exiftool on a stalled network
// mount must not hang the HTTP request indefinitely.
const inheritWriteTimeout = 30 * time.Second

// refreshNodeAfterInPlaceWrite re-reads the file's size and re-hashes it
// (the same fast_hash a scan would compute -- xxHash64 over the same sample
// regions) after an in-place exiftool write, and persists all three onto the
// node's row. This is what keeps the DB and the file in agreement: without
// it, the next scan observes a changed fast_hash at an unchanged path and
// runs commitVersionCollision (see internal/pipeline/commit.go), archiving
// this node and inserting a successor under a new node_uuid.
func (s *Server) refreshNodeAfterInPlaceWrite(ctx context.Context, node sqlcgen.MediaNode) error {
	rctx, cancel := context.WithTimeout(ctx, inheritWriteTimeout)
	defer cancel()

	f, err := s.guard.OpenRead(node.FilePath)
	if err != nil {
		return fmt.Errorf("open for re-hash: %w", err)
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat for re-hash: %w", err)
	}

	fastHash, err := hashing.FastHash(f, stat.Size())
	if err != nil {
		return fmt.Errorf("re-hash: %w", err)
	}

	return s.db.InTx(rctx, func(q *sqlcgen.Queries) error {
		return q.RefreshMediaNodeAfterInPlaceWrite(rctx, sqlcgen.RefreshMediaNodeAfterInPlaceWriteParams{
			ID:        node.ID,
			SizeBytes: stat.Size(),
			MtimeUnix: stat.ModTime().Unix(),
			FastHash:  &fastHash,
		})
	})
}

// validParentRelationships is the closed set of relationship types that
// represent identity ancestry -- the only kinds of edge inherit-metadata may
// treat as "this child's parent". DUPLICATE_OF is a content match, not
// ancestry: stamping a duplicate's node_uuid into XMP-xmpMM:DerivedFrom would
// fabricate a false lineage. PROJECT_SIDECAR's "parent" is the project file
// itself (the resolver makes the project file the edge's source), not a
// media ancestor whose EXIF is meaningful to inherit.
var validParentRelationships = map[string]bool{
	"DERIVED_FROM": true,
	"FINAL_EXPORT": true,
	"PROXY_OF":     true,
}

// pickWinningParent returns the highest-confidence Tier-1/2 parent edge that
// is AUTO_ACCEPTED or CONFIRMED and represents identity ancestry (see
// validParentRelationships), or nil when the node has none. A NEEDS_REVIEW
// (unconfirmed) or REJECTED edge is never a valid identity source: stamping
// an unconfirmed parent's metadata into the child's file would cement a
// possibly-wrong lineage with no recovery path. Tier-3 (heuristic) matches
// are excluded from selection entirely, not merely rejected after one is
// picked -- a Tier-3 edge existing alongside a valid Tier-1/2 parent must
// never prevent inheriting from the valid one. Equal-confidence ties break by
// lowest edge id (ListEdgesByTarget has no ORDER BY), keeping the result
// deterministic.
func pickWinningParent(edges []sqlcgen.MediaEdge) *sqlcgen.MediaEdge {
	var best *sqlcgen.MediaEdge
	for i := range edges {
		e := &edges[i]
		if e.ReviewState != "AUTO_ACCEPTED" && e.ReviewState != "CONFIRMED" {
			continue
		}
		if e.Tier == 3 || !validParentRelationships[e.RelationshipType] {
			continue
		}
		// Highest confidence wins; on a tie, the lowest edge id (deterministic,
		// since ListEdgesByTarget has no ORDER BY).
		if best == nil || e.Confidence > best.Confidence || (e.Confidence == best.Confidence && e.ID < best.ID) {
			best = e
		}
	}
	return best
}

// hasResolvedButIneligibleParent reports whether edges contains any
// AUTO_ACCEPTED/CONFIRMED edge at all. Only meaningful as a follow-up check
// after pickWinningParent(edges) has already returned nil: at that point any
// resolved edge found here must be one pickWinningParent excluded (Tier-3, or
// not an identity-ancestry relationship type) -- since if it were eligible,
// pickWinningParent would have picked it. Used only to give the caller a more
// specific refusal message than "no resolved parent edge at all" when that's
// not actually true. Calling this without first confirming pickWinningParent
// returned nil would not carry that meaning.
func hasResolvedButIneligibleParent(edges []sqlcgen.MediaEdge) bool {
	for i := range edges {
		e := &edges[i]
		if e.ReviewState == "AUTO_ACCEPTED" || e.ReviewState == "CONFIRMED" {
			return true
		}
	}
	return false
}

// loadTagSet assembles a node's inheritable tag values from its promoted
// columns and node_metadata rows (source='exiftool'). A read failure is
// surfaced, not swallowed -- a partial/empty tagset would silently produce a
// partial inheritance.
func loadTagSet(ctx context.Context, q *sqlcgen.Queries, node sqlcgen.MediaNode) (metadata.TagSet, error) {
	ts := metadata.TagSet{
		Identifier: node.NodeUuid, // XMP-dc:Identifier is always the node's own uuid
		Model:      node.CameraModel.String,
	}
	rows, err := q.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		return ts, err
	}
	for _, r := range rows {
		if r.Source != "exiftool" {
			continue
		}
		switch r.Key {
		case "EXIF:Make":
			ts.Make = r.Value
		case "EXIF:LensModel":
			ts.LensModel = r.Value
		case "EXIF:SerialNumber":
			ts.SerialNumber = r.Value
		case "EXIF:DateTimeOriginal":
			ts.DateTimeOriginal = r.Value
		case "EXIF:OffsetTimeOriginal":
			ts.OffsetTimeOriginal = r.Value
		case "Composite:GPSLatitude":
			if f, err := strconv.ParseFloat(r.Value, 64); err == nil {
				ts.GPSLatitude = &f
			}
		case "Composite:GPSLongitude":
			if f, err := strconv.ParseFloat(r.Value, 64); err == nil {
				ts.GPSLongitude = &f
			}
		}
	}
	// Fall back to the promoted captured_at_unix column when node_metadata
	// has no EXIF:DateTimeOriginal row -- a pre-#54 catalog, or any
	// agent-ingested node (internal/agent never writes node_metadata at
	// all). captured_at_unix is an absolute instant (see
	// internal/probe/probe.go's capturedAt), so it's paired with an
	// explicit +00:00 offset rather than a bare wall clock, which exiftool
	// would misread as local time -- on the PARENT side that wrong value is
	// exactly what Plan below writes into the child's file. The overwrite
	// is unconditional (not gated on ts.OffsetTimeOriginal already being
	// set) deliberately: DateTimeOriginal here is always the UTC rendering,
	// so pairing it with any other offset would misname the instant.
	if ts.DateTimeOriginal == "" && node.CapturedAtUnix.Valid {
		ts.DateTimeOriginal = time.Unix(node.CapturedAtUnix.Int64, 0).UTC().Format("2006:01:02 15:04:05")
		ts.OffsetTimeOriginal = "+00:00"
	}
	return ts, nil
}

// --- /api/v1/edges ---

type CreateEdgeInput struct {
	Body struct {
		SourceNodeID     int64  `json:"sourceNodeId" doc:"Source node ID (parent)"`
		TargetNodeID     int64  `json:"targetNodeId" doc:"Target node ID (child)"`
		RelationshipType string `json:"relationshipType" doc:"Relationship type"`
	}
}

type CreateEdgeOutput struct {
	Body edgeDTO
}

func (s *Server) handleCreateEdge(ctx context.Context, in *CreateEdgeInput) (*CreateEdgeOutput, error) {
	if in.Body.SourceNodeID == in.Body.TargetNodeID {
		return nil, huma.Error422UnprocessableEntity("self-loop edge is forbidden")
	}

	// The full schema-legal set (media_edges.relationship_type's CHECK
	// constraint) -- a superset of validParentRelationships below, which
	// narrows to the subset inherit-metadata may treat as identity ancestry.
	// Keep the two in sync with the schema if it ever gains a relationship
	// type.
	validRels := map[string]bool{
		"DERIVED_FROM":    true,
		"FINAL_EXPORT":    true,
		"PROXY_OF":        true,
		"PROJECT_SIDECAR": true,
		"DUPLICATE_OF":    true,
	}
	if !validRels[in.Body.RelationshipType] {
		return nil, huma.Error422UnprocessableEntity("invalid relationship type")
	}

	// #199: a manual edge must not use a project file as an endpoint of an
	// identity-ancestry relationship (.drp/.fcpxml/.edl/.dam.json/.prproj,
	// whatever internal/projectfile's registry recognizes). Such a node is a
	// container of references, not a camera-derived asset: an edge whose CHILD
	// is a project file would pass pickWinningParent's filters (its
	// validParentRelationships) and make handleInheritMetadata run exiftool
	// in-place against the project archive. The target-side refusal is scoped
	// to those identity-ancestry relationship types: PROJECT_SIDECAR
	// *legitimately* targets a project file (ProjectSidecarResolver.Resolve
	// always makes the project file the edge's child), and being excluded from
	// validParentRelationships it can never reach pickWinningParent/
	// handleInheritMetadata regardless of endpoint shape. The source side is
	// unconditional -- the resolver never makes a project file the source of
	// any edge, so no legitimate manual edge needs a project file as parent.
	target, err := s.db.Reader.GetMediaNodeByID(ctx, in.Body.TargetNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("target asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get target asset", err)
	}
	if _, ok := projectfile.GetParser(target.FilePath); ok && validParentRelationships[in.Body.RelationshipType] {
		return nil, huma.Error422UnprocessableEntity("cannot create an identity-ancestry edge targeting a project file")
	}
	source, err := s.db.Reader.GetMediaNodeByID(ctx, in.Body.SourceNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("source asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get source asset", err)
	}
	if _, ok := projectfile.GetParser(source.FilePath); ok {
		return nil, huma.Error422UnprocessableEntity("cannot create an edge sourced from a project file")
	}

	var createdEdge sqlcgen.MediaEdge
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		wouldCycle, err := q.WouldCreateCycle(ctx, sqlcgen.WouldCreateCycleParams{
			ParentNodeID: in.Body.SourceNodeID,
			ChildNodeID:  in.Body.TargetNodeID,
		})
		if err != nil {
			return err
		}
		if wouldCycle {
			return huma.Error409Conflict("creating this edge would close a cycle")
		}

		edge, err := q.CreateManualMediaEdge(ctx, sqlcgen.CreateManualMediaEdgeParams{
			SourceNodeID:     in.Body.SourceNodeID,
			TargetNodeID:     in.Body.TargetNodeID,
			RelationshipType: in.Body.RelationshipType,
			ReviewedBy:       reviewerName(ctx),
		})
		if err != nil {
			return err
		}
		createdEdge = edge
		// A manual edge is CONFIRMED at insert time (CreateManualMediaEdge) --
		// the same review_state write confirm/reject makes, so it needs the
		// same graph_status recompute or the target node stays UNLINKED
		// despite now having a confirmed parent.
		return recomputeGraphStatus(ctx, q, in.Body.TargetNodeID)
	})
	if err != nil {
		var humaErr huma.StatusError
		if errors.As(err, &humaErr) {
			return nil, humaErr
		}
		return nil, huma.Error500InternalServerError("create manual edge", err)
	}

	out := &CreateEdgeOutput{}
	out.Body = edgeDTO{
		ID:               createdEdge.ID,
		SourceNodeID:     createdEdge.SourceNodeID,
		TargetNodeID:     createdEdge.TargetNodeID,
		RelationshipType: createdEdge.RelationshipType,
		Confidence:       createdEdge.Confidence,
		ReviewState:      createdEdge.ReviewState,
		Resolver:         createdEdge.Resolver,
	}
	return out, nil
}

// --- /api/v1/edges/audit ---

type AuditQueueInput struct {
	Limit  int64 `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Offset int64 `query:"offset" default:"0" minimum:"0"`
}

type auditNodeDTO struct {
	ID             int64   `json:"id"`
	FileName       string  `json:"fileName"`
	FilePath       string  `json:"filePath"`
	CapturedAtUnix *int64  `json:"capturedAtUnix,omitempty"`
	CameraModel    *string `json:"cameraModel,omitempty"`
	Phash          *int64  `json:"phash,omitempty"`
}

type auditEntryDTO struct {
	ID                  int64        `json:"id"`
	SourceNodeID        int64        `json:"sourceNodeId"`
	TargetNodeID        int64        `json:"targetNodeId"`
	RelationshipType    string       `json:"relationshipType"`
	Confidence          float64      `json:"confidence"`
	Tier                int64        `json:"tier"`
	Resolver            string       `json:"resolver"`
	EvidenceJSON        string       `json:"evidenceJson"`
	ParentAlive         bool         `json:"parentAlive"`
	ParentMissing       bool         `json:"parentMissing"`
	SourceNode          auditNodeDTO `json:"sourceNode"`
	TargetNode          auditNodeDTO `json:"targetNode"`
	CaptureDeltaSeconds *int64       `json:"captureDeltaSeconds,omitempty"`
	PhashDistance       *int         `json:"phashDistance,omitempty"`
}

type AuditQueueOutput struct {
	Body struct {
		Entries []auditEntryDTO `json:"entries"`
	}
}

func (s *Server) handleAuditQueue(ctx context.Context, in *AuditQueueInput) (*AuditQueueOutput, error) {
	rows, err := s.db.Reader.ListAuditQueueDetailed(ctx, sqlcgen.ListAuditQueueDetailedParams{Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return nil, huma.Error500InternalServerError("list audit queue", err)
	}
	out := &AuditQueueOutput{}
	out.Body.Entries = make([]auditEntryDTO, len(rows))
	for i, r := range rows {
		sn := auditNodeDTO{
			ID:       r.SourceNodeID,
			FileName: r.SourceFileName,
			FilePath: r.SourceFilePath,
		}
		if r.SourceCapturedAtUnix.Valid {
			sn.CapturedAtUnix = &r.SourceCapturedAtUnix.Int64
		}
		if r.SourceCameraModel.Valid {
			sn.CameraModel = &r.SourceCameraModel.String
		}
		if r.SourcePhash.Valid {
			sn.Phash = &r.SourcePhash.Int64
		}

		tn := auditNodeDTO{
			ID:       r.TargetNodeID,
			FileName: r.TargetFileName,
			FilePath: r.TargetFilePath,
		}
		if r.TargetCapturedAtUnix.Valid {
			tn.CapturedAtUnix = &r.TargetCapturedAtUnix.Int64
		}
		if r.TargetCameraModel.Valid {
			tn.CameraModel = &r.TargetCameraModel.String
		}
		if r.TargetPhash.Valid {
			tn.Phash = &r.TargetPhash.Int64
		}

		var captureDelta *int64
		if r.SourceCapturedAtUnix.Valid && r.TargetCapturedAtUnix.Valid {
			d := r.SourceCapturedAtUnix.Int64 - r.TargetCapturedAtUnix.Int64
			if d < 0 {
				d = -d
			}
			captureDelta = &d
		}

		var phashDist *int
		if r.SourcePhash.Valid && r.TargetPhash.Valid {
			dist := hashing.HammingDistance(r.SourcePhash.Int64, r.TargetPhash.Int64)
			phashDist = &dist
		}

		out.Body.Entries[i] = auditEntryDTO{
			ID:                  r.ID,
			SourceNodeID:        r.SourceNodeID,
			TargetNodeID:        r.TargetNodeID,
			RelationshipType:    r.RelationshipType,
			Confidence:          r.Confidence,
			Tier:                r.Tier,
			Resolver:            r.Resolver,
			EvidenceJSON:        r.EvidenceJson,
			ParentAlive:         r.ParentAlive,
			ParentMissing:       r.ParentMissing,
			SourceNode:          sn,
			TargetNode:          tn,
			CaptureDeltaSeconds: captureDelta,
			PhashDistance:       phashDist,
		}
	}
	return out, nil
}

// --- /api/v1/edges/{id}/confirm and /reject ---

type EdgeReviewInput struct {
	ID int64 `path:"id"`
}

type EdgeReviewOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func reviewerName(ctx context.Context) sql.NullString {
	p, ok := auth.From(ctx)
	if !ok || p.Name == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: p.Name, Valid: true}
}

// recomputeGraphStatus delegates to graph.RecomputeStatusFromPersistedEdges,
// which now also backs internal/agent's applyEdgeAttached -- see that
// function's doc comment for why this is a distinct computation from
// Engine.ResolveAndCommit's inline status update, and not something to
// "unify" with it.
func recomputeGraphStatus(ctx context.Context, q *sqlcgen.Queries, nodeID int64) error {
	return graph.RecomputeStatusFromPersistedEdges(ctx, q, nodeID)
}

func (s *Server) handleConfirmEdge(ctx context.Context, in *EdgeReviewInput) (*EdgeReviewOutput, error) {
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		targetNodeID, err := q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{ID: in.ID, ReviewedBy: reviewerName(ctx)})
		if err != nil {
			return err
		}
		return recomputeGraphStatus(ctx, q, targetNodeID)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("edge not found")
		}
		return nil, huma.Error500InternalServerError("confirm edge", err)
	}
	out := &EdgeReviewOutput{}
	out.Body.OK = true
	return out, nil
}

func (s *Server) handleRejectEdge(ctx context.Context, in *EdgeReviewInput) (*EdgeReviewOutput, error) {
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		targetNodeID, err := q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{ID: in.ID, ReviewedBy: reviewerName(ctx)})
		if err != nil {
			return err
		}
		return recomputeGraphStatus(ctx, q, targetNodeID)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("edge not found")
		}
		return nil, huma.Error500InternalServerError("reject edge", err)
	}
	out := &EdgeReviewOutput{}
	out.Body.OK = true
	return out, nil
}

// --- /api/v1/scan ---

type StartScanInput struct {
	Body struct {
		StorageLocationID int64 `json:"storageLocationId"`
	}
}

type StartScanOutput struct {
	Body struct {
		JobID int64 `json:"jobId"`
	}
}

func (s *Server) handleStartScan(ctx context.Context, in *StartScanInput) (*StartScanOutput, error) {
	row, err := s.db.Reader.GetStorageLocationByID(ctx, in.Body.StorageLocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("storage location not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get storage location", err)
	}

	location := storage.Location{
		ID: row.ID, Name: row.Name, RootPath: row.RootPath, Tier: row.Tier, ReadOnly: row.ReadOnly != 0,
	}

	fullHashPolicy := "tier3_and_collision"
	disablePHash := false
	if s.cfg != nil {
		fullHashPolicy = s.cfg.Workers.FullHashPolicy
		disablePHash = !s.cfg.Workers.PerceptualHash
	}

	jobID, err := pipeline.RunScan(ctx, pipeline.ScanDeps{
		DB: s.db, Guard: s.guard, Prober: s.prober, Pool: s.pool, Engine: s.engine,
		FullHashPolicy: fullHashPolicy, DisablePerceptualHash: disablePHash, Log: s.log,
		Tracker: s.tracker, Shutdown: s.shutdown,
		Nudge: func() {
			if s.hub != nil {
				s.hub.Broadcast()
			}
		},
	}, location)
	if errors.Is(err, pipeline.ErrScanAlreadyRunning) {
		return nil, huma.Error409Conflict("a scan is already running for this storage location")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("start scan", err)
	}

	if s.hub != nil {
		s.hub.Broadcast()
	}

	out := &StartScanOutput{}
	out.Body.JobID = jobID
	return out, nil
}

// --- /api/v1/progress ---

type ProgressInput struct {
	Limit int64 `query:"limit" default:"10" minimum:"1" maximum:"100"`
}

type scanJobDTO struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	FilesSeen    int64  `json:"filesSeen"`
	FilesHashed  int64  `json:"filesHashed"`
	FilesFailed  int64  `json:"filesFailed"`
	EdgesCreated int64  `json:"edgesCreated"`
	LastError    string `json:"lastError,omitempty"`
}

type ProgressOutput struct {
	Body struct {
		Jobs []scanJobDTO `json:"jobs"`
	}
}

func (s *Server) handleProgress(ctx context.Context, in *ProgressInput) (*ProgressOutput, error) {
	rows, err := s.db.Reader.ListRecentScanJobs(ctx, in.Limit)
	if err != nil {
		return nil, huma.Error500InternalServerError("list scan jobs", err)
	}
	out := &ProgressOutput{}
	out.Body.Jobs = make([]scanJobDTO, len(rows))
	for i, r := range rows {
		dto := scanJobDTO{
			ID: r.ID, Kind: r.Kind, State: r.State,
			FilesSeen: r.FilesSeen, FilesHashed: r.FilesHashed, FilesFailed: r.FilesFailed,
			EdgesCreated: r.EdgesCreated,
		}
		if r.LastError.Valid {
			dto.LastError = r.LastError.String
		}
		out.Body.Jobs[i] = dto
	}
	return out, nil
}

// --- /api/v1/jobs ---

type ListJobsInput struct {
	Limit  int64  `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Offset int64  `query:"offset" default:"0" minimum:"0"`
	Kind   string `query:"kind"`
	State  string `query:"state"`
}

type ListJobsOutput struct {
	Body struct {
		Jobs  []scanJobDTO `json:"jobs"`
		Total int64        `json:"total"`
	}
}

func (s *Server) handleListJobs(ctx context.Context, in *ListJobsInput) (*ListJobsOutput, error) {
	var kind sql.NullString
	if in.Kind != "" {
		kind = sql.NullString{String: in.Kind, Valid: true}
	}
	var state sql.NullString
	if in.State != "" {
		state = sql.NullString{String: in.State, Valid: true}
	}

	rows, err := s.db.Reader.ListScanJobsFiltered(ctx, sqlcgen.ListScanJobsFilteredParams{
		Limit:  in.Limit,
		Offset: in.Offset,
		Kind:   kind,
		State:  state,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("list scan jobs", err)
	}

	total, err := s.db.Reader.CountScanJobsFiltered(ctx, sqlcgen.CountScanJobsFilteredParams{
		Kind:  kind,
		State: state,
	})
	if err != nil {
		total = int64(len(rows))
	}

	out := &ListJobsOutput{}
	out.Body.Jobs = make([]scanJobDTO, len(rows))
	for i, r := range rows {
		dto := scanJobDTO{
			ID: r.ID, Kind: r.Kind, State: r.State,
			FilesSeen: r.FilesSeen, FilesHashed: r.FilesHashed, FilesFailed: r.FilesFailed,
			EdgesCreated: r.EdgesCreated,
		}
		if r.LastError.Valid {
			dto.LastError = r.LastError.String
		}
		out.Body.Jobs[i] = dto
	}
	out.Body.Total = total
	return out, nil
}

// --- /api/v1/agent/hello ---

type AgentHelloOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
}

func (s *Server) handleAgentHello(_ context.Context, _ *struct{}) (*AgentHelloOutput, error) {
	out := &AgentHelloOutput{}
	out.Body.OK = true
	out.Body.Version = s.version
	return out, nil
}

// --- /api/v1/agent/events ---

type AgentEventInput struct {
	Body struct {
		AgentID   string `json:"agentId" required:"true"`
		EventType string `json:"eventType" required:"true" enum:"EVENT_NODE_CREATED,EVENT_EDGE_ATTACHED,EVENT_NODE_MOVED,EVENT_NODE_DELETED,EVENT_PATH_REBASED"`
		Payload   string `json:"payload" required:"true"` // opaque JSON, validated/processed by the deferred agent-events increment
	}
}

type AgentEventOutput struct {
	Body struct {
		EventID string `json:"eventId"`
	}
}

// handleAgentEvent persists the event and returns 202 Accepted; actually
// draining/processing event_queue ships with the
// deferred workstation-agent increment (see internal/db's event_queue
// migration comment).
func (s *Server) handleAgentEvent(ctx context.Context, in *AgentEventInput) (*AgentEventOutput, error) {
	if p, ok := auth.From(ctx); !ok || p.Kind != auth.KindMachine {
		return nil, huma.Error403Forbidden("agent machine principal required", nil)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, huma.Error500InternalServerError("mint event id", err)
	}

	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.EnqueueAgentEvent(ctx, sqlcgen.EnqueueAgentEventParams{
			EventUuid:   id.String(),
			AgentID:     in.Body.AgentID,
			EventType:   in.Body.EventType,
			PayloadJson: in.Body.Payload,
		})
		return err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("enqueue agent event", fmt.Errorf("%w", err))
	}

	out := &AgentEventOutput{}
	out.Body.EventID = id.String()
	return out, nil
}

// --- /api/v1/agent/handshake ---

type AgentHandshakeInput struct {
	Body struct {
		AgentID                string `json:"agentId" required:"true"`
		ClientVersion          string `json:"clientVersion,omitempty"`
		LastProcessedEventUUID string `json:"lastProcessedEventUuid,omitempty"`
	}
}

type AgentHandshakeOutput struct {
	Body struct {
		OK                    bool   `json:"ok"`
		ServerVersion         string `json:"serverVersion"`
		ServerTimeUnix        int64  `json:"serverTimeUnix"`
		AcknowledgedEventUUID string `json:"acknowledgedEventUuid,omitempty"`
		PendingEventsCount    int64  `json:"pendingEventsCount"`
	}
}

func (s *Server) handleAgentHandshake(ctx context.Context, in *AgentHandshakeInput) (*AgentHandshakeOutput, error) {
	if p, ok := auth.From(ctx); !ok || p.Kind != auth.KindMachine {
		return nil, huma.Error403Forbidden("agent machine principal required", nil)
	}

	var pendingCount int64
	var ackUUID string
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		pendingCount, err = q.CountPendingAgentEvents(ctx)
		if err != nil {
			return err
		}
		if in.Body.AgentID != "" {
			latest, err := q.GetLatestProcessedAgentEventByAgent(ctx, in.Body.AgentID)
			if err == nil {
				ackUUID = latest.EventUuid
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("handshake query failed", err)
	}

	out := &AgentHandshakeOutput{}
	out.Body.OK = true
	out.Body.ServerVersion = s.version
	out.Body.ServerTimeUnix = time.Now().Unix()
	out.Body.AcknowledgedEventUUID = ackUUID
	out.Body.PendingEventsCount = pendingCount
	return out, nil
}

// --- /api/v1/agent/rebase ---

type AgentRebaseInput struct {
	Body struct {
		NodeUUID          string  `json:"nodeUuid" required:"true"`
		TargetPath        string  `json:"targetPath" required:"true"`
		MtimeUnix         int64   `json:"mtimeUnix,omitempty"`
		FileName          string  `json:"fileName,omitempty"`
		FileExt           string  `json:"fileExt,omitempty"`
		SizeBytes         int64   `json:"sizeBytes,omitempty"`
		FastHash          *string `json:"fastHash,omitempty"`
		StorageLocationID int64   `json:"storageLocationId,omitempty"`
	}
}

type AgentRebaseOutput struct {
	Body struct {
		ID                int64  `json:"id"`
		NodeUUID          string `json:"nodeUuid"`
		StorageLocationID int64  `json:"storageLocationId"`
		FilePath          string `json:"filePath"`
		Status            string `json:"status"`
	}
}

func (s *Server) handleAgentRebase(ctx context.Context, in *AgentRebaseInput) (*AgentRebaseOutput, error) {
	if p, ok := auth.From(ctx); !ok || p.Kind != auth.KindMachine {
		return nil, huma.Error403Forbidden("agent machine principal required", nil)
	}
	if in.Body.NodeUUID == "" {
		return nil, huma.Error400BadRequest("nodeUuid is required", nil)
	}
	if in.Body.TargetPath == "" {
		return nil, huma.Error400BadRequest("targetPath is required", nil)
	}

	if s.guard == nil {
		return nil, huma.Error500InternalServerError("storage guard unconfigured", nil)
	}

	loc, err := s.guard.Resolve(in.Body.TargetPath)
	if err != nil {
		return nil, huma.Error400BadRequest(fmt.Sprintf("invalid target path %q: %v", in.Body.TargetPath, err), err)
	}
	// Issue #167: a read-only target is refused unless it is specifically
	// TIER3_MASTER_ARCHIVE and the file already exists there (spec §9's
	// required LOCAL_STAGING -> CENTRAL_TIER3 scenario -- the workstation
	// agent has already copied the bytes into the archive itself; this call
	// only records that location in the database, never writes to the
	// archive). guard.Exists is a pure stat. See
	// internal/agent/drainer.go's resolveRebaseTarget for the identical
	// policy on the event-driven rebase paths.
	if loc.ReadOnly || loc.Tier == "TIER3_MASTER_ARCHIVE" {
		if loc.Tier != "TIER3_MASTER_ARCHIVE" {
			return nil, huma.Error400BadRequest(fmt.Sprintf("rebase to read-only tier %s refused for path %q", loc.Tier, in.Body.TargetPath), nil)
		}
		exists, err := s.guard.Exists(in.Body.TargetPath)
		if err != nil {
			return nil, huma.Error400BadRequest(fmt.Sprintf("checking whether rebase target %q already exists: %v", in.Body.TargetPath, err), err)
		}
		if !exists {
			return nil, huma.Error400BadRequest(fmt.Sprintf("rebase to read-only tier %s refused for path %q: no file exists there yet", loc.Tier, in.Body.TargetPath), nil)
		}
	}

	fileName := in.Body.FileName
	if fileName == "" {
		fileName = filepath.Base(in.Body.TargetPath)
	}
	fileExt := in.Body.FileExt
	if fileExt == "" {
		fileExt = filepath.Ext(in.Body.TargetPath)
	}
	mtime := in.Body.MtimeUnix
	if mtime == 0 {
		mtime = time.Now().Unix()
	}

	var archivedNode bool
	out := &AgentRebaseOutput{}
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		existing, err := q.GetMediaNodeByUUID(ctx, in.Body.NodeUUID)
		if err == nil {
			// An ARCHIVED node is a superseded version. RebaseNodePathByUUID
			// sets lifecycle_state='ACTIVE' unconditionally with no
			// lifecycle filter of its own -- rebasing here would resurrect
			// it. Refuse before that happens, same as internal/agent's
			// drainer for the two event-driven rebase paths.
			if existing.LifecycleState == "ARCHIVED" {
				archivedNode = true
				return nil
			}
			// Known node: rebase path in place, preserving id, content hashes, and lineage edges.
			// Note: Content hashes are not overwritten on path rebase; only location/path/mtime are updated.
			if err := q.RebaseNodePathByUUID(ctx, sqlcgen.RebaseNodePathByUUIDParams{
				NodeUuid:          in.Body.NodeUUID,
				FilePath:          in.Body.TargetPath,
				FileName:          fileName,
				StorageLocationID: loc.ID,
				MtimeUnix:         mtime,
			}); err != nil {
				return err
			}
			out.Body.ID = existing.ID
			out.Body.NodeUUID = existing.NodeUuid
			out.Body.StorageLocationID = loc.ID
			out.Body.FilePath = in.Body.TargetPath
			out.Body.Status = "REBASED"
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Unknown node: agent is the source of truth for an offline staged file; create node!
		newNode, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:           in.Body.NodeUUID,
			StorageLocationID:  loc.ID,
			FilePath:           in.Body.TargetPath,
			FileName:           fileName,
			FileExt:            fileExt,
			SizeBytes:          in.Body.SizeBytes,
			MtimeUnix:          mtime,
			FastHash:           in.Body.FastHash,
			FullHash:           nil,
			Phash:              sql.NullInt64{},
			IndexingStatus:     "INDEXED_SHALLOW",
			GraphStatus:        "UNLINKED",
			LifecycleState:     "ACTIVE",
			OriginalDocumentID: sql.NullString{},
			DocumentID:         sql.NullString{},
			DerivedFromID:      sql.NullString{},
			CapturedAtUnix:     sql.NullInt64{},
			CameraModel:        sql.NullString{},
			FilenameStem:       sql.NullString{},
			CameraSerial:       sql.NullString{},
			LensModel:          sql.NullString{},
		})
		if err != nil {
			return err
		}

		out.Body.ID = newNode.ID
		out.Body.NodeUUID = newNode.NodeUuid
		out.Body.StorageLocationID = loc.ID
		out.Body.FilePath = in.Body.TargetPath
		out.Body.Status = "CREATED"
		return nil
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("rebase operation failed", err)
	}
	if archivedNode {
		return nil, huma.Error404NotFound("asset not found")
	}

	return out, nil
}

// --- /api/v1/storage-health ---

type storageLocationHealthDTO struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	RootPath        string  `json:"rootPath"`
	Tier            string  `json:"tier"`
	ReadOnly        bool    `json:"readOnly"`
	Prunable        bool    `json:"prunable"`
	IsActive        bool    `json:"isActive"`
	NodeCount       int64   `json:"nodeCount"`
	TotalBytes      uint64  `json:"totalBytes"`
	UsedBytes       uint64  `json:"usedBytes"`
	FreeBytes       uint64  `json:"freeBytes"`
	IsDegraded      bool    `json:"isDegraded"`
	DegradedMessage *string `json:"degradedMessage,omitempty"`
}

type storageQueueHealthDTO struct {
	WorkerPoolInFlight int   `json:"workerPoolInFlight"`
	WorkerPoolQueued   int   `json:"workerPoolQueued"`
	WorkerPoolCapacity int   `json:"workerPoolCapacity"`
	WorkerCount        int   `json:"workerCount"`
	RunningScanJobs    int64 `json:"runningScanJobs"`
}

type storageHealthDTO struct {
	Locations []storageLocationHealthDTO `json:"locations"`
	Queues    storageQueueHealthDTO      `json:"queues"`
}

type storageHealthOutput struct {
	Body storageHealthDTO
}

// statfsTimeout bounds how long handleStorageHealth waits on any single
// location's unix.Statfs call. A hung NFS/SMB mount makes Statfs block
// indefinitely with no cancellation mechanism of its own -- this is what
// stops one bad mount from hanging the whole /api/v1/storage-health
// response (and, transitively, delaying shutdown: httpServer.Shutdown
// waits for in-flight requests).
const statfsTimeout = 2 * time.Second

// statfsWithTimeout runs unix.Statfs on its own goroutine and returns a
// timeout error if it doesn't complete within timeout. The goroutine is
// NOT cancelled if the deadline is hit -- there is no way to interrupt a
// blocked Statfs syscall in Go, so a genuinely wedged mount leaks one
// goroutine per timed-out probe until it eventually unblocks (or the
// process exits). That is the acceptable trade-off here: a leaked
// goroutine on a rare, already-degraded mount is far better than every
// caller of this handler hanging until the mount recovers.
func statfsWithTimeout(path string, timeout time.Duration) (unix.Statfs_t, error) {
	return statfsWithTimeoutFn(unix.Statfs, path, timeout)
}

// statfsWithTimeoutFn is statfsWithTimeout with the syscall itself injected,
// so tests can force the timeout branch deterministically -- with a real
// syscall, racing a positive timeout against how fast Statfs happens to
// return is inherently flaky (CI's runner resolved a real local Statfs call
// fast enough to occasionally win even a 1ns timeout, which an earlier
// version of this test relied on). Substituting a stub that blocks forever
// makes the timeout branch win deterministically against any real,
// non-degenerate timeout instead.
func statfsWithTimeoutFn(statfs func(path string, buf *unix.Statfs_t) error, path string, timeout time.Duration) (unix.Statfs_t, error) {
	type result struct {
		stat unix.Statfs_t
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = statfs(path, &r.stat)
		done <- r
	}()
	// time.NewTimer (not time.After) so the common success path can Stop
	// and release the timer immediately, rather than leaving it running
	// for the rest of statfsTimeout after every single successful probe.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.stat, r.err
	case <-timer.C:
		return unix.Statfs_t{}, fmt.Errorf("storage probe timed out after %s", timeout)
	}
}

func (s *Server) handleStorageHealth(ctx context.Context, _ *struct{}) (*storageHealthOutput, error) {
	out := &storageHealthOutput{}
	out.Body.Locations = []storageLocationHealthDTO{}

	var locations []sqlcgen.StorageLocation
	var counts []sqlcgen.ListNodeCountsByLocationRow
	var runningJobs int64

	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		locations, err = q.ListStorageLocations(ctx)
		if err != nil {
			return err
		}
		counts, err = q.ListNodeCountsByLocation(ctx)
		if err != nil {
			return err
		}
		runningJobs, err = q.CountRunningScanJobs(ctx)
		return err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("fetch storage health metrics", err)
	}

	countMap := make(map[int64]int64, len(counts))
	for _, c := range counts {
		countMap[c.StorageLocationID] = c.NodeCount
	}

	for _, loc := range locations {
		dto := storageLocationHealthDTO{
			ID:        loc.ID,
			Name:      loc.Name,
			RootPath:  loc.RootPath,
			Tier:      loc.Tier,
			ReadOnly:  loc.ReadOnly == 1,
			Prunable:  loc.Prunable == 1,
			IsActive:  loc.IsActive == 1,
			NodeCount: countMap[loc.ID],
		}

		stat, err := statfsWithTimeout(loc.RootPath, statfsTimeout)
		if err != nil {
			dto.IsDegraded = true
			msg := err.Error()
			dto.DegradedMessage = &msg
		} else {
			bsize := uint64(stat.Bsize)
			dto.TotalBytes = stat.Blocks * bsize
			dto.FreeBytes = stat.Bavail * bsize
			if stat.Blocks >= stat.Bfree {
				dto.UsedBytes = (stat.Blocks - stat.Bfree) * bsize
			}
		}
		out.Body.Locations = append(out.Body.Locations, dto)
	}

	if s.pool != nil {
		out.Body.Queues = storageQueueHealthDTO{
			WorkerPoolInFlight: s.pool.InFlight(),
			WorkerPoolQueued:   s.pool.QueueDepth(),
			WorkerPoolCapacity: s.pool.QueueCapacity(),
			WorkerCount:        s.pool.WorkerCount(),
			RunningScanJobs:    runningJobs,
		}
	} else {
		out.Body.Queues = storageQueueHealthDTO{
			RunningScanJobs: runningJobs,
		}
	}

	return out, nil
}

// --- /api/v1/prune ---

type pruneCandidateDTO struct {
	NodeID    int64  `json:"nodeId"`
	FilePath  string `json:"filePath"`
	SizeBytes int64  `json:"sizeBytes"`
	Purged    bool   `json:"purged"`
	Error     string `json:"error,omitempty"`
}

type PruneInput struct {
	Body struct {
		StorageLocationID int64 `json:"storageLocationId"`
		// NodeIDs, if non-empty, narrows the plan to exactly these nodes --
		// backs the per-asset [Purge Cache] action. Empty means "every
		// eligible node on the location".
		NodeIDs []int64 `json:"nodeIds,omitempty"`
		// Execute defaults to false (the JSON zero value) deliberately --
		// an empty request body must plan, never purge. A real purge
		// requires the caller to set this explicitly.
		Execute bool `json:"execute,omitempty"`
	}
}

type PruneOutput struct {
	Body struct {
		Executed   bool                `json:"executed"`
		Candidates []pruneCandidateDTO `json:"candidates"`
	}
}

// handlePrune plans -- and, if Execute is set, runs -- a TTL cache purge
// for one storage location (#61). Dry-run (Execute=false, the default)
// only ever reads: it resolves the location's configured cacheTtlHours,
// asks internal/prune.Plan for every eligible node, and returns the
// candidate list with purged=false and no error. Execute=true additionally
// runs internal/prune.Execute, which is the second (and last) real
// production caller of storage.Guard.Remove -- every deletion still
// resolves through Guard.CheckWrite first, so a symlink from this
// (Tier-1-only, schema-enforced) location into Tier 3 is refused before
// any syscall, the same defense storage.TestSymlinkEscapeRefused proves
// for every other Guard caller.
//
// A non-prunable location returns zero candidates (200), not a 422 --
// deliberately the same "never eligible" response shape as a prunable
// location with no cacheTtlHours configured, both below. This is what lets
// AssetDetailPage's per-asset [Purge Cache] control (which has no way to
// know a given asset's location tier/prunable flag up front, unlike
// StorageHealthPage's per-location control, which gates on loc.prunable
// before ever calling this endpoint) report "not eligible" uniformly
// instead of surfacing a location-tier implementation detail as an error.
func (s *Server) handlePrune(ctx context.Context, in *PruneInput) (*PruneOutput, error) {
	loc, err := s.db.Reader.GetStorageLocationByID(ctx, in.Body.StorageLocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("storage location not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get storage location", err)
	}
	out := &PruneOutput{}
	out.Body.Candidates = []pruneCandidateDTO{}
	if loc.Prunable == 0 {
		return out, nil
	}

	// cacheTtlHours is config-only (#61's design: no new migration), so the
	// DB row alone can't answer "what's this location's TTL" -- match it
	// against the live config by root_path, the same join key
	// GetStorageLocationByPath already uses as the source of truth for a
	// location's identity.
	ttlHours := 0
	if s.cfg != nil {
		for _, c := range s.cfg.StorageLocations {
			if c.RootPath == loc.RootPath {
				ttlHours = c.CacheTTLHours
				break
			}
		}
	}
	if ttlHours <= 0 {
		// prunable=true with no configured TTL means "never eligible" --
		// not a config error, just nothing to plan.
		return out, nil
	}
	cutoffUnix := time.Now().Add(-time.Duration(ttlHours) * time.Hour).Unix()

	candidates, err := prune.Plan(ctx, s.db.Reader, loc.ID, cutoffUnix)
	if err != nil {
		return nil, huma.Error500InternalServerError("plan prune", err)
	}

	if len(in.Body.NodeIDs) > 0 {
		want := make(map[int64]bool, len(in.Body.NodeIDs))
		for _, id := range in.Body.NodeIDs {
			want[id] = true
		}
		filtered := candidates[:0]
		for _, c := range candidates {
			if want[c.NodeID] {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}

	if !in.Body.Execute {
		for _, c := range candidates {
			out.Body.Candidates = append(out.Body.Candidates, pruneCandidateDTO{
				NodeID: c.NodeID, FilePath: c.FilePath, SizeBytes: c.SizeBytes,
			})
		}
		return out, nil
	}

	if s.guard == nil {
		return nil, huma.Error500InternalServerError("purge requested but no storage guard is configured", nil)
	}
	results := prune.Execute(ctx, s.db, s.guard, candidates, cutoffUnix)
	out.Body.Executed = true
	for _, r := range results {
		dto := pruneCandidateDTO{NodeID: r.NodeID, FilePath: r.FilePath, SizeBytes: r.SizeBytes, Purged: r.Purged}
		if r.Err != nil {
			dto.Error = r.Err.Error()
		}
		out.Body.Candidates = append(out.Body.Candidates, dto)
	}
	if s.hub != nil {
		s.hub.Broadcast()
	}
	return out, nil
}
