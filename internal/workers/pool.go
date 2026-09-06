// Package workers implements a generic bounded goroutine pool. It exists so
// that hashing, EXIF/ffprobe extraction, and any other per-file work spec
// directive 9.2 requires off the HTTP/watcher threads has one shared,
// tested place to run -- callers never spawn their own goroutines per file.
package workers

import (
	"context"
	"sync"
	"sync/atomic"
)

// Job is one unit of work. Key identifies it for in-flight deduplication --
// e.g. a file path, so the same file submitted twice while its first pass is
// still running is dropped rather than double-processed. Run does the actual
// work and captures whatever result channel or callback its caller needs;
// Pool itself is result-agnostic.
//
// OnAbandon, if set, is called instead of Run for a job that was accepted by
// Submit but never dequeued before shutdown (Run(ctx)'s ctx reaching Done).
// A caller that tracks its own completion via Run's side effects (e.g. a
// sync.WaitGroup token released inside Run) needs OnAbandon to release that
// same token for a job Run never executed -- without it, a job stuck in the
// queue at shutdown leaves that bookkeeping permanently unresolved.
type Job[K comparable] struct {
	Key       K
	Run       func(context.Context) error
	OnAbandon func()
}

// Pool runs Jobs on a fixed number of goroutines, reading from a bounded
// buffered channel. The bound is deliberate: an unbounded queue turns a slow
// consumer (e.g. a NAS under load) into unbounded memory growth. When the
// queue is full, Submit returns false rather than blocking the caller --
// backpressure is visible to the submitter, not hidden inside a channel send.
type Pool[K comparable] struct {
	workerCount int
	jobs        chan Job[K]

	mu       sync.Mutex
	inflight map[K]struct{}
	closed   bool        // set under mu by closeOnDone; see Submit and closeOnDone
	closing  atomic.Bool // set when ctx.Done fires, before closeOnDone takes mu; closes the Submit/closeOnDone race window

	wg sync.WaitGroup
}

// New creates a Pool with the given worker count and queue depth. Both are
// clamped to at least 1 -- a pool of zero workers or zero queue depth is
// never useful and is almost certainly a misconfiguration, not an
// intentional "do nothing" pool.
func New[K comparable](workerCount, queueDepth int) *Pool[K] {
	if workerCount < 1 {
		workerCount = 1
	}
	if queueDepth < 1 {
		queueDepth = 1
	}
	return &Pool[K]{
		workerCount: workerCount,
		jobs:        make(chan Job[K], queueDepth),
		inflight:    make(map[K]struct{}, queueDepth),
	}
}

// Run starts the pool's worker goroutines and returns immediately -- it
// does not block until ctx is done. Call Drain (after cancelling ctx) to
// wait for in-flight work to finish during shutdown.
func (p *Pool[K]) Run(ctx context.Context) {
	p.wg.Add(1)
	go p.closeOnDone(ctx)
	for i := 0; i < p.workerCount; i++ {
		p.wg.Add(1)
		go p.workerLoop(ctx)
	}
}

func (p *Pool[K]) workerLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			p.runJob(ctx, job)
		}
	}
}

// closeOnDone waits for ctx to be done, then marks the pool closed and
// drains whatever is currently buffered in p.jobs, calling each drained
// job's OnAbandon hook (if set) -- so a job that was Submit-ed but never
// dequeued before shutdown still gets its completion bookkeeping released,
// instead of leaving a caller's wg.Wait() blocked forever.
//
// The atomic `closing` flag is set immediately after ctx.Done(), before
// taking p.mu, so that concurrent Submit calls see the flag before they
// take the lock -- closing the race window between ctx cancellation and
// p.closed being set. Without this, a Submit that lands between
// ctx.Done() firing and closeOnDone acquiring p.mu could enqueue a job
// that a worker picks up and processes normally.
//
// This runs as a single dedicated goroutine, tracked by Run alongside the
// workers, rather than each worker independently racing ctx.Done() against
// p.jobs and draining redundantly (an earlier version of this method did
// that). That mattered: with per-worker draining, "check ctx.Done()" and
// "drain the queue" were two separate, unlocked observations, so a worker
// could observe ctx.Done(), find the queue empty, and exit -- all while a
// concurrent Submit call was between its own ctx check and its channel
// send. That job would land in p.jobs after every worker had already given
// up on it: enqueued, but with zero remaining readers, permanently
// unresolved. Marking closed and draining here, both under p.mu -- the same
// lock Submit takes to check closed before it sends -- makes the two
// operations strictly ordered: any Submit that completes its send has
// necessarily done so either entirely before this critical section (so the
// job is already buffered when the drain below runs and gets caught by it,
// or already claimed by a still-live worker) or Submit fails outright,
// because closed was already true by the time it checked.
func (p *Pool[K]) closeOnDone(ctx context.Context) {
	defer p.wg.Done()
	<-ctx.Done()
	p.closing.Store(true)

	p.mu.Lock()
	p.closed = true
	abandoned := p.drainLocked()
	p.mu.Unlock()

	for _, job := range abandoned {
		if job.OnAbandon != nil {
			job.OnAbandon()
		}
	}
}

