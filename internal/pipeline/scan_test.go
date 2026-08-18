package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
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
	return scanTestDepsN(t, database, rootPath, locationID, 2, 16)
}

// scanTestDepsN is scanTestDeps with the worker count and queue depth
// exposed, for tests that need to control the pool's capacity directly --
// e.g. TestDrainAndCommitRunsConcurrentlyWithWalk, which needs a generous
// queue depth so queue capacity itself is never the limiting factor and
// the test exercises only the property it's named for.
func scanTestDepsN(t *testing.T, database *db.DB, rootPath string, locationID int64, workerCount, queueDepth int) ScanDeps {
	t.Helper()
	pool := workers.New[string](workerCount, queueDepth)
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
	return waitJobDoneWithin(t, database, jobID, 10*time.Second)
}

func waitJobDoneWithin(t *testing.T, database *db.DB, jobID int64, timeout time.Duration) sqlcgen.ScanJob {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
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
	t.Fatalf("job %d did not reach a terminal state within %s (state=%s)", jobID, timeout, job.State)
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

// TestScanSweepExcludesFailedPaths guards the false-MISSING bug: a file the
// walk SAW but failed to commit (processFile error) never gets last_seen_at
// bumped, so the sweep must exclude exactly those paths -- not skip the whole
// location -- or a live file gets flipped to MISSING and can feed a spurious
// RebaseMissingNodePath steal. Mutation-sensitive by construction: pass 2
// "sees" a.txt (a real, existing node) but fails to hash it, so its
// last_seen_at stays stale; b.txt is on disk but genuinely absent from pass
// 2's walk. With the exclusion, a.txt survives as ACTIVE while b.txt is swept
// MISSING; without it, both would be swept.
func TestScanSweepExcludesFailedPaths(t *testing.T) {
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
	for _, name := range []string{"a.txt", "b.txt"} {
		if got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, name)); got.LifecycleState != "ACTIVE" {
			t.Fatalf("pre-conditions broken: %s = %q, want ACTIVE", name, got.LifecycleState)
		}
	}

	// Pass 2 must start in a strictly later wall-clock second than pass 1's
	// insert (unixepoch() is 1s-granular), so both nodes' last_seen_at -- set
	// in pass 1, never bumped since -- are stale relative to pass 2's
	// started_at and the sweep would catch them.
	waitForNextSecond(t)

	// Pass 2: emit a SINGLE record at a.txt's real path with an inflated
	// Size, and nothing for b.txt. The walk "sees" a.txt, but
	// hashing.FastHash's ReadAt hits a short-EOF read (hashing.go:43-44:
	// read != n with io.EOF) and errors, so processFile fails -- a.txt joins
	// the seen-but-uncertain set -- while the walk itself returns nil. b.txt
	// is genuinely absent from this pass's walk, so its stale last_seen_at is
	// exactly what the sweep is supposed to catch.
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
		t.Errorf("a.txt lifecycle_state = %q, want ACTIVE (seen-but-uncertain path must be excluded from the sweep)", got.LifecycleState)
	}
	if got := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "b.txt")); got.LifecycleState != "MISSING" {
		t.Errorf("b.txt lifecycle_state = %q, want MISSING (genuinely unseen node must still be swept)", got.LifecycleState)
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

// requireTool skips the test when the named binary isn't installed, mirroring
// internal/probe's own helper -- neither ffmpeg nor exiftool is present on
// every machine that runs `go test ./...` (notably, the CI Go job doesn't
// install them), matching the graceful-degradation requirement of the spec.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed, skipping (the e2e EXIF test needs it)", name)
	}
	return path
}

// makeTaggedFixtureJPEG generates a tiny real JPEG via ffmpeg and tags it
// with exiftool, so the e2e test exercises the actual probe subprocess path
// end to end rather than a canned fixture checked into the repo.
func makeTaggedFixtureJPEG(t *testing.T, dir string) string {
	t.Helper()
	requireTool(t, "ffmpeg")
	exiftoolPath := requireTool(t, "exiftool")

	path := filepath.Join(dir, "fixture.jpg")
	ffmpeg := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x64", "-frames:v", "1", path)
	if out, err := ffmpeg.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture jpeg: %v\n%s", err, out)
	}

	tagArgs := []string{
		"-overwrite_original",
		"-EXIF:Make=SONY", "-EXIF:Model=ILCE-7M4", "-EXIF:LensModel=FE 24-70mm F2.8 GM",
		"-EXIF:SerialNumber=1234567",
		"-EXIF:DateTimeOriginal=2026:07:15 14:30:00", "-EXIF:OffsetTimeOriginal=+02:00",
		"-GPSLatitude=33.9", "-GPSLatitudeRef=S", "-GPSLongitude=18.4", "-GPSLongitudeRef=W",
		"-XMP-xmpMM:DocumentID=doc-abc-123", "-XMP-xmpMM:OriginalDocumentID=orig-doc-xyz",
		path,
	}
	tagCmd := exec.Command(exiftoolPath, tagArgs...)
	if out, err := tagCmd.CombinedOutput(); err != nil {
		t.Fatalf("tag fixture jpeg: %v\n%s", err, out)
	}
	return path
}

