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
