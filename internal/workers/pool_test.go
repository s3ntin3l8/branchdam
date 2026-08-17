package workers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolBoundedConcurrency is T6: 500 jobs into a pool of 4 workers.
// Asserts the pool never runs more than 4 concurrently, every job ran
// exactly once, and Submit itself never blocks on worker availability
// (it only ever waits on a mutex and a non-blocking channel send).
func TestPoolBoundedConcurrency(t *testing.T) {
	const workerCount = 4
	const n = 500
	const jobDuration = 2 * time.Millisecond

	pool := New[int](workerCount, n) // queue sized to hold all 500 so Submit never sees a full queue
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Run(ctx)

	var current int32
	var maxObserved int32
	var ran int32

	submitStart := time.Now()
	for i := 0; i < n; i++ {
		i := i
		ok := pool.Submit(ctx, Job[int]{
			Key: i, // unique keys -- this test is about concurrency bounds, not dedup
			Run: func(context.Context) error {
				c := atomic.AddInt32(&current, 1)
				for {
					old := atomic.LoadInt32(&maxObserved)
					if c <= old || atomic.CompareAndSwapInt32(&maxObserved, old, c) {
						break
					}
				}
				time.Sleep(jobDuration)
				atomic.AddInt32(&current, -1)
				atomic.AddInt32(&ran, 1)
				return nil
			},
		})
		if !ok {
			t.Fatalf("Submit(%d) returned false, want true (queue is sized to hold all %d)", i, n)
		}
	}
	submitDur := time.Since(submitStart)

	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&ran) < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&ran); got != n {
		t.Fatalf("ran = %d, want %d (some jobs never executed)", got, n)
	}
	if got := atomic.LoadInt32(&maxObserved); got > workerCount {
		t.Errorf("max observed concurrency = %d, want <= %d", got, workerCount)
	}

	// With 500 jobs at 2ms each across 4 workers, total wall time is at
	// least 500/4*2ms = 250ms. Submit filling a 500-deep buffered channel
	// should take microseconds, not a meaningful fraction of that -- 20ms
	// is a generous ceiling that still catches "Submit blocks on a worker".
	if submitDur > 20*time.Millisecond {
		t.Errorf("Submit-ing all %d jobs took %s, want it to not block on worker availability", n, submitDur)
	}

	cancel()
	pool.Drain()
}

