package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
)

func createMultipartUploadRequest(t *testing.T, filename string, content []byte, formFields map[string]string) (*http.Request, string) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for k, v := range formFields {
		require.NoError(t, writer.WriteField(k, v))
	}

	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)

	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Authentik-Username", "operator")
	return req, writer.FormDataContentType()
}

func TestWebUpload_MultipartSuccess(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	var archiveLoc sqlcgen.StorageLocation
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		archiveLoc, err = q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
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
	content := []byte("Camera RAW photo file content for web upload test")

	req, _ := createMultipartUploadRequest(t, "DSC_1001.ARW", content, map[string]string{
		"overrideCameraModel": "Sony A7IV",
		"overrideCapturedAt":  "1787998200", // 2026-08-29
		"applyNamingTemplate": "true",
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp WebUploadResponse
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "UPLOADED", resp.Status)
	assert.NotEmpty(t, resp.NodeUUID)
	assert.NotEmpty(t, resp.Blake3Hash)
	assert.Contains(t, resp.RelativePath, "Sony A7IV")
	assert.Contains(t, resp.RelativePath, "DSC_1001.ARW")
	assert.Equal(t, int64(len(content)), resp.BytesWritten)
	assert.Equal(t, resp.NodeUUID, resp.Asset.NodeUUID)

	// Verify file on disk
	diskPath := filepath.Join(archiveDir, resp.RelativePath)
	diskBytes, err := os.ReadFile(diskPath)
	require.NoError(t, err)
	assert.Equal(t, content, diskBytes)

	// Verify media node in database
	node, err := database.Reader.GetMediaNodeByUUID(context.Background(), resp.NodeUUID)
	require.NoError(t, err)
	assert.Equal(t, archiveLoc.ID, node.StorageLocationID)
	assert.Equal(t, "INDEXED_SHALLOW", node.IndexingStatus)
	assert.NotNil(t, node.FastHash)
	assert.Equal(t, resp.Blake3Hash, *node.FullHash)
}

func TestWebUpload_PreserveRelativePath(t *testing.T) {
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
	content := []byte("Video footage content")

	req, _ := createMultipartUploadRequest(t, "CLIP_01.MP4", content, map[string]string{
		"relativePath":        "Shoots/2026_ProjectA",
		"applyNamingTemplate": "false",
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp WebUploadResponse
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "UPLOADED", resp.Status)
	assert.Equal(t, filepath.Join("Shoots", "2026_ProjectA", "CLIP_01.MP4"), filepath.Clean(resp.RelativePath))

	diskPath := filepath.Join(archiveDir, "Shoots", "2026_ProjectA", "CLIP_01.MP4")
	diskBytes, err := os.ReadFile(diskPath)
	require.NoError(t, err)
	assert.Equal(t, content, diskBytes)
}

func TestWebUpload_AuthRejection(t *testing.T) {
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

	// 1. Unauthenticated (no X-Authentik-Username)
	req, _ := createMultipartUploadRequest(t, "test.jpg", []byte("data"), nil)
	req.Header.Del("X-Authentik-Username")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWebUpload_AdminGroupGating(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	srv.cfg().Authz.Groups = []string{"dam-admins"}

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

	// Non-admin user
	reqNonAdmin, _ := createMultipartUploadRequest(t, "test.jpg", []byte("data"), nil)
	reqNonAdmin.Header.Set("X-Authentik-Username", "regular_user")
	reqNonAdmin.Header.Set("X-Authentik-Groups", "viewers|editors")

	recNonAdmin := httptest.NewRecorder()
	handler.ServeHTTP(recNonAdmin, reqNonAdmin)
	assert.Equal(t, http.StatusForbidden, recNonAdmin.Code)

	// Admin user
	reqAdmin, _ := createMultipartUploadRequest(t, "test.jpg", []byte("data"), nil)
	reqAdmin.Header.Set("X-Authentik-Username", "admin_user")
	reqAdmin.Header.Set("X-Authentik-Groups", "dam-admins|staff")

	recAdmin := httptest.NewRecorder()
	handler.ServeHTTP(recAdmin, reqAdmin)
	assert.Equal(t, http.StatusCreated, recAdmin.Code)
}

func TestWebUpload_ReadOnlyLocationConflict(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	roDir := filepath.Join(tmpDir, "ro_archive")
	require.NoError(t, os.MkdirAll(roDir, 0o755))

	var roLoc sqlcgen.StorageLocation
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		roLoc, err = q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "ROArchive",
			RootPath: roDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 1, // Read-only
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()

	req, _ := createMultipartUploadRequest(t, "test.jpg", []byte("data"), map[string]string{
		"storageLocationId": strconv.FormatInt(roLoc.ID, 10),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should be refused as read-only (403 Forbidden)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWebUpload_PathTraversalRejection(t *testing.T) {
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

	// Traversal in relative path
	req, _ := createMultipartUploadRequest(t, "payload.sh", []byte("malicious"), map[string]string{
		"relativePath":        "../../etc",
		"applyNamingTemplate": "false",
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWebUpload_CollisionHandling(t *testing.T) {
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

	// Upload 1
	req1, _ := createMultipartUploadRequest(t, "photo.jpg", []byte("file 1 content"), map[string]string{
		"relativePath":        "coll_test",
		"applyNamingTemplate": "false",
	})
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	assert.Equal(t, filepath.Join("coll_test", "photo.jpg"), filepath.Clean(resp1.RelativePath))

	// Upload 2 (same name and path)
	req2, _ := createMultipartUploadRequest(t, "photo.jpg", []byte("file 2 content"), map[string]string{
		"relativePath":        "coll_test",
		"applyNamingTemplate": "false",
	})
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusCreated, rec2.Code)

	var resp2 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, filepath.Join("coll_test", "photo_1.jpg"), filepath.Clean(resp2.RelativePath))
}

func TestWebUpload_ContentDedup(t *testing.T) {
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
	payload := []byte("identical binary content for web dedup")

	// First upload
	req1, _ := createMultipartUploadRequest(t, "dedup.jpg", payload, map[string]string{
		"relativePath":        "dedup_test",
		"applyNamingTemplate": "false",
	})
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	assert.Equal(t, "UPLOADED", resp1.Status)

	// Second upload with same content
	req2, _ := createMultipartUploadRequest(t, "another_name.jpg", payload, map[string]string{
		"relativePath":        "dedup_test",
		"applyNamingTemplate": "false",
	})
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("X-Dedup"))

	var resp2 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, "DEDUPLICATED", resp2.Status)
	assert.Equal(t, resp1.NodeUUID, resp2.NodeUUID)
	assert.NotZero(t, resp2.Asset.ID)
	assert.Equal(t, resp1.Asset.ID, resp2.Asset.ID)
	assert.Equal(t, resp1.Asset.SizeBytes, resp2.Asset.SizeBytes)
	assert.Positive(t, resp2.Asset.SizeBytes)
	assert.Equal(t, int64(len(payload)), resp2.Asset.SizeBytes)
}

func TestWebUpload_ContentDedup_PreFlightHash(t *testing.T) {
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
	payload := []byte("distinct content for preflight dedup test")
	blake3Hash, err := hashing.FullHash(bytes.NewReader(payload))
	require.NoError(t, err)

	// First upload
	req1, _ := createMultipartUploadRequest(t, "original.jpg", payload, map[string]string{
		"relativePath":        "preflight_test",
		"applyNamingTemplate": "false",
	})
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	assert.Equal(t, "UPLOADED", resp1.Status)
	assert.Equal(t, int64(len(payload)), resp1.Asset.SizeBytes)

	// Second upload with pre-flight X-Blake3-Hash header
	req2, _ := createMultipartUploadRequest(t, "preflight_dup.jpg", payload, map[string]string{
		"relativePath":        "preflight_test",
		"applyNamingTemplate": "false",
	})
	req2.Header.Set("X-Blake3-Hash", blake3Hash)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("X-Dedup"))

	var resp2 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, "DEDUPLICATED", resp2.Status)
	assert.Equal(t, resp1.NodeUUID, resp2.NodeUUID)
	assert.Equal(t, int64(0), resp2.BytesWritten)
	assert.NotZero(t, resp2.Asset.ID)
	assert.Equal(t, resp1.Asset.ID, resp2.Asset.ID)
	assert.Equal(t, resp1.Asset.SizeBytes, resp2.Asset.SizeBytes)
	assert.Positive(t, resp2.Asset.SizeBytes)
	assert.Equal(t, int64(len(payload)), resp2.Asset.SizeBytes)
}

func TestWebUpload_InvalidStorageLocation(t *testing.T) {
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

	// Non-existent storage location ID
	reqNonExistent, _ := createMultipartUploadRequest(t, "test.jpg", []byte("sample"), map[string]string{
		"storageLocationId": "99999",
	})
	recNonExistent := httptest.NewRecorder()
	handler.ServeHTTP(recNonExistent, reqNonExistent)
	assert.Equal(t, http.StatusBadRequest, recNonExistent.Code)

	// Negative storage location ID
	reqNegative, _ := createMultipartUploadRequest(t, "test.jpg", []byte("sample"), map[string]string{
		"storageLocationId": "-5",
	})
	recNegative := httptest.NewRecorder()
	handler.ServeHTTP(recNegative, reqNegative)
	assert.Equal(t, http.StatusBadRequest, recNegative.Code)

	// Non-numeric storage location ID
	reqInvalid, _ := createMultipartUploadRequest(t, "test.jpg", []byte("sample"), map[string]string{
		"storageLocationId": "not-a-number",
	})
	recInvalid := httptest.NewRecorder()
	handler.ServeHTTP(recInvalid, reqInvalid)
	assert.Equal(t, http.StatusBadRequest, recInvalid.Code)
}

func TestWebUpload_InactiveStorageLocation(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)

	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	var inactiveLoc sqlcgen.StorageLocation
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		inactiveLoc, err = q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "InactiveLocation",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}
		return q.SetStorageLocationActive(context.Background(), sqlcgen.SetStorageLocationActiveParams{
			ID:       inactiveLoc.ID,
			IsActive: 0,
		})
	})
	require.NoError(t, err)

	handler := srv.Handler()

	req, _ := createMultipartUploadRequest(t, "test.jpg", []byte("sample"), map[string]string{
		"storageLocationId": strconv.FormatInt(inactiveLoc.ID, 10),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWebUpload_DedupRaceOrphanCleanup(t *testing.T) {
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
	payload := []byte("distinct payload for dedup race orphan cleanup test")

	// First upload
	req1, _ := createMultipartUploadRequest(t, "first.jpg", payload, map[string]string{
		"relativePath":        "orphan_test",
		"applyNamingTemplate": "false",
	})
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code)

	var resp1 WebUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	firstFilePath := filepath.Join(archiveDir, resp1.RelativePath)
	_, err = os.Stat(firstFilePath)
	require.NoError(t, err, "first upload file must exist on disk")

	// Second upload without preflight hash: streams bytes to disk (as first_1.jpg),
	// then detects dedup match post-write, removes first_1.jpg, and returns DEDUPLICATED
	req2, _ := createMultipartUploadRequest(t, "first.jpg", payload, map[string]string{
		"relativePath":        "orphan_test",
		"applyNamingTemplate": "false",
	})
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("X-Dedup"))

	// Verify only first.jpg exists on disk and no orphan first_1.jpg exists
	orphanPath := filepath.Join(archiveDir, "orphan_test", "first_1.jpg")
	_, orphanErr := os.Stat(orphanPath)
	assert.True(t, os.IsNotExist(orphanErr), "orphan file must be cleaned up on dedup match")
}