// drainLocked removes every job currently buffered in p.jobs, clears its
// dedup key, and returns the drained jobs for the caller to abandon outside
// the lock (an OnAbandon callback that itself calls back into the pool,
// e.g. re-Submitting, would deadlock if invoked while p.mu is held). Must
// be called with p.mu held; non-blocking -- it only removes what's already
// buffered, never waits for more to arrive.
func (p *Pool[K]) drainLocked() []Job[K] {
	var drained []Job[K]
	for {
		select {
		case job, ok := <-p.jobs:
			if !ok {
				return drained
			}
			delete(p.inflight, job.Key)
			drained = append(drained, job)
		default:
			return drained
		}
	}
}

func (p *Pool[K]) runJob(ctx context.Context, job Job[K]) {
	defer func() {
		p.mu.Lock()
		delete(p.inflight, job.Key)
		p.mu.Unlock()
	}()
	_ = job.Run(ctx)
}

// Submit enqueues job if its Key is not already in flight and the queue has
// room, and returns whether it was actually enqueued. It never blocks: a
// duplicate key or a full queue both return false immediately, rather than
// waiting for a worker to free up or space to open. A context already
// cancelled at call time also returns false without enqueuing -- callers
// should not keep submitting new work after shutdown has begun.
//
// Submit also refuses once the Pool's own Run context is done: the atomic
// `closing` flag is checked first (set by closeOnDone immediately after
// ctx.Done(), before taking p.mu) to close the race window between ctx
// cancellation and closeOnDone acquiring the lock. The `closed` flag under
// p.mu is the final guard -- see closeOnDone's doc comment for why these
// two checks are strictly ordered: an earlier version only had `closed`
// under the lock, which left a window where Submit could enqueue a job
// after every worker had already given up on draining it.
func (p *Pool[K]) Submit(ctx context.Context, job Job[K]) bool {
	if ctx.Err() != nil {
		return false
	}
	if p.closing.Load() {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return false
	}
	if _, dup := p.inflight[job.Key]; dup {
		return false
	}
	// Non-blocking send while holding p.mu is safe: it either succeeds
	// immediately (room in the buffer) or falls through to default
	// immediately (full) -- it never waits, so it can't hold the lock for
	// longer than a moment.
	select {
	case p.jobs <- job:
		p.inflight[job.Key] = struct{}{}
		return true
	default:
		return false
	}
}

// Drain blocks until every worker goroutine has exited (i.e. the current
// in-flight job on each worker, if any, has finished). Call this after
// cancelling the context passed to Run, as part of an orderly shutdown.
func (p *Pool[K]) Drain() {
	p.wg.Wait()
}

// InFlight returns the number of jobs currently submitted and not yet finished.
func (p *Pool[K]) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// QueueDepth returns the current number of jobs buffered in the queue.
func (p *Pool[K]) QueueDepth() int {
	return len(p.jobs)
}

// QueueCapacity returns the buffer capacity of the queue channel.
func (p *Pool[K]) QueueCapacity() int {
	return cap(p.jobs)
}

// WorkerCount returns the configured number of worker goroutines.
func (p *Pool[K]) WorkerCount() int {
	return p.workerCount
}