// TestPoolDedupesInFlightKey: a key already running is refused by Submit,
// and the dedup entry clears once the job completes so the same key can be
// submitted again later.
func TestPoolDedupesInFlightKey(t *testing.T) {
	pool := New[string](1, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Run(ctx)

	started := make(chan struct{})
	release := make(chan struct{})
	var runs int32

	if !pool.Submit(ctx, Job[string]{Key: "same-key", Run: func(context.Context) error {
		atomic.AddInt32(&runs, 1)
		close(started)
		<-release
		return nil
	}}) {
		t.Fatal("first Submit for key returned false")
	}

	<-started // the job is now actually running, so the inflight entry is still held

	if pool.Submit(ctx, Job[string]{Key: "same-key", Run: func(context.Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}}) {
		t.Fatal("Submit for an in-flight key returned true, want false (deduped)")
	}

	close(release)
	// Give the first job's completion (and inflight cleanup) time to land.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runs) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // let runJob's deferred cleanup run after Run returns

	if !pool.Submit(ctx, Job[string]{Key: "same-key", Run: func(context.Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}}) {
		t.Fatal("Submit for a since-completed key returned false, want true (dedup must clear on completion)")
	}

	cancel()
	pool.Drain()

	if got := atomic.LoadInt32(&runs); got != 2 {
		t.Errorf("runs = %d, want 2 (first + third submission; the duplicate must never run)", got)
	}
}

// TestPoolQueueFullReturnsFalse proves the queue-full path: with no worker
// running to drain it, a queue of depth 1 accepts exactly one job before
// Submit starts returning false.
func TestPoolQueueFullReturnsFalse(t *testing.T) {
	pool := New[int](1, 1)
	ctx := context.Background() // deliberately never Run() -- nothing drains the queue

	if !pool.Submit(ctx, Job[int]{Key: 1, Run: func(context.Context) error { return nil }}) {
		t.Fatal("first Submit into an empty depth-1 queue returned false")
	}
	if pool.Submit(ctx, Job[int]{Key: 2, Run: func(context.Context) error { return nil }}) {
		t.Fatal("second Submit into a full depth-1 queue returned true, want false")
	}
}

// TestPoolRefusesAfterContextCancelled proves Submit stops accepting work
// once its context is already done, rather than queuing work that will
// never be picked up during shutdown.
func TestPoolRefusesAfterContextCancelled(t *testing.T) {
	pool := New[int](1, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if pool.Submit(ctx, Job[int]{Key: 1, Run: func(context.Context) error { return nil }}) {
		t.Fatal("Submit with an already-cancelled context returned true, want false")
	}
}

// TestPoolRefusesAfterRunContextCancelled is the #92 companion to
// TestPoolRefusesAfterContextCancelled: it proves Submit also refuses once
// the *Pool's own* Run context is done, even when the caller passes a
// different context that's still live (e.g. a background scan's
// context.WithoutCancel(reqCtx), which deliberately never observes request
// cancellation). Without this, a caller whose own ctx survives shutdown
// could keep enqueuing jobs after every worker has already exited, and
// nothing would ever dequeue or abandon them.
func TestPoolRefusesAfterRunContextCancelled(t *testing.T) {
	pool := New[int](1, 4)
	runCtx, cancelRun := context.WithCancel(context.Background())
	pool.Run(runCtx)
	cancelRun()

	callerCtx := context.Background() // deliberately never cancelled -- this is the case Submit's own ctx.Err() check can't catch
	if pool.Submit(callerCtx, Job[int]{Key: 1, Run: func(context.Context) error { return nil }}) {
		t.Fatal("Submit after the Pool's Run context was cancelled returned true, want false, even with a live caller ctx")
	}
	pool.Drain()
}

// TestPoolDrainAbandonsQueuedJobs is a direct, deterministic unit test of
// drainAbandoned -- the mechanism workerLoop invokes on ctx.Done() -- with
// no live workers and no goroutine-scheduling race involved. It exercises
// exactly the #92 regression: a job accepted by Submit but never dequeued
// must still release whatever bookkeeping its caller attached via
// OnAbandon, and its dedup key must clear from inflight, or Submit would
// wrongly keep refusing that key as a duplicate forever.
func TestPoolDrainAbandonsQueuedJobs(t *testing.T) {
	pool := New[int](1, 4)
	ctx := context.Background() // deliberately never Run() -- nothing will ever dequeue these except drainAbandoned itself

	var abandoned []int
	for i := 1; i <= 3; i++ {
		i := i
		if !pool.Submit(ctx, Job[int]{
			Key: i,
			Run: func(context.Context) error {
				t.Errorf("Run called for job %d, want OnAbandon only (nothing dequeued this job)", i)
				return nil
			},
			OnAbandon: func() { abandoned = append(abandoned, i) },
		}) {
			t.Fatalf("Submit(%d) returned false", i)
		}
	}

	pool.drainAbandoned()

	if len(abandoned) != 3 {
		t.Fatalf("abandoned = %v, want all 3 jobs abandoned exactly once", abandoned)
	}

	pool.mu.Lock()
	inflightLen := len(pool.inflight)
	pool.mu.Unlock()
	if inflightLen != 0 {
		t.Errorf("inflight has %d entries after drainAbandoned, want 0 -- an abandoned job's dedup key must clear like a completed one does", inflightLen)
	}
}

// TestPoolDrainTerminatesWithAJobStillQueuedAtShutdown is the end-to-end
// #92 regression against a live pool: a job Submit-ed just before shutdown
// begins, and never dequeued by the sole worker before it notices
// ctx.Done(), must not leave Drain() blocked forever. Before this fix,
// nothing ever called that job's Run (it was never dequeued) or released
// any equivalent completion signal, so a caller relying on such a signal
// (like pipeline.runScan's own wg) would hang indefinitely.
//
// Which of Run or OnAbandon actually fires for the queued job is a genuine
// race (Go's select makes no ordering guarantee between an already-ready
// ctx.Done() and an already-buffered channel receive) and deliberately not
// asserted here -- see TestPoolDrainAbandonsQueuedJobs above for a
// deterministic, race-free proof that the abandon path itself works.
// What's asserted here is the actual regression: Drain() returns, and
// exactly one of the two completion paths fired.
func TestPoolDrainTerminatesWithAJobStillQueuedAtShutdown(t *testing.T) {
	pool := New[int](1, 4)
	ctx, cancel := context.WithCancel(context.Background())
	pool.Run(ctx)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	if !pool.Submit(ctx, Job[int]{Key: 1, Run: func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	}}) {
		t.Fatal("first Submit returned false")
	}
	<-firstStarted // the sole worker is now busy, so the next Submit is guaranteed to land in the queue

	var ran, abandoned int32
	if !pool.Submit(ctx, Job[int]{
		Key: 2,
		Run: func(context.Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		},
		OnAbandon: func() {
			atomic.AddInt32(&abandoned, 1)
		},
	}) {
		t.Fatal("second Submit returned false, want true (queue has room)")
	}

	cancel()            // shutdown begins while job 2 is still sitting in the queue, undequeued
	close(releaseFirst) // let the in-flight first job finish so the worker re-checks ctx.Done()

	drained := make(chan struct{})
	go func() {
		pool.Drain()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain() did not return within 5s -- job 2 was left permanently unresolved")
	}

	if got := ran + abandoned; got != 1 {
		t.Errorf("ran(%d) + abandoned(%d) = %d, want exactly 1 -- job 2 must resolve exactly once, one way or the other", ran, abandoned, got)
	}
}
