package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
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

	// Test collision handling on duplicate upload
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
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
