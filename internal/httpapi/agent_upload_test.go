package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestAgentUploadStreaming(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	locDir := filepath.Join(tmpDir, "tier1")
	require.NoError(t, os.MkdirAll(locDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "Tier1_Upload",
			RootPath: locDir,
			Tier:     "TIER1_LOCAL_SCRATCH",
			ReadOnly: 0,
			Prunable: 1,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()

	data := []byte("Hello, branchDAM streaming agent upload test bytes!")
	hasher := blake3.New()
	_, err = hasher.Write(data)
	require.NoError(t, err)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "PXL_20260829_001.dng")
	req.Header.Set("X-Blake3-Hash", expectedHash)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp AgentUploadResponse
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "UPLOADED", resp.Status)
	assert.Equal(t, int64(len(data)), resp.BytesWritten)
	assert.Equal(t, expectedHash, resp.Blake3Hash)
	assert.NotEmpty(t, resp.NodeUUID)

	// Verify node in database
	node, err := database.Reader.GetMediaNodeByUUID(context.Background(), resp.NodeUUID)
	require.NoError(t, err)
	assert.Equal(t, "INDEXED_SHALLOW", node.IndexingStatus)
	assert.Equal(t, expectedHash, *node.FullHash)
}

func TestAgentUploadNoWritableLocation(t *testing.T) {
	srv, _ := fullTestServer(t)

	handler := srv.Handler()

	data := []byte("sample payload")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "test.jpg")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestAgentUploadChecksumMismatch(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	locDir := filepath.Join(tmpDir, "tier1")
	require.NoError(t, os.MkdirAll(locDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "Tier1_Upload",
			RootPath: locDir,
			Tier:     "TIER1_LOCAL_SCRATCH",
			ReadOnly: 0,
			Prunable: 1,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()

	data := []byte("sample payload")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "test.jpg")
	req.Header.Set("X-Blake3-Hash", "0000000000000000000000000000000000000000000000000000000000000000")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAgentUploadAuthForbidden(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader([]byte("unauthed")))
	req.Header.Set("X-API-Key", "wrong-key")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAgentUploadMasterArchiveAndHardlink(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	exportsDir := filepath.Join(tmpDir, "exports")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.MkdirAll(exportsDir, 0o755))

	var (
		archiveLoc sqlcgen.StorageLocation
		exportsLoc sqlcgen.StorageLocation
	)
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		archiveLoc, err = q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}
		exportsLoc, err = q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "Exports",
			RootPath: exportsDir,
			Tier:     "TIER2_EXPORTS",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()

	data := []byte("JPEG image binary content for testing standalone upload")
	hasher := blake3.New()
	_, err = hasher.Write(data)
	require.NoError(t, err)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "IMG_2026.JPG")
	req.Header.Set("X-Camera-Model", "Pixel 9 Pro")
	req.Header.Set("X-Capture-Timestamp", "1787998200") // 2026-08-29
	req.Header.Set("X-Blake3-Hash", expectedHash)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp AgentUploadResponse
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "UPLOADED", resp.Status)
	assert.Equal(t, expectedHash, resp.Blake3Hash)
	assert.NotEmpty(t, resp.RelativePath)
	assert.Contains(t, resp.RelativePath, "Pixel 9 Pro")
	assert.Contains(t, resp.RelativePath, "IMG_2026.JPG")

	// Verify file exists on archive disk
	archiveFile := filepath.Join(archiveDir, resp.RelativePath)
	content, err := os.ReadFile(archiveFile)
	require.NoError(t, err)
	assert.Equal(t, data, content)

	// Verify hardlink exists in exports/immich/
	exportFile := filepath.Join(exportsDir, "immich", resp.RelativePath)
	exportContent, err := os.ReadFile(exportFile)
	require.NoError(t, err)
	assert.Equal(t, data, exportContent)

	// Verify both nodes and edge in database
	archiveNode, err := database.Reader.GetMediaNodeByUUID(context.Background(), resp.NodeUUID)
	require.NoError(t, err)
	assert.Equal(t, archiveLoc.ID, archiveNode.StorageLocationID)

	edges, err := database.Reader.ListEdgesBySource(context.Background(), archiveNode.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, "FINAL_EXPORT", edges[0].RelationshipType)
	assert.Equal(t, "immich_export", edges[0].Resolver)

	exportNode, err := database.Reader.GetMediaNodeByID(context.Background(), edges[0].TargetNodeID)
	require.NoError(t, err)
	assert.Equal(t, exportsLoc.ID, exportNode.StorageLocationID)
	assert.Equal(t, exportFile, exportNode.FilePath)

	// Test collision handling on duplicate upload with different content
	data2 := []byte("Different JPEG image content for collision test")
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data2))
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	req2.Header.Set("X-Filename", "IMG_2026.JPG")
	req2.Header.Set("X-Camera-Model", "Pixel 9 Pro")
	req2.Header.Set("X-Capture-Timestamp", "1787998200") // 2026-08-29
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusCreated, rec2.Code)
	var resp2 AgentUploadResponse
	err = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	require.NoError(t, err)
	assert.Contains(t, resp2.RelativePath, "IMG_2026_1.JPG")

	// Test RAW file upload (should NOT be linked to exports/immich/)
	rawData := []byte("RAW sensor payload")
	reqRaw := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(rawData))
	reqRaw.Header.Set("X-API-Key", routeTestAgentKey)
	reqRaw.Header.Set("X-Filename", "DSC_9999.ARW")
	reqRaw.Header.Set("X-Camera-Model", "Sony A7IV")
	reqRaw.Header.Set("X-Capture-Timestamp", "1787998200")
	recRaw := httptest.NewRecorder()
	handler.ServeHTTP(recRaw, reqRaw)
	assert.Equal(t, http.StatusCreated, recRaw.Code)
	var respRaw AgentUploadResponse
	err = json.Unmarshal(recRaw.Body.Bytes(), &respRaw)
	require.NoError(t, err)
	assert.Contains(t, respRaw.RelativePath, "DSC_9999.ARW")
	rawExportFile := filepath.Join(exportsDir, "immich", respRaw.RelativePath)
	_, statErr := os.Stat(rawExportFile)
	assert.True(t, os.IsNotExist(statErr), "RAW files should not be linked to Immich exports")
}

