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

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Wait until the worker's first drain has pushed the node. This proves
	// Run's goroutine is genuinely in flight before Wait below, so the
	// WaitGroup's Add has definitely happened (and Wait can't vacuously
	// return against a counter that never got added to).
	waitFor(t, 5*time.Second, func() bool {
		rows, err := database.Reader.ListRemoteSyncStateByStatus(context.Background(), sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
		return err == nil && len(rows) == 1
	})

	cancel()

	// Wait must join Run promptly on ctx cancellation -- this is what main.go
	// relies on to drain the worker before db.Close.
	waited := make(chan struct{})
	go func() { w.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Worker.Wait did not return within 2s of ctx cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancellation")
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

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Wait until the export-path node is actually PUSHED (push is invoked
	// before the mark-pushed transaction commits, so waiting on the DB state,
	// not the call count, is what guarantees the batch finished).
	waitFor(t, 5*time.Second, func() bool {
		rows, err := database.Reader.ListRemoteSyncStateByStatus(context.Background(), sqlcgen.ListRemoteSyncStateByStatusParams{Remote: RemoteImmich, SyncStatus: "PUSHED", Limit: 100})
		return err == nil && len(rows) == 1
	})
	cancel()
	<-done

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
