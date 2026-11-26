package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestListSimilarOrdersByHammingDistance(t *testing.T) {
	srv, database, patSvc, userSvc, _ := newMCPReadServer(t, true)
	ctx := context.Background()
	cookie, adminID := mintSessionForUser(t, database, userSvc, "similar-admin", true, "local")
	readToken, _, err := patSvc.Mint(ctx, adminID, "reader", []string{"mcp:read"}, 0)
	require.NoError(t, err)

	ids := seedSimilarNodes(t, database)
	handler := srv.Handler()

	body := getSimilar(t, handler, cookie, ids.source, "")
	require.Empty(t, body.Reason)
	require.Len(t, body.Assets, 4)
	assert.Equal(t, []int64{ids.exact, ids.hidden, ids.close, ids.mid}, similarIDs(body.Assets))
	assert.Equal(t, []int{0, 1, 4, 8}, similarDistances(body.Assets))

	limited := getSimilar(t, handler, cookie, ids.source, "?limit=2&maxDistance=12")
	require.Len(t, limited.Assets, 2)
	assert.Equal(t, []int64{ids.exact, ids.hidden}, similarIDs(limited.Assets))

	exactOnly := getSimilar(t, handler, cookie, ids.source, "?maxDistance=0")
	require.Len(t, exactOnly.Assets, 1)
	assert.Equal(t, ids.exact, exactOnly.Assets[0].ID)
	assert.Equal(t, 0, exactOnly.Assets[0].Distance)

	none := getSimilar(t, handler, cookie, ids.unhashed, "")
	assert.Empty(t, none.Assets)
	assert.Equal(t, noPerceptualHashReason, none.Reason)

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, withSessionCookie(http.MethodGet, "/api/v1/assets/999999/similar", cookie, nil))
	assert.Equal(t, http.StatusNotFound, missing.Code)

	archived := httptest.NewRecorder()
	handler.ServeHTTP(archived, withSessionCookie(http.MethodGet, "/api/v1/assets/"+strconv.FormatInt(ids.archived, 10)+"/similar", cookie, nil))
	assert.Equal(t, http.StatusNotFound, archived.Code, "body: %s", archived.Body.String())

	session := dialMCP(t, handler, readToken)
	res := callMCP(t, session, "find_similar", map[string]any{"id": ids.source, "limit": 20, "maxDistance": 12})
	require.False(t, res.IsError, "find_similar: %s", mcpText(res))
	assets := structuredSlice(t, res, "assets")
	require.Len(t, assets, 4)
	assert.Equal(t, float64(ids.exact), assets[0]["id"])
	assert.Equal(t, float64(0), assets[0]["distance"])
	assert.Equal(t, float64(ids.mid), assets[3]["id"])
	assert.Equal(t, float64(8), assets[3]["distance"])
	assert.Contains(t, mcpText(res), "4 assets")

	omitted := callMCP(t, session, "find_similar", map[string]any{"id": ids.source})
	require.False(t, omitted.IsError, "default find_similar: %s", mcpText(omitted))
	assert.Len(t, structuredSlice(t, omitted, "assets"), 4)
}

type similarIDsSet struct {
	source, exact, hidden, close, mid, unhashed, archived int64
}

func seedSimilarNodes(t *testing.T, database *db.DB) similarIDsSet {
	t.Helper()
	ctx := context.Background()
	var ids similarIDsSet
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "similar", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		insert := func(uuid, name, life string, phash sql.NullInt64) (int64, error) {
			node, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid: uuid, StorageLocationID: loc.ID,
				FilePath: "/" + name, FileName: name, FileExt: "jpg",
				IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: life,
				Phash: phash,
			})
			if err != nil {
				return 0, err
			}
			return node.ID, nil
		}
		hash := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
		if ids.source, err = insert("0198cccc-0000-7000-8000-000000000001", "source.jpg", "ACTIVE", hash(0)); err != nil {
			return err
		}
		if ids.exact, err = insert("0198cccc-0000-7000-8000-000000000002", "exact.jpg", "ACTIVE", hash(0)); err != nil {
			return err
		}
		if ids.hidden, err = insert("0198cccc-0000-7000-8000-000000000003", "hidden.jpg", "HIDDEN", hash(1)); err != nil {
			return err
		}
		if ids.close, err = insert("0198cccc-0000-7000-8000-000000000004", "close.jpg", "ACTIVE", hash(0xF)); err != nil {
			return err
		}
		if ids.mid, err = insert("0198cccc-0000-7000-8000-000000000005", "mid.jpg", "ACTIVE", hash(0xFF)); err != nil {
			return err
		}
		if _, err = insert("0198cccc-0000-7000-8000-000000000006", "far.jpg", "ACTIVE", hash(0xFFFFFFFF)); err != nil {
			return err
		}
		if _, err = insert("0198cccc-0000-7000-8000-000000000007", "trashed.jpg", "TRASHED", hash(1)); err != nil {
			return err
		}
		if _, err = insert("0198cccc-0000-7000-8000-000000000008", "missing.jpg", "MISSING", hash(1)); err != nil {
			return err
		}
		ids.unhashed, err = insert("0198cccc-0000-7000-8000-000000000009", "unhashed.jpg", "ACTIVE", sql.NullInt64{})
		if err != nil {
			return err
		}
		ids.archived, err = insert("0198cccc-0000-7000-8000-00000000000a", "archived.jpg", "ARCHIVED", hash(0))
		return err
	})
	require.NoError(t, err)
	return ids
}

type similarBody struct {
	Assets []similarAssetDTO `json:"assets"`
	Reason string            `json:"reason"`
}

func getSimilar(t *testing.T, handler http.Handler, cookie string, id int64, query string) similarBody {
	t.Helper()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/assets/"+strconv.FormatInt(id, 10)+"/similar"+query, cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var body similarBody
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	return body
}

func similarIDs(assets []similarAssetDTO) []int64 {
	out := make([]int64, len(assets))
	for i, a := range assets {
		out[i] = a.ID
	}
	return out
}

func similarDistances(assets []similarAssetDTO) []int {
	out := make([]int, len(assets))
	for i, a := range assets {
		out[i] = a.Distance
	}
	return out
}

func TestSimilarBoundsMatchHandlerTags(t *testing.T) {
	typ := reflect.TypeOf(SimilarAssetsInput{})
	limit, ok := typ.FieldByName("Limit")
	require.True(t, ok)
	dist, ok := typ.FieldByName("MaxDistance")
	require.True(t, ok)
	assert.Equal(t, strconv.Itoa(SimilarDefaultLimit), limit.Tag.Get("default"))
	assert.Equal(t, strconv.Itoa(SimilarMaxLimit), limit.Tag.Get("maximum"))
	assert.Equal(t, strconv.Itoa(SimilarDefaultDistance), dist.Tag.Get("default"))
	assert.Equal(t, strconv.Itoa(SimilarMaxDistance), dist.Tag.Get("maximum"))
}
