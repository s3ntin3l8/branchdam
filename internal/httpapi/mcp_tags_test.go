package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestMCPTagTools(t *testing.T) {
	srv, database, patSvc, _ := newMCPReviewServer(t)
	ctx := context.Background()
	handler := srv.Handler()
	_, bobID := mintSessionForUser(t, database, srv.localAuth.users, "bob", false, "local")
	userCookie, _ := mintSessionForUser(t, database, srv.localAuth.users, "ada", false, "local")
	enrichToken, _, err := patSvc.Mint(ctx, bobID, "enrich", []string{"mcp:enrich"}, 0)
	require.NoError(t, err)
	readToken, _, err := patSvc.Mint(ctx, bobID, "read", []string{"mcp:read"}, 0)
	require.NoError(t, err)

	var nodeID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "mcp-tags", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		node, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "0198eeee-0000-7000-8000-000000000001", StorageLocationID: loc.ID,
			FilePath: "/archive/plain.jpg", FileName: "plain.jpg", FileExt: "jpg",
			IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		nodeID = node.ID
		return nil
	})
	require.NoError(t, err)

	add := withSessionCookie(http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), userCookie, []byte(`{"tag":"kept"}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, add)
	require.Equal(t, http.StatusOK, rr.Code, "user tag: %s", rr.Body.String())

	enrich := dialMCP(t, handler, enrichToken)
	read := dialMCP(t, handler, readToken)

	denied := callMCP(t, read, "tag_assets", map[string]any{"ids": []any{nodeID}, "tags": []any{"night"}})
	assert.True(t, denied.IsError)
	assert.Contains(t, mcpText(denied), "mcp:enrich")

	tagged := callMCP(t, enrich, "tag_assets", map[string]any{"ids": []any{nodeID}, "tags": []any{"night"}})
	require.False(t, tagged.IsError, mcpText(tagged))
	rows := structuredSlice(t, tagged, "tags")
	require.Len(t, rows, 1)
	assert.Equal(t, "night", rows[0]["tag"])
	assert.Equal(t, "assistant", rows[0]["source"])

	desc := callMCP(t, enrich, "set_description", map[string]any{"id": nodeID, "text": "night beacon", "model": "test"})
	require.False(t, desc.IsError, mcpText(desc))
	assert.Equal(t, "assistant", structuredMap(t, desc)["source"])

	untag := callMCP(t, enrich, "untag_assets", map[string]any{
		"ids": []any{nodeID}, "tags": []any{"kept", "night"},
	})
	require.False(t, untag.IsError, mcpText(untag))
	removed := structuredSlice(t, untag, "removed")
	require.Len(t, removed, 1)
	skipped := structuredSlice(t, untag, "skipped")
	require.Len(t, skipped, 1)
	assert.Equal(t, "kept", skipped[0]["tag"])
	assert.Equal(t, "not_assistant", skipped[0]["reason"])

	detail := callMCP(t, read, "get_asset", map[string]any{"id": nodeID})
	require.False(t, detail.IsError, mcpText(detail))
	body := structuredMap(t, detail)
	assetTags := body["tags"].([]any)
	require.Len(t, assetTags, 1)
	assert.Equal(t, "kept", assetTags[0].(map[string]any)["tag"])
	assert.Equal(t, "user", assetTags[0].(map[string]any)["source"])
	notes := body["annotations"].([]any)
	require.Len(t, notes, 1)
	assert.Equal(t, "assistant", notes[0].(map[string]any)["source"])
	assert.Equal(t, "night beacon", notes[0].(map[string]any)["text"])

	listed := callMCP(t, read, "list_tags", map[string]any{})
	require.False(t, listed.IsError, mcpText(listed))
	assert.Equal(t, "kept", structuredSlice(t, listed, "tags")[0]["tag"])

	noRead := callMCP(t, enrich, "list_tags", map[string]any{})
	assert.True(t, noRead.IsError)
	assert.Contains(t, mcpText(noRead), "mcp:read")

	ids := make([]any, 101)
	for i := range ids {
		ids[i] = nodeID
	}
	tooMany := callMCP(t, enrich, "tag_assets", map[string]any{"ids": ids, "tags": []any{"x"}})
	assert.True(t, tooMany.IsError, mcpText(tooMany))
}
