package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func (s *Server) registerRoutes(api huma.API) {
	huma.Get(api, "/api/v1/me", s.handleMe)
	huma.Get(api, "/api/v1/config", s.handleConfig)
	huma.Get(api, "/api/v1/config/path-rewrites", s.handleListPathRewrites)

	huma.Get(api, "/api/v1/storage-locations", s.handleListStorageLocations)

	huma.Get(api, "/api/v1/assets", s.handleListAssets)
	huma.Get(api, "/api/v1/assets/{id}", s.handleGetAsset)
	huma.Get(api, "/api/v1/assets/{id}/graph", s.handleAssetGraph)
	huma.Get(api, "/api/v1/assets/{id}/lineage", s.handleAssetLineage)

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
		Path:          "/api/v1/agent/events",
		OperationID:   "submitAgentEvent",
		Summary:       "Accept a workstation agent event",
		DefaultStatus: http.StatusAccepted,
	}, s.handleAgentEvent)
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
	Limit  int64 `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Offset int64 `query:"offset" default:"0" minimum:"0"`
}

type ListAssetsOutput struct {
	Body struct {
		Assets []assetDTO `json:"assets"`
	}
}

func (s *Server) handleListAssets(ctx context.Context, in *ListAssetsInput) (*ListAssetsOutput, error) {
	rows, err := s.db.Reader.ListMediaNodes(ctx, sqlcgen.ListMediaNodesParams{Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return nil, huma.Error500InternalServerError("list assets", err)
	}
	out := &ListAssetsOutput{}
	out.Body.Assets = make([]assetDTO, len(rows))
	for i, r := range rows {
		out.Body.Assets[i] = toAssetDTO(r)
	}
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

	var createdEdge sqlcgen.MediaEdge
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
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
		return nil
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

func (s *Server) handleConfirmEdge(ctx context.Context, in *EdgeReviewInput) (*EdgeReviewOutput, error) {
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{ID: in.ID, ReviewedBy: reviewerName(ctx)})
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("confirm edge", err)
	}
	out := &EdgeReviewOutput{}
	out.Body.OK = true
	return out, nil
}

func (s *Server) handleRejectEdge(ctx context.Context, in *EdgeReviewInput) (*EdgeReviewOutput, error) {
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{ID: in.ID, ReviewedBy: reviewerName(ctx)})
	})
	if err != nil {
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
