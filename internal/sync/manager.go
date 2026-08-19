// Package sync owns the remote_sync_state push state machine (spec Pillar 3:
// NOT_QUEUED -> PENDING_CLOUD_PUSH -> PUSHING -> PUSHED/PUSH_FAILED). It is
// deliberately outbound-HTTP-free: the actual remote call is injected as a
// PushFunc, so the state machine is testable without a network and the Immich
// client (#55) is a separate, pure concern.
package sync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// Remote service identifiers, mirroring remote_sync_state.remote's CHECK.
const (
	RemoteImmich       = "IMMICH"
	RemoteGooglePhotos = "GOOGLE_PHOTOS"
)

// DefaultMaxSyncRetries bounds RecoverFailedPushes' automatic re-claim of a
// PUSH_FAILED row (#182): past this many consecutive failures a
// misconfigured remote (an empty Immich libraryId, a dead API key) stops
// being retried every retryWindow forever and is left PUSH_FAILED
// permanently, matching internal/agent.DefaultMaxRetries' precedent. A human
// can still force a re-attempt via POST /api/v1/assets/{id}/sync/retry,
// which is an explicit operator action, not this automatic loop.
const DefaultMaxSyncRetries = 3

// ErrAlreadyPushed is returned by Enqueue when (node_id, remote) is already
// PUSHED or PUSHING -- the idempotent replay short-circuit. A caller that
// does not treat this as an error should skip it via errors.Is.
var ErrAlreadyPushed = errors.New("sync: node already pushed to remote")

// Node is the subset of a media_nodes row the state machine reasons about.
type Node struct {
	ID int64
	// Checksum is the node's fast_hash (xxHash64) -- the spec DDL's
	// "file_checksum at time of upload". It feeds pushKey.
	Checksum string
}

// pushKey derives the spec Pillar-3 idempotency key SHA256(node_checksum +
// target_service). This repo keys idempotency on the (node_id, remote)
// primary key instead -- node_id is structurally tied to content identity (a
// content change is a version collision that mints a NEW node_id), so the PK
// is a faithful, simpler stand-in and NO schema column is needed. pushKey is
// still computed in Enqueue and included in its debug log, purely as a
// documented cross-check for that reasoning -- it is NOT compared or
// asserted against anything at runtime; the (node_id, remote) PK is what
// actually enforces idempotency. This is stated explicitly so the absence of
// a real assertion isn't mistaken for an oversight.
func pushKey(nodeChecksum, remote string) string {
	sum := sha256.Sum256([]byte(nodeChecksum + "|" + remote))
	return hex.EncodeToString(sum[:])
}

// PushFunc performs the actual outbound push for a batch of claimed nodes.
// It is injected so the state machine stays HTTP-free. The Immich client
// (#55) is the production implementation; tests use a recording stub.
// Returning nil causes every node in the batch to be marked PUSHED; an error
// marks them all PUSH_FAILED.
type PushFunc func(ctx context.Context, nodes []Node) error

// Manager owns the remote_sync_state transitions. It never speaks HTTP.
type Manager struct {
	db  *db.DB
	log *slog.Logger
}

func NewManager(database *db.DB, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{db: database, log: log}
}

// Enqueue claims a node for push to remote. Idempotent: if (node_id, remote)
// is already PUSHED or PUSHING it is a no-op returning ErrAlreadyPushed, and
// an already-PENDING row is left untouched -- replaying the same logical push
// never queues a duplicate or re-orders the FIFO. A PUSH_FAILED row is
// re-claimed (retry).
//
// The look-up and the upsert run inside ONE write transaction on the writer
// pool's single connection, so a concurrent ProcessPending that claims and
// pushes the same row cannot interleave between them and regress it back to
// PENDING (which would duplicate the push).
func (m *Manager) Enqueue(ctx context.Context, node Node, remote string) error {
	key := pushKey(node.Checksum, remote)
	return m.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetRemoteSyncState(ctx, sqlcgen.GetRemoteSyncStateParams{
			NodeID: node.ID, Remote: remote,
		})
		if err == nil {
			switch row.SyncStatus {
			case "PUSHED", "PUSHING":
				m.log.Debug("sync: enqueue is a no-op (already in flight or pushed)",
					"nodeID", node.ID, "remote", remote, "pushKey", key)
				return ErrAlreadyPushed
			case "PENDING_CLOUD_PUSH":
				return nil // already queued -- no-op, leave last_attempt_at alone
			}
			// PUSH_FAILED -- fall through to re-claim below.
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sync: get remote sync state: %w", err)
		}
		_, err = q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: node.ID, Remote: remote, SyncStatus: "PENDING_CLOUD_PUSH",
			RemoteAssetID: sql.NullString{}, LastError: sql.NullString{},
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		})
		return err
	})
}

