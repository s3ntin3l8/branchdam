package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestAgentSourceStatus_Auth(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	validHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// 1. Missing auth -> 401
	reqNoAuth := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+validHash, nil)
	recNoAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNoAuth, reqNoAuth)
	assert.Equal(t, http.StatusUnauthorized, recNoAuth.Code)

	// 2. User auth header alone on agent route without X-API-Key -> 401 (AgentChain strips X-Authentik headers)
	reqUser := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+validHash, nil)
	reqUser.Header.Set("X-Authentik-Username", "operator")
	recUser := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recUser, reqUser)
	assert.Equal(t, http.StatusUnauthorized, recUser.Code)

	// 3. Machine auth (X-API-Key) -> 200
	reqMachine := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+validHash, nil)
	reqMachine.Header.Set("X-API-Key", routeTestAgentKey)
	recMachine := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMachine, reqMachine)
	assert.Equal(t, http.StatusOK, recMachine.Code)
}

func TestAgentSourceStatus_Validation(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	// 1. Missing sourcePath query param -> 400
	reqMissing := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status", nil)
	reqMissing.Header.Set("X-API-Key", routeTestAgentKey)
	recMissing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMissing, reqMissing)
	assert.Equal(t, http.StatusBadRequest, recMissing.Code)

	// 2. Malformed sourcePath (not 64 hex chars) -> 422 or 400
	reqBad := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath=not-a-valid-sha256", nil)
	reqBad.Header.Set("X-API-Key", routeTestAgentKey)
	recBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBad, reqBad)
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, recBad.Code)

	// 3. Malformed sourcePathHash alias (not 64 hex chars) -> 422 or 400
	reqBadAlias := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePathHash=not-a-valid-sha256", nil)
	reqBadAlias.Header.Set("X-API-Key", routeTestAgentKey)
	recBadAlias := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBadAlias, reqBadAlias)
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, recBadAlias.Code)

	// 4. Conflicting sourcePath and sourcePathHash parameters -> 400
	hashA := "1111222233334444555566667777888899990000111122223333444455556666"
	hashB := "9999888877776666555544443333222211110000999988887777666655554444"
	reqConflict := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+hashA+"&sourcePathHash="+hashB, nil)
	reqConflict.Header.Set("X-API-Key", routeTestAgentKey)
	recConflict := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recConflict, reqConflict)
	assert.Equal(t, http.StatusBadRequest, recConflict.Code)

	// 5. Matching sourcePath and sourcePathHash parameters -> 200 (not found)
	reqMatching := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+hashA+"&sourcePathHash="+hashA, nil)
	reqMatching.Header.Set("X-API-Key", routeTestAgentKey)
	recMatching := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMatching, reqMatching)
	assert.Equal(t, http.StatusOK, recMatching.Code)
}

func TestAgentSourceStatus_NotFound(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath=1111222233334444555566667777888899990000111122223333444455556666", nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var res AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.False(t, res.Tracked)
	assert.Empty(t, res.NodeUUID)
}

func TestAgentSourceStatus_Found(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	sourceHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
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
			SourcePathHash:    &sourceHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	require.NoError(t, err)

	// 1. Found by sourcePath
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+sourceHash, nil)
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Code)

	var res1 AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &res1))
	assert.True(t, res1.Tracked)
	assert.Equal(t, nodeUUID, res1.NodeUUID)
	assert.Equal(t, "/tmp/test_archive/photo.raw", res1.FilePath)
	assert.Equal(t, "ACTIVE", res1.LifecycleState)
	assert.Equal(t, "INDEXED_FULL", res1.IndexingStatus)

	// 2. Found by sourcePathHash alias
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePathHash="+sourceHash, nil)
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)

	var res2 AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &res2))
	assert.True(t, res2.Tracked)
	assert.Equal(t, nodeUUID, res2.NodeUUID)
}

func TestAgentSourceStatus_ArchivedAndMissingNotMatched(t *testing.T) {
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

		// Archived node with source_path_hash
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "018d3b2f-7630-7e50-9844-3d96e95924aa",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive2/archived.raw",
			FileName:          "archived.raw",
			FileExt:           "raw",
			SourcePathHash:    &archivedHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ARCHIVED",
		})
		if err != nil {
			return err
		}

		// Missing node with source_path_hash
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "018d3b2f-7630-7e50-9844-3d96e95924bb",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive2/missing.raw",
			FileName:          "missing.raw",
			FileExt:           "raw",
			SourcePathHash:    &missingHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "MISSING",
		})
		return err
	})
	require.NoError(t, err)

	// Query for archived node source path hash -> tracked: false
	reqArchived := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+archivedHash, nil)
	reqArchived.Header.Set("X-API-Key", routeTestAgentKey)
	recArchived := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recArchived, reqArchived)
	assert.Equal(t, http.StatusOK, recArchived.Code)

	var resArchived AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(recArchived.Body.Bytes(), &resArchived))
	assert.False(t, resArchived.Tracked)

	// Query for missing node source path hash -> tracked: false
	reqMissing := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+missingHash, nil)
	reqMissing.Header.Set("X-API-Key", routeTestAgentKey)
	recMissing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMissing, reqMissing)
	assert.Equal(t, http.StatusOK, recMissing.Code)

	var resMissing AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(recMissing.Body.Bytes(), &resMissing))
	assert.False(t, resMissing.Tracked)
}

func TestAgentSourceStatus_MultipleActiveRowsDeterministic(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	sharedHash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	olderUUID := "018d3b2f-7630-7e50-9844-3d96e95924c1"
	newerUUID := "018d3b2f-7630-7e50-9844-3d96e95924c2"

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		sl, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "TestArchive3",
			RootPath: "/tmp/test_archive3",
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}

		// First node (lower id)
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          olderUUID,
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive3/photo_older.raw",
			FileName:          "photo_older.raw",
			FileExt:           "raw",
			SourcePathHash:    &sharedHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		if err != nil {
			return err
		}

		// Second node (higher id)
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          newerUUID,
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_archive3/photo_newer.raw",
			FileName:          "photo_newer.raw",
			FileExt:           "raw",
			SourcePathHash:    &sharedHash,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+sharedHash, nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var res AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.True(t, res.Tracked)
	// Must deterministically return the newer row (ORDER BY id DESC)
	assert.Equal(t, newerUUID, res.NodeUUID)
	assert.Equal(t, "/tmp/test_archive3/photo_newer.raw", res.FilePath)
}

func TestAgentUpload_SourcePathHash(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	// Ensure writable archive storage location exists
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: t.TempDir(),
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	sourceHash := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", strings.NewReader("dummy upload image content for source status test"))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "camera_shot.jpg")
	req.Header.Set("X-Source-Path-Hash", sourceHash)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code)

	var uploadResp AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &uploadResp))
	assert.NotEmpty(t, uploadResp.NodeUUID)

	// Verify GET /api/v1/agent/source-status tracks the uploaded node
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/agent/source-status?sourcePath="+sourceHash, nil)
	statusReq.Header.Set("X-API-Key", routeTestAgentKey)
	statusRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(statusRec, statusReq)
	assert.Equal(t, http.StatusOK, statusRec.Code)

	var statusRes AgentSourceStatusResult
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &statusRes))
	assert.True(t, statusRes.Tracked)
	assert.Equal(t, uploadResp.NodeUUID, statusRes.NodeUUID)
}
