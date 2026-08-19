package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sync.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func seedLocation(t *testing.T, database *db.DB, tier string, readOnly bool) int64 {
	t.Helper()
	var id int64
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		ro := int64(0)
		if readOnly {
			ro = 1
		}
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: tier + "-" + t.Name(), RootPath: t.TempDir(), Tier: tier, ReadOnly: ro, Prunable: 0,
		})
		id = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	return id
}

func seedNode(t *testing.T, database *db.DB, locID int64, path string) sqlcgen.MediaNode {
	t.Helper()
	ctx := context.Background()
	var node sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: fmt.Sprintf("uuid-%d", time.Now().UnixNano()), StorageLocationID: locID,
			FilePath: path, FileName: filepath.Base(path), FileExt: "jpg",
			SizeBytes: 1, MtimeUnix: time.Now().Unix(),
			FastHash:       &[]string{"aaaaaaaaaaaaaaaa"}[0],
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
			FilenameStem: sql.NullString{String: "node", Valid: true},
		})
		node = n
		return err
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return node
}

// recordingPush counts calls and records the batches it received.
type recordingPush struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (r *recordingPush) fn(_ context.Context, _ []Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.err
}

func TestPushKeyDeterministic(t *testing.T) {
	a := pushKey("aaaaaaaaaaaaaaaa", RemoteImmich)
	b := pushKey("aaaaaaaaaaaaaaaa", RemoteImmich)
	c := pushKey("bbbbbbbbbbbbbbbb", RemoteImmich)
	d := pushKey("aaaaaaaaaaaaaaaa", RemoteGooglePhotos)
	if a != b {
		t.Fatalf("same inputs produced different keys: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different checksums produced the same key")
	}
	if a == d {
		t.Errorf("different remotes produced the same key")
	}
}

func TestEnqueueIsIdempotentAtStateMachineLevel(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/shot.jpg")
	n := Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}

	if err := mgr.Enqueue(ctx, n, RemoteImmich); err != nil {
		t.Fatalf("Enqueue (1): %v", err)
	}
	if err := mgr.Enqueue(ctx, n, RemoteImmich); err != nil {
		t.Fatalf("Enqueue (2, same node) should be a no-op, got %v", err)
	}

	push := &recordingPush{}
	if _, err := mgr.ProcessPending(ctx, RemoteImmich, 10, push.fn); err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if push.calls != 1 {
		t.Fatalf("push calls = %d, want 1", push.calls)
	}

	if err := mgr.Enqueue(ctx, n, RemoteImmich); !errors.Is(err, ErrAlreadyPushed) {
		t.Fatalf("Enqueue after PUSHED = %v, want ErrAlreadyPushed", err)
	}
	if _, err := mgr.ProcessPending(ctx, RemoteImmich, 10, push.fn); err != nil {
		t.Fatalf("ProcessPending (2): %v", err)
	}
	if push.calls != 1 {
		t.Fatalf("push calls after replay = %d, want still 1 (no duplicate remote calls)", push.calls)
	}

	row, err := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if err != nil {
		t.Fatalf("GetRemoteSyncState: %v", err)
	}
	if row.SyncStatus != "PUSHED" {
		t.Errorf("sync_status = %q, want PUSHED", row.SyncStatus)
	}
}

func TestProcessPendingFailureMarksPushFailed(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/shot.jpg")
	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	push := &recordingPush{err: errors.New("network timeout")}
	if _, err := mgr.ProcessPending(ctx, RemoteImmich, 10, push.fn); err == nil {
		t.Fatalf("ProcessPending should propagate the push error")
	}

	row, err := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if err != nil {
		t.Fatalf("GetRemoteSyncState: %v", err)
	}
	if row.SyncStatus != "PUSH_FAILED" {
		t.Errorf("sync_status = %q, want PUSH_FAILED", row.SyncStatus)
	}
	if !row.LastError.Valid || row.LastError.String != "network timeout" {
		t.Errorf("last_error = %+v, want 'network timeout'", row.LastError)
	}

	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue (retry): %v", err)
	}
	row, _ = database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if row.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("sync_status after retry = %q, want PENDING_CLOUD_PUSH", row.SyncStatus)
	}
	if row.LastError.Valid {
		t.Errorf("last_error after retry = %q, want cleared", row.LastError.String)
	}
}