func TestAgentUploadConcurrentSameFilenameNoTOCTOU(t *testing.T) {
	// Two uploads of the same filename at the same timestamp must both succeed
	// and land in distinct on-disk paths. The O_EXCL loop guarantees no
	// Stat→Create window between the two goroutines.
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()

	type result struct {
		nodeUUID string
		path     string
		code     int
	}

	results := make(chan result, 2)
	for i := range 2 {
		go func(i int) {
			data := []byte(fmt.Sprintf("concurrent upload payload bytes %d", i))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
			req.Header.Set("X-API-Key", routeTestAgentKey)
			req.Header.Set("X-Filename", "CONCURRENT.JPG")
			req.Header.Set("X-Camera-Model", "TestCamera")
			req.Header.Set("X-Capture-Timestamp", "1787998200")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			var resp AgentUploadResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			results <- result{nodeUUID: resp.NodeUUID, path: resp.RelativePath, code: rec.Code}
		}(i)
	}

	r1 := <-results
	r2 := <-results
	assert.Equal(t, http.StatusCreated, r1.code)
	assert.Equal(t, http.StatusCreated, r2.code)
	// Both must have landed at different paths — no file was silently overwritten.
	assert.NotEqual(t, r1.path, r2.path, "concurrent uploads must produce distinct archive paths")
	_, err1 := os.Stat(filepath.Join(archiveDir, r1.path))
	_, err2 := os.Stat(filepath.Join(archiveDir, r2.path))
	assert.NoError(t, err1, "first upload's file must exist on disk")
	assert.NoError(t, err2, "second upload's file must exist on disk")

	// Verify fast_hash is populated (non-nil) in both DB nodes. This catches
	// any regression where O_WRONLY prevents ReadAt inside hashing.FastHash.
	node1, err := database.Reader.GetMediaNodeByUUID(context.Background(), r1.nodeUUID)
	require.NoError(t, err)
	assert.NotNil(t, node1.FastHash, "fast_hash must be populated for first upload node")

	node2, err := database.Reader.GetMediaNodeByUUID(context.Background(), r2.nodeUUID)
	require.NoError(t, err)
	assert.NotNil(t, node2.FastHash, "fast_hash must be populated for second upload node")
}

