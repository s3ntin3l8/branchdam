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

func registerTools(s *sdkmcp.Server, b Backend) {
	sdkmcp.AddTool(s, tool("search_assets", "Search assets. Inputs match the asset list: q, cameraModel, graphStatus, lifecycleState, storageLocationId, unlinkedOnly, uploadedByUserId, sort, limit, offset. Offset pagination, no cursor."),
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

	sdkmcp.AddTool(s, tool("get_asset", "Fetch one asset: detail, extracted metadata and sync status together."),
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
}