func TestEnqueuePendingIsNoOpAndPushingShortCircuits(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/shot.jpg")
	n := Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}

	if err := mgr.Enqueue(ctx, n, RemoteImmich); err != nil {
		t.Fatalf("Enqueue (1): %v", err)
	}
	// Backdate the PENDING row's last_attempt_at to a clearly-old value so the
	// "unchanged on no-op" assertion below is robust across unix-second
	// granularity (the claim query drains by last_attempt_at ASC).
	backdated := time.Now().Add(-time.Hour).Unix()
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PENDING_CLOUD_PUSH",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{},
			LastAttemptAt: sql.NullInt64{Int64: backdated, Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Re-enqueueing an already-PENDING row is a no-op and must NOT bump
	// last_attempt_at (re-ordering the FIFO) nor duplicate the row.
	if err := mgr.Enqueue(ctx, n, RemoteImmich); err != nil {
		t.Fatalf("Enqueue (2, pending) should be a no-op: %v", err)
	}
	after, _ := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if !after.LastAttemptAt.Valid || after.LastAttemptAt.Int64 != backdated {
		t.Errorf("last_attempt_at changed on a no-op enqueue: want %d, got %+v", backdated, after.LastAttemptAt)
	}

	// A PUSHING row short-circuits with ErrAlreadyPushed and leaves the row untouched.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PUSHING",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{},
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("force PUSHING: %v", err)
	}
	if err := mgr.Enqueue(ctx, n, RemoteImmich); !errors.Is(err, ErrAlreadyPushed) {
		t.Fatalf("Enqueue on PUSHING = %v, want ErrAlreadyPushed", err)
	}
	still, _ := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if still.SyncStatus != "PUSHING" {
		t.Errorf("row regressed by a short-circuited enqueue: sync_status = %q, want still PUSHING", still.SyncStatus)
	}
}

func TestProcessPendingIsRemoteScoped(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/shot.jpg")

	// One node legitimately holds rows for both remotes under the (node_id, remote) PK.
	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue IMMICH: %v", err)
	}
	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteGooglePhotos); err != nil {
		t.Fatalf("Enqueue GOOGLE_PHOTOS: %v", err)
	}

	push := &recordingPush{}
	if _, err := mgr.ProcessPending(ctx, RemoteImmich, 10, push.fn); err != nil {
		t.Fatalf("ProcessPending(IMMICH): %v", err)
	}

	// Only the IMMICH row may be claimed/pushed; GOOGLE_PHOTOS stays PENDING.
	immich, err := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if err != nil {
		t.Fatalf("GetRemoteSyncState(IMMICH): %v", err)
	}
	if immich.SyncStatus != "PUSHED" {
		t.Errorf("IMMICH sync_status = %q, want PUSHED", immich.SyncStatus)
	}
	gp, err := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteGooglePhotos})
	if err != nil {
		t.Fatalf("GetRemoteSyncState(GOOGLE_PHOTOS): %v", err)
	}
	if gp.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("GOOGLE_PHOTOS sync_status = %q, want PENDING_CLOUD_PUSH (must not be touched)", gp.SyncStatus)
	}
}

func TestRecoverStalePushingResetsStrandedRows(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/shot.jpg")
	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Manually strand it in PUSHING to model the crash window between claim and mark.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PUSHING",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{},
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-10 * time.Minute).Unix(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("strand: %v", err)
	}

	n, err := mgr.RecoverStalePushing(ctx, RemoteImmich, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStalePushing: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered = %d, want 1", n)
	}
	row, _ := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if row.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("sync_status after recovery = %q, want PENDING_CLOUD_PUSH", row.SyncStatus)
	}
}