// TestExifFixtureProducesExpectedNodeMetadata is the #33 e2e: a real tagged
// JPEG walked and scanned by the full pipeline (indexer.Walk -> workers.Pool
// -> processFile's probe.Exif -> Commit) must land its EXIF fields in
// node_metadata under source='exiftool'.
func TestExifFixtureProducesExpectedNodeMetadata(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	imgPath := makeTaggedFixtureJPEG(t, root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q (last_error=%v)", job.State, job.LastError)
	}

	node := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, filepath.Base(imgPath)))
	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	var makeVal string
	for _, r := range rows {
		if r.Source == "exiftool" && r.Key == "EXIF:Make" {
			makeVal = r.Value
		}
	}
	if makeVal != "SONY" {
		t.Errorf("EXIF:Make = %q, want SONY (all rows: %+v)", makeVal, rows)
	}
}

// makeFixtureMP4 generates a tiny real H.264/AAC video via ffmpeg, so the
// e2e test exercises the actual probe subprocess path rather than a canned
// fixture file checked into the repo. Mirrors internal/probe's fixture.
func makeFixtureMP4(t *testing.T, dir string) string {
	t.Helper()
	requireTool(t, "ffmpeg")

	path := filepath.Join(dir, "fixture.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
		"-c:v", "libx264", "-c:a", "aac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture mp4: %v\n%s", err, out)
	}
	return path
}

// TestFFProbeVideoFixturePersistsThroughScan is the #34 e2e: a real video
// walked and scanned by the full pipeline (indexer.Walk -> workers.Pool ->
// processFile's probe.FFProbe -> Commit) must land its ffprobe stream
// fields in node_metadata under source='ffprobe'.
func TestFFProbeVideoFixturePersistsThroughScan(t *testing.T) {
	requireTool(t, "ffprobe")
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	vidPath := makeFixtureMP4(t, root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q (last_error=%v)", job.State, job.LastError)
	}

	node := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, filepath.Base(vidPath)))
	rows, err := database.Reader.ListNodeMetadata(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListNodeMetadata: %v", err)
	}
	var codecVal string
	for _, r := range rows {
		if r.Source == "ffprobe" && r.Key == "video_codec" {
			codecVal = r.Value
		}
	}
	if codecVal != "h264" {
		t.Errorf("video_codec = %q, want h264 (all rows: %+v)", codecVal, rows)
	}
}

func TestPerceptualHashExtractionInScan(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	path := filepath.Join(root, "sample.png")
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sample.png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatalf("encode sample.png: %v", err)
	}
	_ = f.Close()

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-phash", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q (last_error=%v)", job.State, job.LastError)
	}

	node := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "sample.png"))
	if !node.Phash.Valid {
		t.Errorf("node.Phash.Valid = false, want valid pHash")
	}
}

func TestPerceptualHashDisabledInScan(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	root := t.TempDir()
	path := filepath.Join(root, "sample.png")
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sample.png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatalf("encode sample.png: %v", err)
	}
	_ = f.Close()

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	deps.DisablePerceptualHash = true
	loc := storage.Location{ID: locationID, Name: "test-phash-disabled", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, loc)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if job := waitJobDone(t, database, jobID); job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q (last_error=%v)", job.State, job.LastError)
	}

	node := mustGetLiveNode(t, database, filepath.Join(resolvedRoot, "sample.png"))
	if node.Phash.Valid {
		t.Errorf("node.Phash.Valid = true, want false when DisablePerceptualHash is true")
	}
}

