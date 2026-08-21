package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/thumbs"
)

func openThumbnailTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thumbnail-route.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func thumbnailTestNode(t *testing.T, database *db.DB, thumbState string) sqlcgen.MediaNode {
	t.Helper()
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "thumb-test", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		node, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "0198abcd-0000-7000-8000-000000000001", StorageLocationID: loc.ID,
			FilePath: "/tmp/does-not-matter.jpg", FileName: "photo.jpg", FileExt: "jpg",
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		return q.SetThumbState(context.Background(), sqlcgen.SetThumbStateParams{
			ID: node.ID, ThumbState: thumbState, ThumbAttempts: 0,
		})
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	node.ThumbState = thumbState
	return node
}

func TestHandleThumbnailServesReadyNode(t *testing.T) {
	database := openThumbnailTestDB(t)

	cache := thumbs.New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	node := thumbnailTestNode(t, database, "READY")
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xD9} // minimal JPEG SOI/EOI marker pair, content is never decoded by the handler
	if err := cache.Write(node.NodeUuid, jpegBytes); err != nil {
		t.Fatalf("cache.Write: %v", err)
	}

	srv := New(Deps{DB: database, Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test", ThumbCache: cache})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/thumbnail", nil)
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if rr.Header().Get("ETag") == "" {
		t.Error("ETag header missing")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "private, max-age=3600" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, max-age=3600")
	}
	if rr.Body.String() != string(jpegBytes) {
		t.Errorf("body = %v, want %v", rr.Body.Bytes(), jpegBytes)
	}
}

func TestHandleThumbnailNotFoundWhenNotReady(t *testing.T) {
	for _, state := range []string{"PENDING", "UNSUPPORTED", "FAILED"} {
		t.Run(state, func(t *testing.T) {
			database := openThumbnailTestDB(t)

			cache := thumbs.New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
			node := thumbnailTestNode(t, database, state)

			srv := New(Deps{DB: database, Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test", ThumbCache: cache})
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/thumbnail", nil)
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Errorf("thumb_state=%s: status = %d, want 404", state, rr.Code)
			}
		})
	}
}

func TestHandleThumbnailNotFoundWhenCacheNil(t *testing.T) {
	database := openThumbnailTestDB(t)

	node := thumbnailTestNode(t, database, "READY")

	// No ThumbCache in Deps -- must 404, not panic on a nil dereference.
	srv := New(Deps{DB: database, Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/thumbnail", nil)
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleThumbnailNotFoundWhenUnknownID(t *testing.T) {
	database := openThumbnailTestDB(t)

	cache := thumbs.New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	srv := New(Deps{DB: database, Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test", ThumbCache: cache})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/999999/thumbnail", nil)
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