func TestRecoverFailedPushesRespectsRetryBound(t *testing.T) {
	for name, tc := range map[string]struct {
		failures  int
		wantRecov int64
		wantFinal string
	}{
		"below the bound is re-claimed":  {failures: DefaultMaxSyncRetries - 1, wantRecov: 1, wantFinal: "PENDING_CLOUD_PUSH"},
		"at the bound stays PUSH_FAILED": {failures: DefaultMaxSyncRetries, wantRecov: 0, wantFinal: "PUSH_FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			database := openTestDB(t)
			mgr := NewManager(database, nil)
			ctx := context.Background()
			locID := seedLocation(t, database, "TIER2_EXPORTS", false)
			node := seedNode(t, database, locID, "/exports/immich/shot.jpg")

			if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			// Drive retry_count up via the real transition MarkRemoteSyncStateFailed
			// performs on every failure, not a direct column poke.
			for i := 0; i < tc.failures; i++ {
				if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
					return q.MarkRemoteSyncStateFailed(ctx, sqlcgen.MarkRemoteSyncStateFailedParams{
						NodeID: node.ID, Remote: RemoteImmich, LastError: sql.NullString{String: "boom", Valid: true},
					})
				}); err != nil {
					t.Fatalf("mark failed (%d): %v", i, err)
				}
			}
			// Backdate so the age window alone is never what gates the outcome here.
			if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
				_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
					NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PUSH_FAILED",
					RemoteAssetID: sql.NullString{}, LastError: sql.NullString{String: "boom", Valid: true},
					LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-1 * time.Hour).Unix(), Valid: true},
				})
				return err
			}); err != nil {
				t.Fatalf("backdate: %v", err)
			}

			n, err := mgr.RecoverFailedPushes(ctx, RemoteImmich, 0, DefaultMaxSyncRetries)
			if err != nil {
				t.Fatalf("RecoverFailedPushes: %v", err)
			}
			if n != tc.wantRecov {
				t.Errorf("recovered = %d, want %d", n, tc.wantRecov)
			}
			row, _ := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
			if row.SyncStatus != tc.wantFinal {
				t.Errorf("sync_status = %q, want %q", row.SyncStatus, tc.wantFinal)
			}
		})
	}
}

