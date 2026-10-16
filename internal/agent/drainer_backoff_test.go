package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// fakeClock is an injectable drainer clock (agent.WithClock) so tests can
// walk a backoff schedule spanning minutes or hours without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_790_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func eventRow(t *testing.T, database *db.DB, eventUUID string) sqlcgen.GetAgentEventByUUIDRow {
	t.Helper()
	ev, err := database.Reader.GetAgentEventByUUID(context.Background(), eventUUID)
	require.NoError(t, err)
	return ev
}

func TestRetryDelay_ExponentialAndCapped(t *testing.T) {
	require.Equal(t, 5*time.Second, agent.RetryDelay(1))
	require.Equal(t, 10*time.Second, agent.RetryDelay(2))
	require.Equal(t, 20*time.Second, agent.RetryDelay(3))
	require.Equal(t, 2560*time.Second, agent.RetryDelay(10))
	require.Equal(t, time.Hour, agent.RetryDelay(11))
	require.Equal(t, time.Hour, agent.RetryDelay(19))
	require.Equal(t, time.Hour, agent.RetryDelay(1000))
	require.Equal(t, 20, agent.DefaultMaxRetries)
}

// TestDrainer_ArchiveFileNotYetPresent_SurvivesFiveMinuteCopy is the
// audit's case: the workstation agent sends EVENT_PATH_REBASED for a Tier-3
// target while it is still copying a large file into the archive. The
// rebase fails with the transient ErrArchiveFileNotYetPresent until the
// bytes land. Retried back to back (the old behavior: three attempts on
// consecutive ~2s drain ticks) it was FAILED permanently within seconds;
// with the exponential backoff it must still be PENDING when a 5-minute
// copy finishes, and then succeed.
func TestDrainer_ArchiveFileNotYetPresent_SurvivesFiveMinuteCopy(t *testing.T) {
	env := setupTestDB(t)
	clock := newFakeClock()
	drainer := agent.NewDrainer(env.db, env.guard, nil, agent.WithClock(clock.Now))
	ctx := context.Background()

	nodeUUID := uuid.New().String()
	enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: nodeUUID, FilePath: filepath.Join(env.staging, "A001_C001.mov"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)

	tier3Path := filepath.Join(env.archive, "A001_C001.mov")
	rebase := enqueueEvent(t, env.db, agent.EventPathRebased, agent.PathRebasedPayload{
		NodeUUID: nodeUUID, TargetFilePath: tier3Path,
	})

	// Simulate the production drain loop (Start ticks every
	// DefaultDrainInterval) across a 5-minute copy.
	copyDone := clock.Now().Add(5 * time.Minute)
	for clock.Now().Before(copyDone) {
		stats, err := drainer.DrainAll(ctx)
		require.NoError(t, err)
		require.Zero(t, stats.Failed, "transient Tier-3 rebase failed permanently at %s into the copy", clock.Now().Sub(copyDone.Add(-5*time.Minute)))
		require.Equal(t, "PENDING", eventRow(t, env.db, rebase.EventUuid).Status)
		clock.Advance(agent.DefaultDrainInterval)
	}

	ev := eventRow(t, env.db, rebase.EventUuid)
	require.Equal(t, "PENDING", ev.Status)
	require.Less(t, ev.RetryCount, int64(agent.DefaultMaxRetries))
	require.Contains(t, ev.ErrorLog.String, "does not exist there yet")

	// The copy finishes. Within the current backoff window the drainer
	// picks the row up again and the rebase succeeds.
	require.NoError(t, os.WriteFile(tier3Path, []byte("all the bytes"), 0o644))
	deadline := clock.Now().Add(agent.RetryDelay(int(ev.RetryCount)) + agent.DefaultDrainInterval)
	for eventRow(t, env.db, rebase.EventUuid).Status == "PENDING" {
		require.True(t, clock.Now().Before(deadline), "rebase never retried after the copy finished")
		_, err := drainer.DrainAll(ctx)
		require.NoError(t, err)
		clock.Advance(agent.DefaultDrainInterval)
	}
	require.Equal(t, "PROCESSED", eventRow(t, env.db, rebase.EventUuid).Status)

	node, err := env.db.Reader.GetMediaNodeByUUID(ctx, nodeUUID)
	require.NoError(t, err)
	require.Equal(t, tier3Path, node.FilePath)
	require.Equal(t, env.locID3, node.StorageLocationID)
}

