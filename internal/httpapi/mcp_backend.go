package httpapi

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/mcp"
)

// mcpBackend adapts the existing Huma handlers to the MCP Backend
// interface. The handlers are unexported and return Huma output
// types, so the adapter lives in this package rather than internal/mcp
// (which would import-cycle through the mount in server.go).
type mcpBackend struct {
	s *Server
}

func (b mcpBackend) SearchAssets(ctx context.Context, in mcp.SearchAssetsInput) (any, error) {
	limit, err := pageLimit(in.Limit, 50, 500)
	if err != nil {
		return nil, err
	}
	if in.Offset < 0 {
		return nil, fmt.Errorf("offset must be >= 0")
	}
	if !knownAssetSort(in.Sort) {
		return nil, fmt.Errorf("unknown sort %q", in.Sort)
	}
	out, err := b.s.handleListAssets(ctx, &ListAssetsInput{
		Limit:             limit,
		Offset:            in.Offset,
		CameraModel:       in.CameraModel,
		GraphStatus:       in.GraphStatus,
		StorageLocationID: in.StorageLocationID,
		LifecycleState:    in.LifecycleState,
		UnlinkedOnly:      in.UnlinkedOnly,
		UploadedByUserID:  in.UploadedByUserID,
		Q:                 in.Q,
		Tag:               in.Tag,
		Sort:              in.Sort,
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) ListFacets(ctx context.Context) (any, error) {
	out, err := b.s.handleListAssetFacets(ctx, nil)
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) GetAsset(ctx context.Context, id int64) (any, error) {
	asset, err := b.s.handleGetAsset(ctx, &AssetPathInput{ID: id})
	if err != nil {
		return nil, err
	}
	meta, err := b.s.handleGetAssetMetadata(ctx, &AssetPathInput{ID: id})
	if err != nil {
		return nil, err
	}
	syncStatus, err := b.s.handleAssetSyncStatus(ctx, &AssetSyncStatusInput{ID: id})
	if err != nil {
		return nil, err
	}
	tags, err := b.s.handleListAssetTags(ctx, &AssetPathInput{ID: id})
	if err != nil && !humaNotFound(err) {
		return nil, err
	}
	notes, err := b.s.handleListAssetAnnotations(ctx, &AssetPathInput{ID: id})
	if err != nil && !humaNotFound(err) {
		return nil, err
	}
	tagRows := []tagDTO{}
	if tags != nil {
		tagRows = tags.Body.Tags
	}
	noteRows := []annotationDTO{}
	if notes != nil {
		noteRows = notes.Body.Annotations
	}
	return struct {
		Asset       assetDTO           `json:"asset"`
		Metadata    []nodeMetadatumDTO `json:"metadata"`
		Sync        []syncStateDTO     `json:"sync"`
		Tags        []tagDTO           `json:"tags"`
		Annotations []annotationDTO    `json:"annotations"`
	}{
		Asset:       asset.Body,
		Metadata:    meta.Body.Metadata,
		Sync:        syncStatus.Body.Sync,
		Tags:        tagRows,
		Annotations: noteRows,
	}, nil
}

func (b mcpBackend) FindSimilar(ctx context.Context, in mcp.FindSimilarInput) (any, error) {
	limit, err := pageLimit(in.Limit, SimilarDefaultLimit, SimilarMaxLimit)
	if err != nil {
		return nil, err
	}
	dist := int64(SimilarDefaultDistance)
	if in.MaxDistance != nil {
		dist = *in.MaxDistance
	}
	if dist < 0 || dist > SimilarMaxDistance {
		return nil, fmt.Errorf("maxDistance must be between 0 and 64")
	}
	out, err := b.s.handleListSimilar(ctx, &SimilarAssetsInput{
		ID:          in.ID,
		Limit:       limit,
		MaxDistance: dist,
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) GetLineage(ctx context.Context, id int64, depth int) (any, error) {
	if depth == 0 {
		depth = 2
	}
	if depth < 1 || depth > 5 {
		return nil, fmt.Errorf("depth must be between 1 and 5")
	}
	out, err := b.s.handleAssetLineage(ctx, &AssetLineageInput{ID: id, Depth: depth})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) ViewAsset(ctx context.Context, id int64) ([]byte, bool, error) {
	asset, err := b.s.handleGetAsset(ctx, &AssetPathInput{ID: id})
	if err != nil {
		return nil, false, err
	}
	if b.s.thumbs == nil || asset.Body.NodeUUID == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(b.s.thumbs.Path(asset.Body.NodeUUID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (b mcpBackend) ListReviewQueue(ctx context.Context, limit, beforeID int64) (any, error) {
	limit, err := pageLimit(limit, 50, 500)
	if err != nil {
		return nil, err
	}
	if beforeID < 0 {
		return nil, fmt.Errorf("beforeId must be >= 0")
	}
	out, err := b.s.handleAuditQueue(ctx, &AuditQueueInput{Limit: limit, BeforeID: beforeID})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) ListJobs(ctx context.Context, kind, state string) (any, error) {
	out, err := b.s.handleListJobs(ctx, &ListJobsInput{Limit: 50, Kind: kind, State: state})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) GetScanProgress(ctx context.Context) (any, error) {
	out, err := b.s.handleProgress(ctx, &ProgressInput{Limit: 10})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) StorageHealth(ctx context.Context) (any, error) {
	health, err := b.s.handleStorageHealth(ctx, nil)
	if err != nil {
		return nil, err
	}
	agents, err := b.s.handleListAgentTelemetry(ctx, nil)
	if err != nil {
		return nil, err
	}
	return struct {
		Health storageHealthDTO        `json:"health"`
		Agents []agentScratchHealthDTO `json:"agents"`
	}{
		Health: health.Body,
		Agents: agents.Body.Agents,
	}, nil
}

func (b mcpBackend) ConfirmEdge(ctx context.Context, id int64) (any, error) {
	if err := b.s.confirmEdge(ctx, id, reviewSourceAssistant); err != nil {
		return nil, err
	}
	return struct {
		OK bool `json:"ok"`
	}{OK: true}, nil
}

func (b mcpBackend) RejectEdge(ctx context.Context, id int64) (any, error) {
	if err := b.s.rejectEdge(ctx, id, reviewSourceAssistant); err != nil {
		return nil, err
	}
	return struct {
		OK bool `json:"ok"`
	}{OK: true}, nil
}

func (b mcpBackend) ResetEdgeReview(ctx context.Context, id int64) (any, error) {
	out, err := b.s.resetEdgeReview(ctx, id, reviewSourceAssistant)
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) CreateEdge(ctx context.Context, sourceNodeID, targetNodeID int64, relationshipType string) (any, error) {
	out, err := b.s.createManualEdge(ctx, sourceNodeID, targetNodeID, relationshipType, reviewSourceAssistant)
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) TrashAsset(ctx context.Context, id int64) (any, error) {
	out, err := b.s.doTrashAsset(ctx, id, false)
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) RestoreAsset(ctx context.Context, id int64) (any, error) {
	out, err := b.s.handleRestoreAsset(ctx, &AssetPathInput{ID: id})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) RetrySync(ctx context.Context, id int64) (any, error) {
	out, err := b.s.handleSyncRetry(ctx, &SyncRetryInput{ID: id})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) ListTags(ctx context.Context) (any, error) {
	out, err := b.s.handleListTags(ctx, nil)
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b mcpBackend) TagAssets(ctx context.Context, in mcp.TagAssetsInput) (any, error) {
	ids, tags, err := prepareTagBatch(ctx, b.s, in)
	if err != nil {
		return nil, err
	}
	tagged := make([]tagDTO, 0, len(ids)*len(tags))
	for _, id := range ids {
		for _, tag := range tags {
			var input AddAssetTagInput
			input.ID = id
			input.Body.Tag = tag
			out, err := b.s.handleAddAssetTag(ctx, &input)
			if err != nil {
				return nil, err
			}
			tagged = append(tagged, out.Body)
		}
	}
	return struct {
		Tags []tagDTO `json:"tags"`
	}{Tags: tagged}, nil
}

func (b mcpBackend) UntagAssets(ctx context.Context, in mcp.TagAssetsInput) (any, error) {
	ids, tags, err := prepareTagBatch(ctx, b.s, in)
	if err != nil {
		return nil, err
	}
	type skipped struct {
		ID     int64  `json:"id"`
		Tag    string `json:"tag"`
		Reason string `json:"reason"`
	}
	removed := make([]tagDTO, 0)
	skippedRows := make([]skipped, 0)
	for _, id := range ids {
		for _, tag := range tags {
			ok, norm, reason, err := b.s.removeAssetTag(ctx, id, tag, true)
			if err != nil {
				return nil, err
			}
			if !ok {
				skippedRows = append(skippedRows, skipped{ID: id, Tag: norm, Reason: reason})
				continue
			}
			removed = append(removed, tagDTO{NodeID: id, Tag: norm})
		}
	}
	return struct {
		Removed []tagDTO  `json:"removed"`
		Skipped []skipped `json:"skipped"`
	}{Removed: removed, Skipped: skippedRows}, nil
}

func (b mcpBackend) SetDescription(ctx context.Context, in mcp.SetDescriptionInput) (any, error) {
	var input PutDescriptionInput
	input.ID = in.ID
	input.Body.Text = in.Text
	input.Body.Model = in.Model
	out, err := b.s.handlePutAssetDescription(ctx, &input)
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

// prepareTagBatch dedupes, then rejects an axis over TagBatchLimit or a
// product over TagPairLimit, then checks every id. A missing or archived
// asset fails the call before any write.
func prepareTagBatch(ctx context.Context, s *Server, in mcp.TagAssetsInput) ([]int64, []string, error) {
	ids := uniqueInt64(in.IDs)
	tags, err := uniqueTags(in.Tags)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) < 1 || len(ids) > mcp.TagBatchLimit {
		return nil, nil, fmt.Errorf("ids must contain 1 to %d assets", mcp.TagBatchLimit)
	}
	if len(tags) < 1 || len(tags) > mcp.TagBatchLimit {
		return nil, nil, fmt.Errorf("tags must contain 1 to %d values", mcp.TagBatchLimit)
	}
	if len(ids)*len(tags) > mcp.TagPairLimit {
		return nil, nil, fmt.Errorf("ids and tags together must be at most %d pairs", mcp.TagPairLimit)
	}
	for _, id := range ids {
		if err := s.enrichmentNode(ctx, id); err != nil {
			return nil, nil, err
		}
	}
	return ids, tags, nil
}

func uniqueInt64(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func uniqueTags(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		norm, err := normalizeTag(tag)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out, nil
}

func humaNotFound(err error) bool {
	var model *huma.ErrorModel
	return errors.As(err, &model) && model.Status == 404
}

func pageLimit(v, def, max int64) (int64, error) {
	if v == 0 {
		return def, nil
	}
	if v < 1 || v > max {
		return 0, fmt.Errorf("limit must be between 1 and %d", max)
	}
	return v, nil
}

func knownAssetSort(sort string) bool {
	switch sort {
	case "", "newest", "name_asc", "name_desc", "created_asc", "created_desc", "captured_asc", "captured_desc":
		return true
	default:
		return false
	}
}
