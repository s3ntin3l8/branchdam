package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

// TestRunScanEndToEnd exercises the full pipeline against real files on
// disk: indexer.Walk finds them, storage.Guard gates the reads, a
// workers.Pool hashes them concurrently, and drainAndCommit writes the
// resulting nodes -- proving the pieces built across PR 2 through PR 6
// actually compose, not just that each passes in isolation.
func TestRunScanEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "bravo content")
	writeFile(t, filepath.Join(root, "sub", "c.txt"), "charlie content")

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	database := openTestDB(t)
	ctx := context.Background()

	var locationID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "test-export",
			RootPath: resolvedRoot,
			Tier:     "TIER2_EXPORTS",
			ReadOnly: 0,
			Prunable: 0,
		})
		locationID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})

	pool := workers.New[string](2, 16)
	poolCtx, cancelPool := context.WithCancel(context.Background())
	defer cancelPool()
	pool.Run(poolCtx)

	deps := ScanDeps{
		DB:             database,
		Guard:          guard,
		Prober:         probe.New(), // whatever is/isn't installed -- RunScan must work either way
		Pool:           pool,
		FullHashPolicy: "never", // keep this test focused on orchestration, not hashing policy (covered by TestNeedsFullHashPolicy)
	}
	location := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if jobID == 0 {
		t.Fatal("RunScan returned job id 0")
	}

	var job sqlcgen.ScanJob
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.State != "RUNNING" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q, want COMPLETED (last_error=%v)", job.State, job.LastError)
	}
	if job.FilesSeen != 3 {
		t.Errorf("FilesSeen = %d, want 3", job.FilesSeen)
	}
	if job.FilesHashed != 3 {
		t.Errorf("FilesHashed = %d, want 3", job.FilesHashed)
	}
	if job.FilesFailed != 0 {
		t.Errorf("FilesFailed = %d, want 0", job.FilesFailed)
	}

	for _, name := range []string{"a.txt", filepath.Join("sub", "b.txt"), filepath.Join("sub", "c.txt")} {
		path := filepath.Join(resolvedRoot, name)
		node, err := database.Reader.GetLiveNodeByPath(ctx, path)
		if err != nil {
			t.Errorf("node for %s not found: %v", name, err)
			continue
		}
		if node.FastHash == nil || len(*node.FastHash) != 16 {
			t.Errorf("node for %s has fast_hash = %v, want a 16-char hash", name, node.FastHash)
		}
		if node.IndexingStatus != "INDEXED_SHALLOW" {
			t.Errorf("node for %s indexing_status = %q, want INDEXED_SHALLOW (FullHashPolicy=never)", name, node.IndexingStatus)
		}
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
