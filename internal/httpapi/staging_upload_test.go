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

func TestStagingUploadStreaming(t *testing.T) {
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

	data := []byte("Hello, branchDAM streaming staging upload test bytes!")
	hasher := blake3.New()
	_, err = hasher.Write(data)
	require.NoError(t, err)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/staging/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "PXL_20260829_001.dng")
	req.Header.Set("X-Blake3-Hash", expectedHash)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp StagingUploadResponse
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "STAGED", resp.Status)
	assert.Equal(t, int64(len(data)), resp.BytesWritten)
	assert.Equal(t, expectedHash, resp.Blake3Hash)
	assert.NotEmpty(t, resp.NodeUUID)

	// Verify node in database
	node, err := database.Reader.GetMediaNodeByUUID(context.Background(), resp.NodeUUID)
	require.NoError(t, err)
	assert.Equal(t, "INDEXED_SHALLOW", node.IndexingStatus)
	assert.Equal(t, expectedHash, *node.FullHash)
}

func TestStagingUploadNoWritableLocation(t *testing.T) {
	srv, _ := fullTestServer(t)

	handler := srv.Handler()

	data := []byte("sample payload")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staging/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "test.jpg")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestStagingUploadChecksumMismatch(t *testing.T) {
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staging/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "test.jpg")
	req.Header.Set("X-Blake3-Hash", "0000000000000000000000000000000000000000000000000000000000000000")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestStagingUploadAuthForbidden(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)

	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/staging/upload", bytes.NewReader([]byte("unauthed")))
	req.Header.Set("X-API-Key", "wrong-key")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