func TestAgentUploadContentDedup(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()
	data := []byte("identical upload bytes for dedup test")
	hasher := blake3.New()
	_, _ = hasher.Write(data)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	// First upload: creates node
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	req1.Header.Set("X-Filename", "FIRST.JPG")
	req1.Header.Set("X-Blake3-Hash", expectedHash)

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	require.Equal(t, "UPLOADED", resp1.Status)
	require.Equal(t, expectedHash, resp1.Blake3Hash)
	firstUUID := resp1.NodeUUID

	// Second upload with pre-flight X-Blake3-Hash header: pre-write dedup hit
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	req2.Header.Set("X-Filename", "SECOND.JPG")
	req2.Header.Set("X-Blake3-Hash", expectedHash)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("X-Dedup"))

	var resp2 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, "DEDUPLICATED", resp2.Status)
	assert.Equal(t, firstUUID, resp2.NodeUUID)
	assert.Equal(t, expectedHash, resp2.Blake3Hash)
	assert.Equal(t, int64(0), resp2.BytesWritten)

	// Third upload without X-Blake3-Hash header: post-compute dedup hit
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req3.Header.Set("X-API-Key", routeTestAgentKey)
	req3.Header.Set("X-Filename", "THIRD.JPG")

	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	assert.Equal(t, http.StatusOK, rec3.Code)
	assert.Equal(t, "true", rec3.Header().Get("X-Dedup"))

	var resp3 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &resp3))
	assert.Equal(t, "DEDUPLICATED", resp3.Status)
	assert.Equal(t, firstUUID, resp3.NodeUUID)
}

func TestAgentUploadContentDedup_AllowsReingestAfterMissing(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()
	data := []byte("content for missing node re-ingest test")

	// First upload: creates node1
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	req1.Header.Set("X-Filename", "UPLOAD1.JPG")

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	require.Equal(t, "UPLOADED", resp1.Status)

	// Mark node1 as MISSING
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		n, err := q.GetMediaNodeByUUID(context.Background(), resp1.NodeUUID)
		if err != nil {
			return err
		}
		return q.MarkNodeMissing(context.Background(), n.ID)
	})
	require.NoError(t, err)

	// Second upload of identical content: should create fresh node2, NOT dedup to MISSING node1
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	req2.Header.Set("X-Filename", "UPLOAD2.JPG")

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusCreated, rec2.Code)

	var resp2 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, "UPLOADED", resp2.Status)
	assert.NotEqual(t, resp1.NodeUUID, resp2.NodeUUID)
}

func TestAgentUpload_InvalidFilename(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader([]byte("test")))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "../../../etc/passwd")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAgentUpload_DedupRaceOrphanCleanup(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()
	data := []byte("agent upload bytes for orphan cleanup test")

	// Upload 1
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	req1.Header.Set("X-Filename", "TEST_ORPHAN.JPG")
	req1.Header.Set("X-Camera-Model", "TestCam")
	req1.Header.Set("X-Capture-Timestamp", "1787998200")

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	firstFile := filepath.Join(archiveDir, resp1.RelativePath)
	_, err = os.Stat(firstFile)
	require.NoError(t, err)

	// Upload 2 without X-Blake3-Hash: writes to TEST_ORPHAN_1.JPG then cleans it up upon dedup match
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req2.Header.Set("X-API-Key", routeTestAgentKey)
	req2.Header.Set("X-Filename", "TEST_ORPHAN.JPG")
	req2.Header.Set("X-Camera-Model", "TestCam")
	req2.Header.Set("X-Capture-Timestamp", "1787998200")

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("X-Dedup"))

	// Check orphan file is not present
	dir := filepath.Dir(firstFile)
	orphanFile := filepath.Join(dir, "TEST_ORPHAN_1.JPG")
	_, statErr := os.Stat(orphanFile)
	assert.True(t, os.IsNotExist(statErr), "orphan file must be cleaned up on dedup match")
}