func TestMarkRemoteSyncStatePushedResetsRetryCount(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/shot.jpg")

	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Fail it up to (but not past) the bound, then succeed.
	for i := 0; i < DefaultMaxSyncRetries-1; i++ {
		if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.MarkRemoteSyncStateFailed(ctx, sqlcgen.MarkRemoteSyncStateFailedParams{
				NodeID: node.ID, Remote: RemoteImmich, LastError: sql.NullString{String: "boom", Valid: true},
			})
		}); err != nil {
			t.Fatalf("mark failed (%d): %v", i, err)
		}
	}
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkRemoteSyncStatePushed(ctx, sqlcgen.MarkRemoteSyncStatePushedParams{NodeID: node.ID, Remote: RemoteImmich})
	}); err != nil {
		t.Fatalf("mark pushed: %v", err)
	}

	// A later failure must start counting from zero again, not resume where
	// the pre-success streak left off -- otherwise a node that succeeds once
	// every few attempts would eventually get stuck at the bound anyway.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkRemoteSyncStateFailed(ctx, sqlcgen.MarkRemoteSyncStateFailedParams{
			NodeID: node.ID, Remote: RemoteImmich, LastError: sql.NullString{String: "boom again", Valid: true},
		})
	}); err != nil {
		t.Fatalf("mark failed after success: %v", err)
	}
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PUSH_FAILED",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{String: "boom again", Valid: true},
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-1 * time.Hour).Unix(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := mgr.RecoverFailedPushes(ctx, RemoteImmich, 0, DefaultMaxSyncRetries)
	if err != nil {
		t.Fatalf("RecoverFailedPushes: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered = %d, want 1 (retry_count reset to 0 by the intervening success)", n)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within", timeout)
}

func TestWorkerStopsOnCancel(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	exportPath := filepath.Join(root, "immich")
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	seedNode(t, database, locID, filepath.Join(exportPath, "shot.jpg")) // under export path -> enqueued by the first drain

	w := NewWorker(mgr, RemoteImmich, exportPath, 16, time.Hour, func(_ context.Context, _ []Node) error { return nil }, nil)

	w.Start(ctx)

	// Wait until the worker's first drain has pushed the node -- proof the run
	// goroutine is genuinely in flight before Wait below (Start already
	// incremented the WaitGroup synchronously, but this pins down that the loop
	// is doing real work rather than about to return).
	waitFor(t, 5*time.Second, func() bool {
		rows, err := database.Reader.ListRemoteSyncStateByStatus(context.Background(), sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
		return err == nil && len(rows) == 1
	})

	cancel()

	// Wait must join the run goroutine promptly on ctx cancellation -- this is
	// what main.go relies on to drain the worker before db.Close.
	waited := make(chan struct{})
	go func() { w.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Worker.Wait did not return within 2s of ctx cancellation")
	}
}

// TestWorkerDrainCoalescesPushWithinOneTick covers #183: a backlog spanning
// several batchSize-sized claims must still trigger the injected push exactly
// once *within a single drain tick* (enqueueUntracked's discovery limit is a
// multiple of batchSize precisely so a backlog like this fits in one tick's
// enqueue snapshot -- see enqueueUntracked's doc comment).
func TestWorkerDrainCoalescesPushWithinOneTick(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()

	root := t.TempDir()
	exportPath := filepath.Join(root, "immich")
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	// 5 nodes, batchSize 2 -> ProcessPending needs 3 claim/mark sub-batches to
	// exhaust what one enqueueUntracked call discovers (limit = 2*10 = 20, so
	// all 5 are enqueued together), but drain's internal loop must still only
	// invoke the real push once.
	for i := 0; i < 5; i++ {
		seedNode(t, database, locID, filepath.Join(exportPath, fmt.Sprintf("shot-%d.jpg", i)))
	}

	push := &recordingPush{}
	w := NewWorker(mgr, RemoteImmich, exportPath, 2, time.Hour, push.fn, nil)

	w.drain(ctx)

	if push.calls != 1 {
		t.Errorf("push calls for one tick's 3 internal sub-batches = %d, want 1 (coalesced into one trigger)", push.calls)
	}
	rows, err := database.Reader.ListRemoteSyncStateByStatus(ctx, sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
	if err != nil {
		t.Fatalf("ListRemoteSyncStateByStatus: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("pushed rows = %d, want 5 (every node still marked PUSHED despite the coalesced trigger)", len(rows))
	}
}

// TestWorkerDrainNeverCoalescesAcrossTicks is the regression test for the bug
// a review caught in an earlier version of #183's fix: coalescing scoped to a
// "contiguous run of non-empty ticks" (spanning multiple drain() calls) could
// mark a node PUSHED via a later tick's no-op sub-batch even though that
// node's file only appeared on disk *after* the run's one real push already
// fired -- silently telling nobody at the remote about it. Two back-to-back
// non-empty ticks (no empty tick between them) must each still get their own
// real push call.
func TestWorkerDrainNeverCoalescesAcrossTicks(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()

	root := t.TempDir()
	exportPath := filepath.Join(root, "immich")
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	seedNode(t, database, locID, filepath.Join(exportPath, "shot-1.jpg"))

	push := &recordingPush{}
	w := NewWorker(mgr, RemoteImmich, exportPath, 16, time.Hour, push.fn, nil)

	w.drain(ctx) // tick 1: pushes shot-1 -> real trigger
	if push.calls != 1 {
		t.Fatalf("push calls after tick 1 = %d, want 1", push.calls)
	}

	// A second file lands before the next tick runs -- e.g. arriving on the
	// export mount between two 10s-interval ticks in production. The old
	// cross-run coalescing would have swallowed this into tick 1's trigger
	// via a no-op; it must instead get pushed for real.
	seedNode(t, database, locID, filepath.Join(exportPath, "shot-2.jpg"))
	w.drain(ctx) // tick 2: must push shot-2 for real, not silently mark it PUSHED
	if push.calls != 2 {
		t.Errorf("push calls after tick 2's new arrival = %d, want 2 (each tick's own backlog gets its own real trigger)", push.calls)
	}

	rows, err := database.Reader.ListRemoteSyncStateByStatus(ctx, sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
	if err != nil {
		t.Fatalf("ListRemoteSyncStateByStatus: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("pushed rows = %d, want 2", len(rows))
	}
}

// TestWorkerDrainRetriesAfterAFailedTrigger proves a failed push halts that
// tick's internal loop rather than masking the failure behind a later no-op
// sub-batch, and that the very next tick retries the real call.
func TestWorkerDrainRetriesAfterAFailedTrigger(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()

	root := t.TempDir()
	exportPath := filepath.Join(root, "immich")
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	seedNode(t, database, locID, filepath.Join(exportPath, "shot.jpg"))

	failing := &recordingPush{err: errors.New("transient 5xx")}
	w := NewWorker(mgr, RemoteImmich, exportPath, 16, time.Hour, failing.fn, nil)
	w.retryWindow = 0

	w.drain(ctx) // enqueues + attempts to push -> fails, PUSH_FAILED
	if failing.calls != 1 {
		t.Fatalf("push calls after first (failing) drain = %d, want 1", failing.calls)
	}

	// Backdate last_attempt_at, same as TestWorkerRetriesFailedPushes: without
	// it, unixepoch()'s 1s granularity can make last_attempt_at < now false
	// within the same wall-clock second, independent of anything this test
	// is actually about.
	node, err := database.Reader.GetLiveNodeByPath(ctx, filepath.Join(exportPath, "shot.jpg"))
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PUSH_FAILED",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{String: "transient 5xx", Valid: true},
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-1 * time.Hour).Unix(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	w.drain(ctx) // retries the same node -> must call push again, not skip it
	if failing.calls != 2 {
		t.Errorf("push calls after a retried drain = %d, want 2 (a failed trigger must not be coalesced away)", failing.calls)
	}
}

func TestWorkerEnqueuesAndPushesUntrackedNodes(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	exportPath := filepath.Join(root, "immich")
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	seedNode(t, database, locID, filepath.Join(exportPath, "shot.jpg")) // under export path -> enqueued
	seedNode(t, database, locID, filepath.Join(root, "other.jpg"))      // outside export path -> ignored

	push := &recordingPush{}
	w := NewWorker(mgr, RemoteImmich, exportPath, 16, 50*time.Millisecond, push.fn, nil)

	w.Start(ctx)

	// Wait until the export-path node is actually PUSHED (push is invoked
	// before the mark-pushed transaction commits, so waiting on the DB state,
	// not the call count, is what guarantees the batch finished).
	waitFor(t, 5*time.Second, func() bool {
		rows, err := database.Reader.ListRemoteSyncStateByStatus(context.Background(), sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
		return err == nil && len(rows) == 1
	})
	cancel()
	// Join the run goroutine so no worker DB writes are in flight while the
	// final assertions below read the DB.
	w.Wait()

	// Only the export-path node was pushed.
	rows, err := database.Reader.ListRemoteSyncStateByStatus(context.Background(), sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
	if err != nil {
		t.Fatalf("ListRemoteSyncStateByStatus: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("pushed rows = %d, want 1 (only the export-path node)", len(rows))
	}
}

func TestWorkerRetriesFailedPushes(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	exportPath := filepath.Join(root, "immich")
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, filepath.Join(exportPath, "shot.jpg"))

	// Enqueue + force a failure so the node lands in PUSH_FAILED.
	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	failing := &recordingPush{err: errors.New("transient 5xx")}
	if _, err := mgr.ProcessPending(ctx, RemoteImmich, 10, failing.fn); err == nil {
		t.Fatal("expected the simulated push failure")
	}
	row, _ := database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if row.SyncStatus != "PUSH_FAILED" {
		t.Fatalf("seed status = %q, want PUSH_FAILED", row.SyncStatus)
	}

	// Backdate the failed attempt so the worker's retry window re-claims it.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: RemoteImmich, SyncStatus: "PUSH_FAILED",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{String: "transient", Valid: true},
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-1 * time.Hour).Unix(), Valid: true},
		})
		return err
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// A worker drain with an immediate retry window re-claims and pushes it.
	push := &recordingPush{}
	w := NewWorker(mgr, RemoteImmich, exportPath, 16, time.Hour, push.fn, nil)
	w.retryWindow = 0
	w.drain(ctx)

	row, _ = database.Reader.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: RemoteImmich})
	if row.SyncStatus != "PUSHED" {
		t.Errorf("after retry sync_status = %q, want PUSHED", row.SyncStatus)
	}
}
