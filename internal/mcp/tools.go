package mcp

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type idIn struct {
	ID int64 `json:"id" jsonschema:"asset id"`
}

type lineageIn struct {
	ID    int64 `json:"id" jsonschema:"asset id"`
	Depth int   `json:"depth,omitempty" jsonschema:"traversal depth from 1 to 5; omitted means 2"`
}

type reviewIn struct {
	Limit    int64 `json:"limit,omitempty" jsonschema:"page size from 1 to 500; omitted means 50"`
	BeforeID int64 `json:"beforeId,omitempty" jsonschema:"keyset cursor; omitted means the first page"`
}

type jobsIn struct {
	Kind  string `json:"kind,omitempty" jsonschema:"job kind filter; blank means every kind"`
	State string `json:"state,omitempty" jsonschema:"job state filter; blank means every state"`
}

type edgeIDIn struct {
	ID int64 `json:"id" jsonschema:"edge id"`
}

type createEdgeIn struct {
	SourceNodeID     int64  `json:"sourceNodeId" jsonschema:"parent asset id"`
	TargetNodeID     int64  `json:"targetNodeId" jsonschema:"child asset id"`
	RelationshipType string `json:"relationshipType" jsonschema:"DERIVED_FROM, FINAL_EXPORT, PROXY_OF, PROJECT_SIDECAR or DUPLICATE_OF"`
}

type viewOut struct {
	Ready    bool   `json:"ready"`
	MIMEType string `json:"mimeType,omitempty"`
}

func tool(name, description string) *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name:        name,
		Description: description + untrustedNote,
		Annotations: &sdkmcp.ToolAnnotations{
			ReadOnlyHint: true,
			Title:        name,
		},
	}
}

func writeTool(name, description string) *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name:        name,
		Description: description + " Acts only on the explicit id arguments." + untrustedNote,
		Annotations: &sdkmcp.ToolAnnotations{
			ReadOnlyHint: false,
			Title:        name,
		},
	}
}