// TestDrainAndCommitRunsConcurrentlyWithWalk is the #93 regression: before
// this fix, runScan ran the entire walk to completion before drainAndCommit
// started consuming `results`. Once the results channel (cap batchSize*2)
// filled, every pool worker blocked sending its result, the pool's own
// queue then filled behind them, and Submit started refusing -- silently
// capping a scan at roughly the first
// `cap(results) + queue depth + worker count` files and reporting the rest
// as failed instead of indexing them.
//
// An earlier version of this test tried to reproduce that capacity ceiling
// directly (many files, a small queueDepth, asserting FilesFailed == 0).
// That approach doesn't work: on a warm temp directory, os.Lstat is so much
// faster than one worker's hash-plus-DB-check that the entire walk can
// enumerate hundreds of files before a single job completes, regardless of
// whether draining is concurrent -- queueDepth alone then decides the
// outcome, and old and new code failed or passed identically depending
// only on queueDepth, never distinguishing the fix (confirmed empirically
// by reverting this fix locally and re-running the same test). Production
// hits the real ceiling at ~1150 files because real filesystem enumeration
// has enough per-entry latency for worker throughput to matter within one
// walk; a fast unit test can't reproduce that latency without an
// artificial delay, and an artificial delay just moves the flakiness from
// "how many files" to "how many milliseconds," which proved just as
// sensitive to `-race`/coverage instrumentation overhead.
//
// This test instead proves the actual property directly: it gates the walk
// mid-flight (via WalkFn) and polls, from inside the still-running walk
// callback, for evidence that drainAndCommit has already committed at
// least one batch. That is only possible if drainAndCommit is running
// concurrently with the walk -- under the pre-fix code, drainAndCommit is
// never even invoked until walkFn returns, and walkFn is blocked in this
// very poll, so the poll would time out deterministically every time, not
// as a matter of relative speed. The bound is generous (15s) specifically
// so this is a correctness assertion, not a race.
func TestDrainAndCommitRunsConcurrentlyWithWalk(t *testing.T) {
	const workerCount = 4
	const queueDepth = 300 // generous: queue capacity isn't what this test exercises
	const preGateCount = 5 // enough for flush() to have something to commit; batchInterval's ticker doesn't need a full batch
	const postGateCount = 20
	const total = preGateCount + postGateCount

	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	paths := make([]string, total)
	for i := 0; i < total; i++ {
		p := filepath.Join(resolvedRoot, fmt.Sprintf("file-%04d.txt", i))
		writeFile(t, p, fmt.Sprintf("content-%d", i))
		paths[i] = p
	}

	database := openTestDB(t)
	ctx := context.Background()

	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDepsN(t, database, resolvedRoot, locationID, workerCount, queueDepth)

	// RunScan (below) creates the scan_jobs row and returns its id
	// synchronously, before this WalkFn is ever invoked (it only runs
	// inside runScan's background goroutine) -- so by the time the gate
	// below needs jobID, it's already been sent.
	jobIDCh := make(chan int64, 1)
	deps.WalkFn = func(_ context.Context, _ string, onFile func(indexer.Record) error) error {
		for i, p := range paths {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if err := onFile(indexer.Record{Path: p, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			if i != preGateCount-1 {
				continue
			}
			// Everything needed for at least one commit has now been
			// submitted to the pool. Block this walk goroutine here --
			// walkFn has therefore NOT returned -- and poll for proof
			// that a batch was already committed.
			jobID := <-jobIDCh
			deadline := time.Now().Add(15 * time.Second)
			committed := false
			for time.Now().Before(deadline) {
				if job, err := database.Reader.GetScanJob(context.Background(), jobID); err == nil && job.FilesHashed > 0 {
					committed = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !committed {
				return fmt.Errorf("no batch was committed within 15s while the walk was still in progress -- drainAndCommit is not running concurrently with the walk")
			}
		}
		return nil
	}
	location := storage.Location{ID: locationID, Name: "test-concurrent-drain", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	jobIDCh <- jobID

	job := waitJobDoneWithin(t, database, jobID, 30*time.Second)
	if job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q, want COMPLETED (last_error=%v) -- a FAILED state here with the gate's own message means drainAndCommit did not run concurrently with the walk", job.State, job.LastError)
	}
	if job.FilesSeen != int64(total) {
		t.Errorf("FilesSeen = %d, want %d", job.FilesSeen, total)
	}
	if job.FilesFailed != 0 {
		t.Errorf("FilesFailed = %d, want 0", job.FilesFailed)
	}
	if job.FilesHashed != int64(total) {
		t.Errorf("FilesHashed = %d, want %d", job.FilesHashed, total)
	}

	// Every file must actually be committed, not just counted.
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("file-%04d.txt", i)
		path := filepath.Join(resolvedRoot, name)
		if _, err := database.Reader.GetLiveNodeByPath(ctx, path); err != nil {
			t.Errorf("node for %s not found: %v", name, err)
		}
	}
}

// TestScanFinishesWhenPoolShutsDownMidWalk is the #92 end-to-end regression:
// cmd/branchdam's shutdown sequence cancels the pool's Run context and then
// joins ScanTracker.Wait() before the deferred db.Close() -- this test
// simulates exactly that against a scan that's still mid-walk when shutdown
// begins, and asserts the join actually terminates (not a hang) with the
// job in a terminal state, rather than trusting the ordering by inspection
// alone.
//
// Like TestDrainAndCommitRunsConcurrentlyWithWalk (#93), this uses a gated
// WalkFn instead of real timing: the walk cancels the pool's context after
// submitting a handful of files, which is a real mid-walk shutdown, not a
// simulated race. Its termination is genuinely deterministic (bounded, not
// just "usually fast") because Pool.Submit and the pool's own shutdown-drain
// are serialized through the same mutex -- see
// internal/workers/pool.go's Submit/closeOnDone doc comments, and
// internal/workers/pool_test.go's
// TestPoolSubmitNeverOrphansAJobUnderConcurrentShutdown, which reproduces
// (against an intentionally-reverted Submit) the TOCTOU race that would
// otherwise make this test's outcome depend on goroutine-scheduling luck.
func TestScanFinishesWhenPoolShutsDownMidWalk(t *testing.T) {
	const workerCount = 2
	const queueDepth = 300 // generous: this test is about shutdown termination, not queue capacity
	const preGateCount = 5
	const postGateCount = 20
	const total = preGateCount + postGateCount

	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	paths := make([]string, total)
	for i := 0; i < total; i++ {
		p := filepath.Join(resolvedRoot, fmt.Sprintf("file-%04d.txt", i))
		writeFile(t, p, fmt.Sprintf("content-%d", i))
		paths[i] = p
	}

	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedPipelineLocation(t, database, resolvedRoot)

	poolCtx, cancelPool := context.WithCancel(context.Background())
	pool := workers.New[string](workerCount, queueDepth)
	pool.Run(poolCtx)
	t.Cleanup(func() {
		cancelPool()
		pool.Drain()
	})

	tracker := &ScanTracker{}
	deps := ScanDeps{
		DB:             database,
		Guard:          storage.NewGuard([]storage.Location{{ID: locationID, Name: "test-shutdown", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}}),
		Prober:         probe.New(),
		Pool:           pool,
		FullHashPolicy: "never",
		Tracker:        tracker,
	}
	deps.WalkFn = func(_ context.Context, _ string, onFile func(indexer.Record) error) error {
		for i, p := range paths {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if err := onFile(indexer.Record{Path: p, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			if i == preGateCount-1 {
				// Simulate cmd/branchdam's shutdown starting here, mid-walk:
				// the pool's Run context is cancelled while the walk still
				// has postGateCount files left to enumerate.
				cancelPool()
			}
		}
		return nil
	}
	location := storage.Location{ID: locationID, Name: "test-shutdown", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	// Mirrors cmd/branchdam/main.go's shutdown ordering: join the tracker
	// before anything that could touch a closed DB. The database here is
	// never actually closed early -- what this asserts is that the join
	// itself terminates, which is the property a closed-DB write or a
	// permanent hang would both violate.
	trackerDone := make(chan struct{})
	go func() {
		tracker.Wait()
		close(trackerDone)
	}()
	select {
	case <-trackerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ScanTracker.Wait() did not return within 10s after a mid-walk pool shutdown -- the scan is hung")
	}

	pool.Drain() // should return promptly: tracker.Wait() already implies every job this scan submitted has resolved

	job, err := database.Reader.GetScanJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetScanJob: %v", err)
	}
	if job.State == "RUNNING" {
		t.Fatalf("scan job state = RUNNING after shutdown joined cleanly, want a terminal state (COMPLETED or FAILED)")
	}
	if job.FilesSeen != int64(total) {
		t.Errorf("FilesSeen = %d, want %d -- the walk itself always completes (it observes cancelPool but doesn't stop), only Submit outcomes change at shutdown", job.FilesSeen, total)
	}
	if got := job.FilesHashed + job.FilesFailed; got != int64(total) {
		t.Errorf("FilesHashed(%d) + FilesFailed(%d) = %d, want %d -- every file the walk saw must resolve to exactly one outcome; a mismatch means a file was lost during the mid-walk shutdown", job.FilesHashed, job.FilesFailed, got, total)
	}
}

// seedScanJob creates a bare scan_jobs row directly, for tests that drive
// drainAndCommit itself rather than going through RunScan.
func seedScanJob(t *testing.T, database *db.DB, locationID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var jobID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		job, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: locationID, Valid: true},
			Kind:              "FULL_SCAN",
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed scan job: %v", err)
	}
	return jobID
}

// TestDrainAndCommitReportsFilesSeenOnEmptyTick backs #90 cause A: with
// hashWorkers capped and a single video's probes able to run for tens of
// seconds, a scan can go a long stretch between committed batches even
// while the walk itself keeps advancing -- filesSeen (incremented by the
// walk callback, independent of hashing) must still reach scan_jobs on the
// batchInterval ticker, not only when a batch actually commits. Drives
// drainAndCommit directly (same package) with a results channel nothing is
// ever sent on, so every observed progress write can only have come from an
// empty-buffer ticker tick, not a commit.
func TestDrainAndCommitReportsFilesSeenOnEmptyTick(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedPipelineLocation(t, database, t.TempDir())
	jobID := seedScanJob(t, database, locationID)

	results := make(chan Result)
	var filesSeen, filesFailed atomic.Int32
	filesSeen.Store(7) // the walk has "seen" 7 files; none have been hashed or committed yet
	uncertain := newUncertainPaths()

	var nudges atomic.Int32
	deps := ScanDeps{DB: database, Nudge: func() { nudges.Add(1) }}

	done := make(chan Stats, 1)
	go func() {
		done <- drainAndCommit(ctx, deps, locationID, jobID, results, &filesSeen, &filesFailed, uncertain, deps.logOrDiscard())
	}()

	// Poll for both the DB write and the nudge together -- flush() does the
	// InTx commit and then calls Nudge as two sequential, unsynchronized
	// steps in the producer goroutine, so breaking on FilesSeen alone and
	// only then reading nudges could observe the write before Nudge has
	// run yet, even though it always follows shortly after.
	deadline := time.Now().Add(5 * time.Second)
	var job sqlcgen.ScanJob
	var err error
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, jobID)
		if err == nil && job.FilesSeen == 7 && nudges.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.FilesSeen != 7 {
		t.Fatalf("files_seen was never persisted from an empty-buffer ticker tick (last read: %d)", job.FilesSeen)
	}
	if nudges.Load() == 0 {
		t.Error("Nudge was never called for a files_seen-only progress report")
	}

	// A further tick with nothing new (filesSeen unchanged, still no
	// results) must not write or nudge again -- only advancing state
	// warrants a report.
	nudgesAfterFirstReport := nudges.Load()
	time.Sleep(3 * batchInterval)
	if got := nudges.Load(); got != nudgesAfterFirstReport {
		t.Errorf("nudges = %d after idle ticks, want %d (no new state to report)", got, nudgesAfterFirstReport)
	}

	close(results)
	<-done
}

// testFixedParentResolver is a test-only graph.Resolver that always
// proposes parentPath as the parent of any other node in the same scan --
// used to force a real edge through the real graph.Engine (not a fake),
// so TestScanPersistsEdgesCreated exercises the actual
// resolveEdgesForBatch/ResolveAndCommit path #90 wires scan_jobs.edges_created
// through, not just drainAndCommit's bookkeeping in isolation.
type testFixedParentResolver struct{ parentPath string }

func (testFixedParentResolver) Name() string { return "test-fixed-parent" }
func (testFixedParentResolver) Tier() int    { return 1 }
func (r testFixedParentResolver) Resolve(ctx context.Context, child graph.Node, lookup graph.Lookup) ([]graph.Candidate, error) {
	if child.FilePath == r.parentPath {
		return nil, nil // don't propose the parent as its own parent
	}
	parent, err := lookup.ByPath(ctx, r.parentPath)
	if err != nil || parent == nil {
		return nil, nil
	}
	return []graph.Candidate{{
		ParentID: parent.ID, ChildID: child.ID, Rel: "DERIVED_FROM",
		Confidence: 0.95, Tier: 1, Resolver: "test-fixed-parent", Evidence: map[string]any{},
	}}, nil
}

// TestScanPersistsEdgesCreated backs #90's third fix: edges_created must
// reflect real edges the scan's graph.Engine pass creates, not stay
// permanently 0. Runs two scans over the same two files: the first creates
// the edge (edges_created == 1), the second re-resolves the same pair --
// UpsertMediaEdge refreshes the existing row rather than inserting a new
// one, so edges_created on the second scan must be 0, not another 1.
func TestScanPersistsEdgesCreated(t *testing.T) {
	root := t.TempDir()
	parentPath := filepath.Join(root, "parent.jpg")
	childPath := filepath.Join(root, "child.jpg")
	writeFile(t, parentPath, "parent content")
	writeFile(t, childPath, "child content")

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	resolvedParentPath, err := filepath.EvalSymlinks(parentPath)
	if err != nil {
		t.Fatalf("resolve parent path: %v", err)
	}

	database := openTestDB(t)
	ctx := context.Background()
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	deps.Engine = graph.NewEngine(database, nil, testFixedParentResolver{parentPath: resolvedParentPath})
	location := storage.Location{ID: locationID, Name: "test-edges-created", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	jobID, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan (first): %v", err)
	}
	job := waitJobDone(t, database, jobID)
	if job.State != "COMPLETED" {
		t.Fatalf("first scan state = %q, want COMPLETED (last_error=%v)", job.State, job.LastError)
	}
	if job.EdgesCreated != 1 {
		t.Errorf("first scan EdgesCreated = %d, want 1", job.EdgesCreated)
	}

	jobID2, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan (second): %v", err)
	}
	job2 := waitJobDone(t, database, jobID2)
	if job2.State != "COMPLETED" {
		t.Fatalf("second scan state = %q, want COMPLETED (last_error=%v)", job2.State, job2.LastError)
	}
	if job2.EdgesCreated != 0 {
		t.Errorf("second scan EdgesCreated = %d, want 0 (the edge already existed from the first scan)", job2.EdgesCreated)
	}
}

// TestProjectSidecarScanIntegration verifies end-to-end scanner pipeline
// integration with ProjectSidecarResolver, ensuring that scanning a storage
// location containing project sidecars (.dam.json, .edl) automatically resolves
// and persists PROJECT_SIDECAR edges into the database.
func TestProjectSidecarScanIntegration(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval symlinks root: %v", err)
	}

	mediaDir := filepath.Join(resolvedRoot, "01_media")
	projDir := filepath.Join(resolvedRoot, "projects")

	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatalf("mkdir media: %v", err)
	}
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	mediaPath := filepath.Join(mediaDir, "A001_C001.mov")
	damJsonPath := filepath.Join(projDir, "project.dam.json")
	edlPath := filepath.Join(projDir, "timeline.edl")

	writeFile(t, mediaPath, "raw video payload")
	damJsonContent := `{"version":"1.0","project_name":"Test","media_references":[{"raw_path":"../01_media/A001_C001.mov","role":"media"}]}`
	writeFile(t, damJsonPath, damJsonContent)

	edlContent := "TITLE: TEST\n* FROM CLIP WITH TRANSFER: ../01_media/A001_C001.mov\n"
	writeFile(t, edlPath, edlContent)

	database := openTestDB(t)
	ctx := context.Background()

	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)

	sidecarResolver := graph.NewProjectSidecarResolver(nil)
	deps.Engine = graph.NewEngine(database, nil, sidecarResolver)

	location := storage.Location{
		ID:       locationID,
		Name:     "test-sidecar-scan",
		RootPath: resolvedRoot,
		Tier:     "PROJECTS",
		ReadOnly: false,
	}

	jobID, err := RunScan(ctx, deps, location)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	job := waitJobDone(t, database, jobID)
	if job.State != "COMPLETED" {
		t.Fatalf("scan job state = %q, want COMPLETED (last_error=%v)", job.State, job.LastError)
	}
	if job.EdgesCreated != 2 {
		t.Fatalf("job.EdgesCreated = %d, want 2 PROJECT_SIDECAR edges (.dam.json and .edl)", job.EdgesCreated)
	}

	// Verify edges in database
	mediaNode, err := database.Reader.GetLiveNodeByPath(ctx, filepath.Join(resolvedRoot, "01_media", "A001_C001.mov"))
	if err != nil {
		t.Fatalf("GetLiveNodeByPath media: %v", err)
	}

	children, err := database.Reader.ListEdgesBySource(ctx, mediaNode.ID)
	if err != nil {
		t.Fatalf("ListEdgesBySource: %v", err)
	}

	if len(children) != 2 {
		t.Fatalf("expected 2 child edges for media node, got %d", len(children))
	}

	for _, edge := range children {
		if edge.RelationshipType != "PROJECT_SIDECAR" || edge.Confidence != 1.00 {
			t.Errorf("unexpected edge properties: %+v", edge)
		}
	}
}