// ProcessPending claims up to batchSize PENDING_CLOUD_PUSH rows for remote,
// marks them PUSHING, invokes push once for the whole batch, and marks every
// node PUSHED (or PUSH_FAILED on error) -- on failure the WHOLE batch is
// marked PUSH_FAILED, not just whichever node push actually failed on.
// Returns how many nodes were attempted.
//
// The all-or-nothing failure marking is correct for today's only PushFunc
// (Immich's whole-library TriggerScan, wired via Worker -- see its drain doc
// comment): a single call either covers every claimed node or none of them,
// so there is no per-node result to attribute a failure to. A future
// genuinely-per-node PushFunc would need ProcessPending itself reworked to
// track per-node outcomes; reusing this batch-wide marking as-is would
// re-mark already-succeeded nodes PUSH_FAILED alongside a real failure.
//
// The claim is atomic with the selection: both happen inside ONE write
// transaction on the writer pool's single connection, so a concurrent second
// ProcessPending cannot see the same rows still PENDING and double-trigger.
// (The worker is additionally a single goroutine per remote, but the atomic
// claim is what makes the guarantee hold even under an accidental second
// worker.) The batch is scoped to `remote` so a node's other-remote rows are
// never touched.
func (m *Manager) ProcessPending(ctx context.Context, remote string, batchSize int, push PushFunc) (int, error) {
	if batchSize <= 0 {
		batchSize = 1
	}

	var claimed []int64
	err := m.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListRemoteSyncStateByStatus(ctx, sqlcgen.ListRemoteSyncStateByStatusParams{
			Remote: remote, SyncStatus: "PENDING_CLOUD_PUSH", Limit: int64(batchSize),
		})
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
				NodeID: r.NodeID, Remote: remote, SyncStatus: "PUSHING",
				RemoteAssetID: sql.NullString{}, LastError: sql.NullString{},
				LastAttemptAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
			}); err != nil {
				return err
			}
			claimed = append(claimed, r.NodeID)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sync: claim pending batch: %w", err)
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	nodes := make([]Node, len(claimed))
	for i, id := range claimed {
		nodes[i] = Node{ID: id}
	}

	if err := push(ctx, nodes); err != nil {
		msg := err.Error()
		if merr := m.setBatchStatus(ctx, claimed, remote, "PUSH_FAILED", "", &msg); merr != nil {
			m.log.Error("sync: mark batch failed", "err", merr)
		}
		return len(claimed), err
	}

	if err := m.setBatchStatus(ctx, claimed, remote, "PUSHED", "", nil); err != nil {
		return len(claimed), fmt.Errorf("sync: mark batch pushed: %w", err)
	}
	return len(claimed), nil
}

// RecoverStalePushing resets PUSHING rows older than olderThan back to
// PENDING_CLOUD_PUSH, so a crash mid-push doesn't strand nodes forever.
// Returns how many rows were recovered. Call once at startup.
func (m *Manager) RecoverStalePushing(ctx context.Context, remote string, olderThan time.Duration) (int64, error) {
	var n int64
	err := m.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.ResetRemoteSyncStateStale(ctx, sqlcgen.ResetRemoteSyncStateStaleParams{
			Remote:        remote,
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-olderThan).Unix(), Valid: true},
		})
		return err
	})
	return n, err
}

// RecoverFailedPushes resets PUSH_FAILED rows older than olderThan back to
// PENDING_CLOUD_PUSH, so a transient remote failure doesn't strand a batch
// forever -- this is the worker-level retry, bounded in frequency by
// olderThan (the Immich client's own retry is the per-attempt backoff) and
// in count by maxRetries: a row whose retry_count has already reached
// maxRetries is left PUSH_FAILED rather than re-claimed again (#182).
// maxRetries <= 0 falls back to DefaultMaxSyncRetries.
func (m *Manager) RecoverFailedPushes(ctx context.Context, remote string, olderThan time.Duration, maxRetries int) (int64, error) {
	if maxRetries <= 0 {
		maxRetries = DefaultMaxSyncRetries
	}
	var n int64
	err := m.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.ResetRemoteSyncStateFailed(ctx, sqlcgen.ResetRemoteSyncStateFailedParams{
			Remote:        remote,
			LastAttemptAt: sql.NullInt64{Int64: time.Now().Add(-olderThan).Unix(), Valid: true},
			RetryCount:    int64(maxRetries),
		})
		return err
	})
	return n, err
}

// CountExhaustedPushes returns how many PUSH_FAILED rows for remote have a
// retry_count at or past maxRetries -- rows RecoverFailedPushes will never
// automatically re-claim again (#182). maxRetries <= 0 falls back to
// DefaultMaxSyncRetries, matching RecoverFailedPushes' own fallback so the
// two stay measured against the same bound.
func (m *Manager) CountExhaustedPushes(ctx context.Context, remote string, maxRetries int) (int64, error) {
	if maxRetries <= 0 {
		maxRetries = DefaultMaxSyncRetries
	}
	return m.db.Reader.CountRemoteSyncStateExhausted(ctx, sqlcgen.CountRemoteSyncStateExhaustedParams{
		Remote: remote, RetryCount: int64(maxRetries),
	})
}

// setBatchStatus applies one status transition to a set of node ids inside a
// single write transaction.
func (m *Manager) setBatchStatus(ctx context.Context, nodeIDs []int64, remote, status, remoteAssetID string, lastErr *string) error {
	return m.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, id := range nodeIDs {
			switch status {
			case "PUSHED":
				if err := q.MarkRemoteSyncStatePushed(ctx, sqlcgen.MarkRemoteSyncStatePushedParams{
					NodeID: id, Remote: remote,
					RemoteAssetID: sql.NullString{String: remoteAssetID, Valid: remoteAssetID != ""},
				}); err != nil {
					return err
				}
			case "PUSH_FAILED":
				msg := "sync: push failed"
				if lastErr != nil {
					msg = *lastErr
				}
				if err := q.MarkRemoteSyncStateFailed(ctx, sqlcgen.MarkRemoteSyncStateFailedParams{
					NodeID: id, Remote: remote,
					LastError: sql.NullString{String: msg, Valid: true},
				}); err != nil {
					return err
				}
			default: // PENDING_CLOUD_PUSH / PUSHING -- create or update the row
				if _, err := q.UpsertRemoteSyncState(ctx, sqlcgen.UpsertRemoteSyncStateParams{
					NodeID: id, Remote: remote, SyncStatus: status,
					RemoteAssetID: sql.NullString{}, LastError: sql.NullString{},
					LastAttemptAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
