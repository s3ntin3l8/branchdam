package thumbs

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func openWorkerTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thumbs-worker.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return database
}

func seedTestLocation(t *testing.T, database *db.DB, rootPath string) int64 {
	t.Helper()
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "worker-test", RootPath: rootPath, Tier: "PROJECTS",
		})
		id = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	return id
}

func seedTestNode(t *testing.T, database *db.DB, locationID int64, uuidSuffix, path string) sqlcgen.MediaNode {
	t.Helper()
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		node, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "0198abcd-1234-7000-8000-0000000000" + uuidSuffix,
			StorageLocationID: locationID,
			FilePath:          path,
			FileName:          filepath.Base(path),
			FileExt:           "png",
			IndexingStatus:    "INDEXED_SHALLOW",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed node %s: %v", path, err)
	}
	return node
}

func writeWorkerTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for x := 0; x < 200; x++ {
		for y := 0; y < 100; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
}

func TestWorkerProcessPendingGeneratesReady(t *testing.T) {
	database := openWorkerTestDB(t)
	locDir := t.TempDir()
	locationID := seedTestLocation(t, database, locDir)

	imgPath := filepath.Join(locDir, "photo.png")
	writeWorkerTestPNG(t, imgPath)
	node := seedTestNode(t, database, locationID, "01", imgPath)

	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	w := NewWorker(database, cache, nil)

	stats, err := w.ProcessPending(context.Background())
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if stats.Generated != 1 {
		t.Errorf("stats.Generated = %d, want 1", stats.Generated)
	}

	got, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if got.ThumbState != "READY" {
		t.Errorf("thumb_state = %q, want READY", got.ThumbState)
	}
	if got.ThumbAttempts != 0 {
		t.Errorf("thumb_attempts = %d, want 0", got.ThumbAttempts)
	}
	if _, err := os.Stat(cache.Path(node.NodeUuid)); err != nil {
		t.Errorf("thumbnail file not written: %v", err)
	}
}

func TestWorkerProcessPendingUnsupported(t *testing.T) {
	database := openWorkerTestDB(t)
	locDir := t.TempDir()
	locationID := seedTestLocation(t, database, locDir)

	textPath := filepath.Join(locDir, "notes.txt")
	if err := os.WriteFile(textPath, []byte("not an image"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}
	node := seedTestNode(t, database, locationID, "02", textPath)

	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	w := NewWorker(database, cache, nil)

	stats, err := w.ProcessPending(context.Background())
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if stats.Unsupported != 1 {
		t.Errorf("stats.Unsupported = %d, want 1", stats.Unsupported)
	}

	got, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if got.ThumbState != "UNSUPPORTED" {
		t.Errorf("thumb_state = %q, want UNSUPPORTED", got.ThumbState)
	}
}

func TestWorkerProcessPendingFailedIncrementsAttempts(t *testing.T) {
	database := openWorkerTestDB(t)
	locDir := t.TempDir()
	locationID := seedTestLocation(t, database, locDir)

	// A path under the seeded location that never actually exists on disk --
	// Generate's guard.OpenRead fails with a real I/O error, distinct from
	// ErrUnsupported (see cache_test.go's TestCacheGenerateMissingFile).
	missingPath := filepath.Join(locDir, "vanished.png")
	node := seedTestNode(t, database, locationID, "03", missingPath)

	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	w := NewWorker(database, cache, nil)

	stats, err := w.ProcessPending(context.Background())
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if stats.Failed != 1 {
		t.Errorf("stats.Failed = %d, want 1", stats.Failed)
	}

	got, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if got.ThumbState != "FAILED" {
		t.Errorf("thumb_state = %q, want FAILED", got.ThumbState)
	}
	if got.ThumbAttempts != 1 {
		t.Errorf("thumb_attempts = %d, want 1 (incremented from 0)", got.ThumbAttempts)
	}
}

func TestWorkerProcessPendingRespectsMaxAttempts(t *testing.T) {
	database := openWorkerTestDB(t)
	locDir := t.TempDir()
	locationID := seedTestLocation(t, database, locDir)

	missingPath := filepath.Join(locDir, "vanished.png")
	node := seedTestNode(t, database, locationID, "04", missingPath)

	// Push thumb_attempts to the bound directly via SetThumbState -- what a
	// prior FAILED pass would have left behind.
	if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		return q.SetThumbState(context.Background(), sqlcgen.SetThumbStateParams{
			ID: node.ID, ThumbState: "PENDING", ThumbAttempts: DefaultMaxAttempts,
		})
	}); err != nil {
		t.Fatalf("pre-set thumb_attempts: %v", err)
	}

	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	w := NewWorker(database, cache, nil)

	stats, err := w.ProcessPending(context.Background())
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if stats.Generated+stats.Unsupported+stats.Failed != 0 {
		t.Errorf("stats = %+v, want a node at the retry bound to be excluded entirely", stats)
	}

	got, err := database.Reader.GetMediaNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("GetMediaNodeByID: %v", err)
	}
	if got.ThumbState != "PENDING" {
		t.Errorf("thumb_state = %q, want unchanged PENDING", got.ThumbState)
	}
}

func TestWorkerProcessPendingBatchConcurrent(t *testing.T) {
	database := openWorkerTestDB(t)
	locDir := t.TempDir()
	locationID := seedTestLocation(t, database, locDir)

	const n = 12
	for i := 0; i < n; i++ {
		imgPath := filepath.Join(locDir, fmt.Sprintf("photo-%02d.png", i))
		writeWorkerTestPNG(t, imgPath)
		seedTestNode(t, database, locationID, fmt.Sprintf("%02d", 10+i), imgPath)
	}

	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	// Deliberately fewer goroutines than nodes, so the semaphore actually
	// bounds concurrency rather than every node running at once.
	w := NewWorker(database, cache, nil, WithConcurrency(3), WithBatchSize(n))

	stats, err := w.ProcessPending(context.Background())
	if err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if stats.Generated != n {
		t.Errorf("stats.Generated = %d, want %d (no result lost across concurrent goroutines)", stats.Generated, n)
	}
}

func TestWorkerNudgeCalledOnChange(t *testing.T) {
	database := openWorkerTestDB(t)
	locDir := t.TempDir()
	locationID := seedTestLocation(t, database, locDir)

	imgPath := filepath.Join(locDir, "photo.png")
	writeWorkerTestPNG(t, imgPath)
	seedTestNode(t, database, locationID, "05", imgPath)

	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	nudged := false
	w := NewWorker(database, cache, nil, WithNudge(func() { nudged = true }))

	if _, err := w.ProcessPending(context.Background()); err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if !nudged {
		t.Error("nudge was not called after a pass that changed thumb_state")
	}
}

func TestWorkerNudgeNotCalledWhenNothingPending(t *testing.T) {
	database := openWorkerTestDB(t)
	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	nudged := false
	w := NewWorker(database, cache, nil, WithNudge(func() { nudged = true }))

	if _, err := w.ProcessPending(context.Background()); err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if nudged {
		t.Error("nudge was called even though nothing was pending")
	}
}

func TestWorkerStartWait(t *testing.T) {
	database := openWorkerTestDB(t)
	cache := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	w := NewWorker(database, cache, nil)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx, 10*time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5s of ctx cancellation")
	}
}
