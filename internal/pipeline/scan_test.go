package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/indexer"
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

	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	location := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if jobID == 0 {
		t.Fatal("RunScan returned job id 0")
	}

	job := waitJobDone(t, database, jobID)
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

func seedPipelineLocation(t *testing.T, database *db.DB, rootPath string) int64 {
	t.Helper()
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "scan-test-" + t.Name(), RootPath: rootPath,
			Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		id = loc.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	return id
}

func scanTestDeps(t *testing.T, database *db.DB, rootPath string, locationID int64) ScanDeps {
	t.Helper()
	pool := workers.New[string](2, 16)
	poolCtx, cancelPool := context.WithCancel(context.Background())
	t.Cleanup(cancelPool)
	pool.Run(poolCtx)
	return ScanDeps{
		DB:             database,
		Guard:          storage.NewGuard([]storage.Location{{ID: locationID, Name: "test-export", RootPath: rootPath, Tier: "TIER2_EXPORTS", ReadOnly: false}}),
		Prober:         probe.New(),
		Pool:           pool,
		FullHashPolicy: "never",
	}
}

func waitJobDone(t *testing.T, database *db.DB, jobID int64) sqlcgen.ScanJob {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var job sqlcgen.ScanJob
	var err error
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.State != "RUNNING" {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %d did not reach a terminal state within 10s (state=%s)", jobID, job.State)
	return job
}

// waitForNextSecond blocks until the current wall-clock second is strictly
// over. The MISSING sweep's before_unix is the scan's started_at, stored via
// unixepoch() -- 1s granularity. A node inserted in the SAME second as the
// next scan's start has last_seen_at == before_unix, so the sweep skips it
// (delayed-not-wrong in production). Tests that need the sweep to fire
// deterministically cross a second boundary between scans.
func waitForNextSecond(t *testing.T) {
	t.Helper()
	now := time.Now()
	next := now.Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(next) + 50*time.Millisecond)
}

// TestScanSweepMarksDeletedFileMissing is the headline behavior of #31: a
// full scan whose walk no longer sees a previously-indexed file must mark
// that node MISSING so Pillar 5 move detection (RebaseMissingNodePath) can
// fire on a later scan.
func TestScanSweepMarksDeletedFileMissing(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	writeFile(t, filepath.Join(root, "b.txt"), "bravo content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}

	if err := os.Remove(filepath.Join(resolvedRoot, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	waitForNextSecond(t)

	jobID2, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 2): %v", err)
	}
	if job := waitJobDone(t, database, jobID2); job.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job.State, job.LastError)
	}

	// GetLiveNodeByPath includes MISSING rows (only ARCHIVED is excluded).
	a, err := database.Reader.GetLiveNodeByPath(ctx, filepath.Join(resolvedRoot, "a.txt"))
	if err != nil {
		t.Fatalf("GetLiveNodeByPath(a.txt): %v", err)
	}
	if a.LifecycleState != "MISSING" {
		t.Errorf("a.txt lifecycle_state = %q, want MISSING", a.LifecycleState)
	}
	b, err := database.Reader.GetLiveNodeByPath(ctx, filepath.Join(resolvedRoot, "b.txt"))
	if err != nil {
		t.Fatalf("GetLiveNodeByPath(b.txt): %v", err)
	}
	if b.LifecycleState != "ACTIVE" {
		t.Errorf("b.txt lifecycle_state = %q, want ACTIVE", b.LifecycleState)
	}
}

// TestScanSweepTriggersMoveDetectionRebase is Pillar 5 wired end to end: the
// MISSING sweep (#31) flags a deleted file, then a later scan finding the
// same content at a new path rebases the original node in place -- same id,
// same node_uuid, edges untouched.
func TestScanSweepTriggersMoveDetectionRebase(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q", job.State)
	}
	a := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt"))

	// Child node + edge on the original node -- must survive the rebase.
	childStats, err := Commit(ctx, database, locationID, []Result{
		{Path: filepath.Join(resolvedRoot, "a_proxy.txt"), FileName: "a_proxy.txt", FileExt: "txt", Size: 5, ModTime: time.Now(), FastHash: "cccccccccccccccc"},
	})
	if err != nil || childStats.Inserted != 1 {
		t.Fatalf("Commit child: stats=%+v err=%v", childStats, err)
	}
	child := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a_proxy.txt"))
	var edgeID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		e, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: a.ID, TargetNodeID: child.ID, RelationshipType: "PROXY_OF",
			Confidence: 0.9, Tier: 2, Resolver: "test-fixture", EvidenceJson: "{}",
			ReviewState: "AUTO_ACCEPTED",
		})
		edgeID = e.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	if err := os.Remove(filepath.Join(resolvedRoot, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	waitForNextSecond(t)
	jobID2, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 2): %v", err)
	}
	if job := waitJobDone(t, database, jobID2); job.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q", job.State)
	}
	if got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt")); got.LifecycleState != "MISSING" {
		t.Fatalf("pass 2 a.txt = %q, want MISSING", got.LifecycleState)
	}

	moved := filepath.Join(resolvedRoot, "moved")
	writeFile(t, filepath.Join(moved, "a.txt"), "alpha content")
	jobID3, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 3): %v", err)
	}
	if job := waitJobDone(t, database, jobID3); job.State != "COMPLETED" {
		t.Fatalf("pass 3 state = %q", job.State)
	}

	rebased := mustGetLiveNode(t, database, filepath.Join(moved, "a.txt"))
	if rebased.ID != a.ID {
		t.Errorf("rebased id = %d, want %d (same row)", rebased.ID, a.ID)
	}
	if rebased.NodeUuid != a.NodeUuid {
		t.Errorf("rebased node_uuid = %q, want %q (identity survives a move)", rebased.NodeUuid, a.NodeUuid)
	}
	if rebased.LifecycleState != "ACTIVE" {
		t.Errorf("rebased lifecycle_state = %q, want ACTIVE", rebased.LifecycleState)
	}
	if edge, err := database.Reader.GetMediaEdge(ctx, edgeID); err != nil || edge.SourceNodeID != a.ID {
		t.Errorf("edge %d on the moved node broken: err=%v src=%d want %d", edgeID, err, edge.SourceNodeID, a.ID)
	}
}

