package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestAgentCheckContent_Auth(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	// 1. Missing auth -> 401
	reqNoAuth := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", nil)
	recNoAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNoAuth, reqNoAuth)
	assert.Equal(t, http.StatusUnauthorized, recNoAuth.Code)

	// 2. User auth header alone on agent route without X-API-Key -> 401 (AgentChain strips X-Authentik headers)
	reqUser := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", nil)
	reqUser.Header.Set("X-Authentik-Username", "operator")
	recUser := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recUser, reqUser)
	assert.Equal(t, http.StatusUnauthorized, recUser.Code)

	// 3. Machine auth (X-API-Key) -> 200
	reqMachine := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", nil)
	reqMachine.Header.Set("X-API-Key", routeTestAgentKey)
	recMachine := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMachine, reqMachine)
	assert.Equal(t, http.StatusOK, recMachine.Code)
}

func TestAgentCheckContent_Validation(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	// Missing fullHash query param
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content", nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAgentCheckContent_NotFound(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash=1111222233334444555566667777888899990000111122223333444455556666", nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var res AgentCheckContentResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.False(t, res.Found)
	assert.Empty(t, res.NodeUUID)
}

func TestAgentCheckContent_FoundAndShortCircuit(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	fastHash := "0123456789abcdef"
	fullHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	nodeUUID := "018d3b2f-7630-7e50-9844-3d96e9592499"

	var sl sqlcgen.StorageLocation
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		sl, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "TestArchive",
			RootPath: "/tmp/test_archive",
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}

		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          nodeUUID,
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive/photo.raw",
			FileName:          "photo.raw",
			FileExt:           "raw",
			FastHash:          &fastHash,
			FullHash:          &fullHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	require.NoError(t, err)

	// 1. Found by fullHash only
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash="+fullHash, nil)
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Code)

	var res1 AgentCheckContentResult
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &res1))
	assert.True(t, res1.Found)
	assert.Equal(t, nodeUUID, res1.NodeUUID)
	assert.Equal(t, "/tmp/test_archive/photo.raw", res1.FilePath)
	assert.Equal(t, "ACTIVE", res1.LifecycleState)
	assert.Equal(t, "INDEXED_FULL", res1.IndexingStatus)

	// 2. Found by matching fastHash and fullHash
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fastHash="+fastHash+"&fullHash="+fullHash, nil)
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)

	var res2 AgentCheckContentResult
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &res2))
	assert.True(t, res2.Found)
	assert.Equal(t, nodeUUID, res2.NodeUUID)

	// 3. Non-existent fastHash short-circuits to found=false even if fullHash exists
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fastHash=fedcba9876543210&fullHash="+fullHash, nil)
	req3.Header.Set("X-API-Key", routeTestAgentKey)
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	assert.Equal(t, http.StatusOK, rec3.Code)

	var res3 AgentCheckContentResult
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &res3))
	assert.False(t, res3.Found)
	assert.Empty(t, res3.NodeUUID)
}

func TestAgentCheckContent_ArchivedAndMissingNotMatched(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	archivedHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	missingHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		sl, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "TestArchive",
			RootPath: "/tmp/test_archive2",
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}

		// Insert ARCHIVED node
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "018d3b2f-7630-7e50-9844-3d96e9592401",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive2/archived.jpg",
			FileName:          "archived.jpg",
			FileExt:           "jpg",
			FullHash:          &archivedHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ARCHIVED",
		})
		if err != nil {
			return err
		}

		// Insert MISSING node
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "018d3b2f-7630-7e50-9844-3d96e9592402",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive2/missing.jpg",
			FileName:          "missing.jpg",
			FileExt:           "jpg",
			FullHash:          &missingHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "MISSING",
		})
		return err
	})
	require.NoError(t, err)

	// Query for ARCHIVED node -> found: false
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash="+archivedHash, nil)
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Code)

	var res1 AgentCheckContentResult
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &res1))
	assert.False(t, res1.Found)

	// Query for MISSING node -> found: false
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/check-content?fullHash="+missingHash, nil)
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)

	var res2 AgentCheckContentResult
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &res2))
	assert.False(t, res2.Found)
}