func registerTools(s *sdkmcp.Server, b Backend) {
	sdkmcp.AddTool(s, tool("search_assets", "Search assets. Inputs match the asset list: q, cameraModel, graphStatus, lifecycleState, storageLocationId, unlinkedOnly, uploadedByUserId, sort, tag, limit, offset. Offset pagination, no cursor."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in SearchAssetsInput) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.SearchAssets(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "assets", "asset", "assets", "search_assets")), body, nil
		})

	sdkmcp.AddTool(s, tool("list_facets", "List asset filter values, including camera models."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.ListFacets(ctx)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "cameraModels", "camera model", "camera models", "list_facets")), body, nil
		})

	sdkmcp.AddTool(s, tool("get_asset", "Fetch one asset: detail, extracted metadata, sync status, tags and annotations. Tags and annotations include source (user or assistant)."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in idIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.GetAsset(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("asset %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, tool("find_similar", "Find live assets that look like this one, nearest perceptual-hash Hamming distance first. limit is 1 to 100 (omitted means 20). maxDistance is 0 to 64 (omitted means 12). A source with no hash returns an empty list and reason no_perceptual_hash."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in FindSimilarInput) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.FindSimilar(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "assets", "asset", "assets", "find_similar")), body, nil
		})

	sdkmcp.AddTool(s, tool("get_lineage", "Bounded lineage around one asset. depth is 1 to 5. The body is rootId, nodes and edges."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in lineageIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.GetLineage(ctx, in.ID, in.Depth)
			if err != nil {
				return nil, nil, err
			}
			nodes, _ := sliceLen(body, "nodes")
			edges, _ := sliceLen(body, "edges")
			return textResult(counted(nodes, "node", "nodes") + ", " + counted(edges, "edge", "edges")), body, nil
		})

	sdkmcp.AddTool(s, tool("view_asset", "Return the cached JPEG thumbnail for one asset. When the thumbnail has not been generated yet, the result is the text \"thumbnail not ready yet\" and is not an error."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in idIn) (*sdkmcp.CallToolResult, viewOut, error) {
			if err := requireRead(ctx); err != nil {
				return nil, viewOut{}, err
			}
			jpeg, ready, err := b.ViewAsset(ctx, in.ID)
			if err != nil {
				return nil, viewOut{}, err
			}
			if !ready {
				return textResult("thumbnail not ready yet"), viewOut{Ready: false}, nil
			}
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{
					&sdkmcp.TextContent{Text: "jpeg thumbnail"},
					&sdkmcp.ImageContent{MIMEType: "image/jpeg", Data: jpeg},
				},
			}, viewOut{Ready: true, MIMEType: "image/jpeg"}, nil
		})

	sdkmcp.AddTool(s, tool("list_review_queue", "List edges waiting for review, including confidence, tier, resolver, evidence and both endpoints' file names. beforeId is a keyset cursor."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in reviewIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.ListReviewQueue(ctx, in.Limit, in.BeforeID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "entries", "review edge", "review edges", "list_review_queue")), body, nil
		})

	sdkmcp.AddTool(s, tool("list_jobs", "List scan jobs, optionally filtered by kind and state."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in jobsIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.ListJobs(ctx, in.Kind, in.State)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "jobs", "job", "jobs", "list_jobs")), body, nil
		})

	sdkmcp.AddTool(s, tool("get_scan_progress", "Recent scan jobs and their progress counters."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.GetScanProgress(ctx)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "jobs", "job", "jobs", "get_scan_progress")), body, nil
		})

	sdkmcp.AddTool(s, tool("storage_health", "Storage location health and agent scratch telemetry."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.StorageHealth(ctx)
			if err != nil {
				return nil, nil, err
			}
			return textResult("storage health"), body, nil
		})

	sdkmcp.AddTool(s, writeTool("confirm_edge", "Confirm one edge. The edge becomes CONFIRMED with review_source assistant, which is a weaker signal than a human confirm."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in edgeIDIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.ConfirmEdge(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("confirmed edge %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, writeTool("reject_edge", "Reject one edge. The edge becomes REJECTED with review_source assistant."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in edgeIDIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.RejectEdge(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("rejected edge %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, writeTool("reset_edge_review", "Return one CONFIRMED or REJECTED edge to NEEDS_REVIEW and clear its review source."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in edgeIDIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.ResetEdgeReview(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("reset edge %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, writeTool("create_edge", "Create a manual edge between two assets. Uses the same validation as creating an edge in the web UI, and stamps review_source assistant."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in createEdgeIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.CreateEdge(ctx, in.SourceNodeID, in.TargetNodeID, in.RelationshipType)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("created edge %d to %d", in.SourceNodeID, in.TargetNodeID)), body, nil
		})

	sdkmcp.AddTool(s, writeTool("trash_asset", "Move one asset to the trash. Linked exports are trashed with it."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in idIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.TrashAsset(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("trashed asset %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, writeTool("restore_asset", "Restore one trashed or archived asset."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in idIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.RestoreAsset(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("restored asset %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, writeTool("retry_sync", "Requeue failed remote sync rows for one asset."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in idIn) (*sdkmcp.CallToolResult, any, error) {
			if err := requireReview(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.RetrySync(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("retried sync for asset %d", in.ID)), body, nil
		})

	sdkmcp.AddTool(s, tool("list_tags", "List tags with counts. Counts cover live assets only (not archived, trashed or superseded)."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			if err := requireRead(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.ListTags(ctx)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "tags", "tag", "tags", "list_tags")), body, nil
		})

	sdkmcp.AddTool(s, writeTool("tag_assets", "Add tags to assets. At most 100 ids, 100 tags and 1000 id-tag pairs. source is assistant. A tag a person already owns is left unchanged."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in TagAssetsInput) (*sdkmcp.CallToolResult, any, error) {
			if err := requireEnrich(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.TagAssets(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return textResult(countSummary(body, "tags", "tag", "tags", "tag_assets")), body, nil
		})

	sdkmcp.AddTool(s, writeTool("untag_assets", "Remove assistant tags from assets. At most 100 ids, 100 tags and 1000 id-tag pairs. A user tag is not removed."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in TagAssetsInput) (*sdkmcp.CallToolResult, any, error) {
			if err := requireEnrich(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.UntagAssets(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			removed, _ := sliceLen(body, "removed")
			skipped, _ := sliceLen(body, "skipped")
			return textResult(counted(removed, "tag", "tags") + " removed, " + counted(skipped, "tag", "tags") + " skipped"), body, nil
		})

	sdkmcp.AddTool(s, writeTool("set_description", "Set the assistant description on one asset. Replaces the previous assistant description. A user description is left unchanged."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in SetDescriptionInput) (*sdkmcp.CallToolResult, any, error) {
			if err := requireEnrich(ctx); err != nil {
				return nil, nil, err
			}
			body, err := b.SetDescription(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			return textResult("description set"), body, nil
		})
}