// TestScanAbortedPartwayLeavesAllNodesActive is the data-loss regression
// guard for the whole #31 design: the MISSING sweep must NEVER fire on a
// failed walk. A walk error (here: forced via the WalkFn seam) marks the
// job FAILED and must leave every previously-live node ACTIVE -- otherwise a
// transient read error on one mount would silently sweep an entire location.
func TestScanAbortedPartwayLeavesAllNodesActive(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}
	if got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt")); got.LifecycleState != "ACTIVE" {
		t.Fatalf("pre-conditions broken: a.txt = %q, want ACTIVE", got.LifecycleState)
	}

	deps.WalkFn = func(ctx context.Context, root string, onFile func(indexer.Record) error) error {
		return fmt.Errorf("simulated mid-walk failure")
	}
	jobID2, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 2): %v", err)
	}
	job := waitJobDone(t, database, jobID2)
	if job.State != "FAILED" {
		t.Fatalf("state = %q, want FAILED (last_error=%v)", job.State, job.LastError)
	}
	if !job.LastError.Valid || !strings.Contains(job.LastError.String, "simulated mid-walk failure") {
		t.Errorf("last_error = %v, want it to contain the walk error", job.LastError)
	}
	if got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt")); got.LifecycleState != "ACTIVE" {
		t.Errorf("a.txt lifecycle_state = %q, want ACTIVE (MISSING sweep must not run on a failed walk)", got.LifecycleState)
	}
}

// TestScanSweepSkippedWhenFilesFailed guards the false-MISSING bug: a file
// the walk SAW but failed to commit (processFile error) never gets
// last_seen_at bumped, so a pass with failed files must skip the sweep
// entirely -- otherwise a live file gets flipped to MISSING and can feed a
// spurious RebaseMissingNodePath steal. Mutation-sensitive by construction:
// pass 2 "sees" a.txt (a real, existing node) but fails to hash it, so its
// last_seen_at stays stale and, without the guard, the sweep catches it.
func TestScanSweepSkippedWhenFilesFailed(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q (last_error=%v)", job.State, job.LastError)
	}

	// Pass 2 must start in a strictly later wall-clock second than pass 1's
	// insert (unixepoch() is 1s-granular), so a.txt's last_seen_at -- set in
	// pass 1, never bumped since -- is stale relative to pass 2's started_at
	// and a sweep would catch it.
	waitForNextSecond(t)

	// Pass 2: emit a SINGLE record at a.txt's real path with an inflated
	// Size. The walk "sees" a.txt, but hashing.FastHash's ReadAt hits a
	// short-EOF read (hashing.go:43-44: read != n with io.EOF) and errors, so
	// processFile fails -- filesFailed > 0 -- while the walk itself returns
	// nil. a.txt is never re-committed this pass, so its last_seen_at stays
	// stale from pass 1.
	aPath := filepath.Join(resolvedRoot, "a.txt")
	deps.WalkFn = func(ctx context.Context, root string, onFile func(indexer.Record) error) error {
		return onFile(indexer.Record{Path: aPath, Size: 1 << 30, ModTime: time.Now()})
	}
	jobID2, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 2): %v", err)
	}
	if job := waitJobDone(t, database, jobID2); job.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q (last_error=%v)", job.State, job.LastError)
	}

	if got := mustGetLiveNode(t, database, aPath); got.LifecycleState != "ACTIVE" {
		t.Errorf("a.txt lifecycle_state = %q, want ACTIVE (sweep must skip a pass with failed files)", got.LifecycleState)
	}
}

// TestSamePathRecreationReactivates: a file deleted then re-created at the
// SAME path with identical content hits commitOne's same-fast_hash branch.
// #31's TouchMediaNode change must reactivate that node in place rather than
// leaving a live file marked MISSING forever.
func TestSamePathRecreationReactivates(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 1): %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("pass 1 state = %q", job.State)
	}
	original := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt"))

	if err := os.Remove(filepath.Join(resolvedRoot, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	waitForNextSecond(t)
	jobID2, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 2): %v", err)
	}
	if job := waitJobDone(t, database, jobID2); job.State != "COMPLETED" {
		t.Fatalf("pass 2 state = %q", job.State)
	}
	if got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt")); got.LifecycleState != "MISSING" {
		t.Fatalf("pass 2 = %q, want MISSING", got.LifecycleState)
	}

	writeFile(t, filepath.Join(resolvedRoot, "a.txt"), "alpha content") // same content, same path
	jobID3, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan (pass 3): %v", err)
	}
	if job := waitJobDone(t, database, jobID3); job.State != "COMPLETED" {
		t.Fatalf("pass 3 state = %q", job.State)
	}
	got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "a.txt"))
	if got.ID != original.ID {
		t.Errorf("id = %d, want %d (reactivated in place, no new row)", got.ID, original.ID)
	}
	if got.LifecycleState != "ACTIVE" {
		t.Errorf("lifecycle_state = %q, want ACTIVE", got.LifecycleState)
	}
}
