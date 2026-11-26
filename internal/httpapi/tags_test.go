package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestAssetTagsAndAnnotations(t *testing.T) {
	srv, database, patSvc, _ := newMCPReviewServer(t)
	ctx := context.Background()
	handler := srv.Handler()
	userSvc := srv.localAuth.users

	adminCookie, adminID := mintSessionForUser(t, database, userSvc, "admin", true, "local")
	userCookie, bobID := mintSessionForUser(t, database, userSvc, "bob", false, "local")
	enrichToken, enrichPAT, err := patSvc.Mint(ctx, bobID, "enrich", []string{"mcp:enrich"}, 0)
	require.NoError(t, err)
	otherToken, otherPAT, err := patSvc.Mint(ctx, bobID, "enrich-b", []string{"mcp:enrich"}, 0)
	require.NoError(t, err)
	readToken, _, err := patSvc.Mint(ctx, bobID, "read", []string{"mcp:read"}, 0)
	require.NoError(t, err)

	var nodeID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "tags", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		node, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "0198dddd-0000-7000-8000-000000000001", StorageLocationID: loc.ID,
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

	add := withSessionCookie(http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), userCookie,
		[]byte(`{"tag":"ZebraStripe","source":"assistant"}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, add)
	require.Equal(t, http.StatusOK, rr.Code, "add tag body: %s", rr.Body.String())
	var added tagDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &added))
	assert.Equal(t, "zebrastripe", added.Tag)
	assert.Equal(t, "user", added.Source, "a session must not honor a body source")

	desc := withSessionCookie(http.MethodPut, fmt.Sprintf("/api/v1/assets/%d/annotations/description", nodeID), userCookie,
		[]byte(`{"text":"cobalt lantern","source":"assistant"}`))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, desc)
	require.Equal(t, http.StatusOK, rr.Code, "description body: %s", rr.Body.String())
	var note annotationDTO
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &note))
	assert.Equal(t, "user", note.Source)
	assert.Equal(t, "cobalt lantern", note.Text)

	listed := getJSON[struct {
		Tags []tagDTO `json:"tags"`
	}](t, handler, withSessionCookie(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), userCookie, nil))
	require.Len(t, listed.Tags, 1)
	assert.Equal(t, "zebrastripe", listed.Tags[0].Tag)

	byTag := getJSON[struct {
		Assets []assetDTO `json:"assets"`
		Total  int64      `json:"total"`
	}](t, handler, withSessionCookie(http.MethodGet, "/api/v1/assets?tag=ZebraStripe", userCookie, nil))
	require.Len(t, byTag.Assets, 1)
	assert.Equal(t, nodeID, byTag.Assets[0].ID)
	assert.Equal(t, int64(1), byTag.Total)

	byQ := getJSON[struct {
		Assets []assetDTO `json:"assets"`
	}](t, handler, withSessionCookie(http.MethodGet, "/api/v1/assets?q=zebrastripe", userCookie, nil))
	require.Len(t, byQ.Assets, 1)
	byDesc := getJSON[struct {
		Assets []assetDTO `json:"assets"`
	}](t, handler, withSessionCookie(http.MethodGet, "/api/v1/assets?q=lantern", userCookie, nil))
	require.Len(t, byDesc.Assets, 1)

	patAdd := bearerJSON(http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), enrichToken, `{"tag":"nightbeacon"}`)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, patAdd)
	require.Equal(t, http.StatusOK, rr.Code, "pat tag body: %s", rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &added))
	assert.Equal(t, "assistant", added.Source)
	assert.Equal(t, enrichPAT.ID, *added.PatID)

	patDesc := bearerJSON(http.MethodPut, fmt.Sprintf("/api/v1/assets/%d/annotations/description", nodeID), enrichToken, `{"text":"night beacon","model":"test"}`)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, patDesc)
	require.Equal(t, http.StatusOK, rr.Code, "pat description body: %s", rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &note))
	assert.Equal(t, "assistant", note.Source)

	otherAdd := bearerJSON(http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), otherToken, `{"tag":"otherassistant"}`)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, otherAdd)
	require.Equal(t, http.StatusOK, rr.Code, "other pat tag body: %s", rr.Body.String())

	readPost := bearerJSON(http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), readToken, `{"tag":"nope"}`)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, readPost)
	assert.Equal(t, http.StatusForbidden, rr.Code, "mcp:read must not write tags, body: %s", rr.Body.String())

	revertDenied := withSessionCookie(http.MethodPost, "/api/v1/tags/revert", userCookie, []byte(`{"source":"assistant"}`))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, revertDenied)
	assert.Equal(t, http.StatusForbidden, rr.Code, "non-admin revert body: %s", rr.Body.String())

	revert := withSessionCookie(http.MethodPost, "/api/v1/tags/revert", adminCookie,
		[]byte(fmt.Sprintf(`{"source":"assistant","patId":%d}`, enrichPAT.ID)))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, revert)
	require.Equal(t, http.StatusOK, rr.Code, "revert body: %s", rr.Body.String())
	var reverted struct {
		TagsRemoved        int64 `json:"tagsRemoved"`
		AnnotationsRemoved int64 `json:"annotationsRemoved"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &reverted))
	assert.Equal(t, int64(1), reverted.TagsRemoved)
	assert.Equal(t, int64(1), reverted.AnnotationsRemoved)

	left := getJSON[struct {
		Tags []tagDTO `json:"tags"`
	}](t, handler, withSessionCookie(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/tags", nodeID), adminCookie, nil))
	names := map[string]string{}
	for _, tag := range left.Tags {
		names[tag.Tag] = tag.Source
	}
	assert.Equal(t, map[string]string{"zebrastripe": "user", "otherassistant": "assistant"}, names)

	ann := getJSON[struct {
		Annotations []annotationDTO `json:"annotations"`
	}](t, handler, withSessionCookie(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/annotations", nodeID), adminCookie, nil))
	require.Len(t, ann.Annotations, 1)
	assert.Equal(t, "user", ann.Annotations[0].Source)

	gone := getJSON[struct {
		Assets []assetDTO `json:"assets"`
	}](t, handler, withSessionCookie(http.MethodGet, "/api/v1/assets?q=beacon", adminCookie, nil))
	assert.Empty(t, gone.Assets)

	var auditRows []sqlcgen.ListActorAuditRow
	err = database.ReadTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		auditRows, err = q.ListActorAudit(ctx, sqlcgen.ListActorAuditParams{
			Limit: 50, Event: "tag.added",
		})
		return err
	})
	require.NoError(t, err)
	var localAdds, patAdds int
	for _, row := range auditRows {
		if row.AuthProvider.String == "local" && !row.PatID.Valid {
			localAdds++
		}
		if row.AuthProvider.String == "pat" && row.PatID.Valid && row.PatID.Int64 == enrichPAT.ID {
			patAdds++
		}
	}
	assert.Equal(t, 1, localAdds)
	assert.Equal(t, 1, patAdds)

	counts := getJSON[struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Count int64  `json:"count"`
		} `json:"tags"`
	}](t, handler, withSessionCookie(http.MethodGet, "/api/v1/tags", adminCookie, nil))
	assert.Len(t, counts.Tags, 2)

	del := withSessionCookie(http.MethodDelete, fmt.Sprintf("/api/v1/assets/%d/tags/zebrastripe", nodeID), userCookie, nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, del)
	require.Equal(t, http.StatusOK, rr.Code, "delete body: %s", rr.Body.String())

	_ = adminID
	_ = otherPAT
}

func bearerJSON(method, path, token, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func getJSON[T any](t *testing.T, handler http.Handler, req *http.Request) T {
	t.Helper()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out T
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}
