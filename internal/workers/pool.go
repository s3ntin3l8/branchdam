// Package workers implements a generic bounded goroutine pool. It exists so
// that hashing, EXIF/ffprobe extraction, and any other per-file work spec
// directive 9.2 requires off the HTTP/watcher threads has one shared,
// tested place to run -- callers never spawn their own goroutines per file.
package workers

import (
	"context"
	"sync"
)

// Job is one unit of work. Key identifies it for in-flight deduplication --
// e.g. a file path, so the same file submitted twice while its first pass is
// still running is dropped rather than double-processed. Run does the actual
// work and captures whatever result channel or callback its caller needs;
// Pool itself is result-agnostic.
type Job[K comparable] struct {
	Key K
	Run func(context.Context) error
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
func (p *Pool[K]) Submit(ctx context.Context, job Job[K]) bool {
	if ctx.Err() != nil {
		return false
	}

	p.mu.Lock()
	if _, dup := p.inflight[job.Key]; dup {
		p.mu.Unlock()
		return false
	}
	p.inflight[job.Key] = struct{}{}
	p.mu.Unlock()

	select {
	case p.jobs <- job:
		return true
	default:
		p.mu.Lock()
		delete(p.inflight, job.Key)
		p.mu.Unlock()
		return false
	}
}

// Drain blocks until every worker goroutine has exited (i.e. the current
// in-flight job on each worker, if any, has finished). Call this after
// cancelling the context passed to Run, as part of an orderly shutdown.
func (p *Pool[K]) Drain() {
	p.wg.Wait()
}
