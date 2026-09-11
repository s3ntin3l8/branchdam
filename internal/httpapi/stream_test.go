package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func streamTestServer(t *testing.T, dir string) (*Server, *db.DB, int64) {
	t.Helper()
	database := openThumbnailTestDB(t)

	var locID int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "exports", RootPath: dir, Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		locID = loc.ID
		return nil
	})
	if err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: locID, Name: "exports", RootPath: dir, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})

	srv := New(Deps{
		DB:      database,
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
		Guard:   guard,
	})
	return srv, database, locID
}

func TestStreamAssetSuccess(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "sample.mp4")
	content := []byte("fake-mp4-video-content-stream-bytes-1234567890")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv, database, locID := streamTestServer(t, dir)
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "0198abcd-0000-7000-8000-000000000099",
			StorageLocationID: locID,
			FilePath:          filePath,
			FileName:          "sample.mp4",
			FileExt:           "mp4",
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "LINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InsertMediaNode: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/stream", node.ID), nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	if ar := rr.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
	if etag := rr.Header().Get("ETag"); etag == "" {
		t.Error("ETag is empty")
	}
	if rr.Body.String() != string(content) {
		t.Errorf("body = %q, want %q", rr.Body.String(), string(content))
	}
}

func TestStreamAssetWebMContentType(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "clip.webm")
	if err := os.WriteFile(filePath, []byte("webm-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv, database, locID := streamTestServer(t, dir)
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "0198abcd-0000-7000-8000-000000000095",
			StorageLocationID: locID,
			FilePath:          filePath,
			FileName:          "clip.webm",
			FileExt:           "webm",
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "LINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InsertMediaNode: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/stream", node.ID), nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "video/webm" {
		t.Errorf("Content-Type = %q, want video/webm (must not fall back to audio/webm)", ct)
	}
}

func TestStreamAssetRangeRequest(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "video.mp4")
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv, database, locID := streamTestServer(t, dir)
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "0198abcd-0000-7000-8000-000000000088",
			StorageLocationID: locID,
			FilePath:          filePath,
			FileName:          "video.mp4",
			FileExt:           "mp4",
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "LINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InsertMediaNode: %v", err)
	}

	// Byte range request: bytes=5-9
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/stream", node.ID), nil)
	req.Header.Set("Range", "bytes=5-9")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 Partial Content", rr.Code)
	}
	expectedSlice := string(content[5:10]) // bytes 5,6,7,8,9
	if rr.Body.String() != expectedSlice {
		t.Errorf("range body = %q, want %q", rr.Body.String(), expectedSlice)
	}
	wantRange := fmt.Sprintf("bytes 5-9/%d", len(content))
	if cr := rr.Header().Get("Content-Range"); cr != wantRange {
		t.Errorf("Content-Range = %q, want %q", cr, wantRange)
	}
}

func TestStreamAssetNotFoundOrArchived(t *testing.T) {
	dir := t.TempDir()
	srv, database, locID := streamTestServer(t, dir)

	t.Run("unknown id 404s", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/999999/stream", nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("unknown id status = %d, want 404", rr.Code)
		}
	})

	t.Run("archived asset 404s", func(t *testing.T) {
		var node sqlcgen.MediaNode
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			var err error
			node, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
				NodeUuid:          "0198abcd-0000-7000-8000-000000000077",
				StorageLocationID: locID,
				FilePath:          filepath.Join(dir, "archived.jpg"),
				FileName:          "archived.jpg",
				FileExt:           "jpg",
				IndexingStatus:    "INDEXED_FULL",
				GraphStatus:       "LINKED",
				LifecycleState:    "ARCHIVED",
			})
			return err
		})
		if err != nil {
			t.Fatalf("InsertMediaNode: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/stream", node.ID), nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("archived asset status = %d, want 404", rr.Code)
		}
	})
}