// TestDrainer_TransientFailureIsDeferredNotRetriedImmediately pins the
// next_attempt_at gate: a deferred row is invisible to the drainer until its
// backoff elapses, and does not block rows behind it.
func TestDrainer_TransientFailureIsDeferredNotRetriedImmediately(t *testing.T) {
	env := setupTestDB(t)
	clock := newFakeClock()
	drainer := agent.NewDrainer(env.db, env.guard, nil, agent.WithClock(clock.Now))
	ctx := context.Background()

	stuck := enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID: uuid.New().String(), NewFilePath: filepath.Join(env.staging, "ghost.mov"),
	})
	stats, err := drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Processed)
	ev := eventRow(t, env.db, stuck.EventUuid)
	require.Equal(t, int64(1), ev.RetryCount)
	require.Equal(t, clock.Now().Add(agent.RetryDelay(1)).Unix(), ev.NextAttemptAt)

	// A fresh event behind the deferred one is processed right away.
	good := enqueueEvent(t, env.db, agent.EventNodeCreated, agent.NodeCreatedPayload{
		NodeUUID: uuid.New().String(), FilePath: filepath.Join(env.staging, "good.jpg"),
	})
	stats, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Processed)
	require.Equal(t, "PROCESSED", eventRow(t, env.db, good.EventUuid).Status)

	// Not yet due: repeated passes leave the deferred row alone.
	clock.Advance(agent.RetryDelay(1) - time.Second)
	for range 3 {
		_, err = drainer.DrainAll(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), eventRow(t, env.db, stuck.EventUuid).RetryCount)

	// Due: exactly one more attempt, deferred twice as long.
	clock.Advance(time.Second)
	_, err = drainer.DrainAll(ctx)
	require.NoError(t, err)
	ev = eventRow(t, env.db, stuck.EventUuid)
	require.Equal(t, int64(2), ev.RetryCount)
	require.Equal(t, clock.Now().Add(agent.RetryDelay(2)).Unix(), ev.NextAttemptAt)
}

// TestDrainer_TransientFailureFailsAfterMaxAttempts walks the whole default
// schedule: 20 attempts, delays capped at an hour, then FAILED.
func TestDrainer_TransientFailureFailsAfterMaxAttempts(t *testing.T) {
	env := setupTestDB(t)
	clock := newFakeClock()
	drainer := agent.NewDrainer(env.db, env.guard, nil, agent.WithClock(clock.Now))
	ctx := context.Background()

	stuck := enqueueEvent(t, env.db, agent.EventNodeMoved, agent.NodeMovedPayload{
		NodeUUID: uuid.New().String(), NewFilePath: filepath.Join(env.staging, "ghost.mov"),
	})
	start := clock.Now()
	for attempt := 1; attempt < agent.DefaultMaxRetries; attempt++ {
		stats, err := drainer.ProcessPending(ctx, 10)
		require.NoError(t, err)
		require.Zero(t, stats.Failed, "attempt %d", attempt)
		ev := eventRow(t, env.db, stuck.EventUuid)
		require.Equal(t, int64(attempt), ev.RetryCount)
		delay := time.Unix(ev.NextAttemptAt, 0).Sub(clock.Now())
		require.Equal(t, agent.RetryDelay(attempt), delay)
		require.LessOrEqual(t, delay, time.Hour)
		clock.Advance(delay)
	}
	stats, err := drainer.ProcessPending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Failed)
	require.Equal(t, "FAILED", eventRow(t, env.db, stuck.EventUuid).Status)
	require.Greater(t, clock.Now().Sub(start), 8*time.Hour, "a retryable event should be retried for hours, not seconds")
}
