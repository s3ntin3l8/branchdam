package rehash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

type env struct {
	db       *db.DB
	guard    *storage.Guard
	archive  string // Tier-3 root
	scratch  string // Tier-1 root
	tier3ID  int64
	tier1ID  int64
	nextUUID int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "rehash.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	archive, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{db: database, archive: archive, scratch: scratch}
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		t3, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "archive", RootPath: archive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1,
		})
		if err != nil {
			return err
		}
		t1, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "scratch", RootPath: scratch, Tier: "TIER1_LOCAL_SCRATCH", Prunable: 1,
		})
		if err != nil {
			return err
		}
		e.tier3ID, e.tier1ID = t3.ID, t1.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed locations: %v", err)
	}
	e.guard = storage.NewGuard([]storage.Location{
		{ID: e.tier3ID, Name: "archive", RootPath: archive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
		{ID: e.tier1ID, Name: "scratch", RootPath: scratch, Tier: "TIER1_LOCAL_SCRATCH"},
	})
	return e
}

func hashOf(t *testing.T, content string) string {
	t.Helper()
	h, err := hashing.FullHash(bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// node seeds a live node at name under root (writing content to disk when
// content is non-nil) whose full_hash is hash, recorded with source.
func (e *env) node(t *testing.T, locID int64, root, name string, content *string, hash, source string) int64 {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(root, name)
	if content != nil {
		if err := os.WriteFile(path, []byte(*content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.nextUUID++
	var id int64
	err := e.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: fmt.Sprintf("018f0000-0000-7000-8000-%012d", e.nextUUID), StorageLocationID: locID,
			FilePath: path, FileName: name, FileExt: ".mov", SizeBytes: 1, MtimeUnix: 1,
			FullHash: &hash, IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		id = n.ID
		return db.RecordFullHashSource(ctx, q, n.ID, &hash, source)
	})
	if err != nil {
		t.Fatalf("seed node %s: %v", name, err)
	}
	return id
}

func (e *env) provenance(t *testing.T, nodeID int64) sqlcgen.FullHashProvenance {
	t.Helper()
	p, err := e.db.Reader.GetFullHashProvenance(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("GetFullHashProvenance(%d): %v", nodeID, err)
	}
	return p
}

func strp(s string) *string { return &s }

// TestMatchFlipsToServer: the file's bytes hash to the asserted value, so
// the hash becomes server-computed.
func TestMatchFlipsToServer(t *testing.T) {
	e := newEnv(t)
	content := "master bytes"
	id := e.node(t, e.tier3ID, e.archive, "a.mov", strp(content), hashOf(t, content), db.HashSourceAgent)

	w := NewWorker(e.db, e.guard, nil)
	stats, err := w.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if stats.Matched != 1 || stats.Mismatched != 0 || stats.Errored != 0 {
		t.Fatalf("stats = %+v, want one match", stats)
	}
	p := e.provenance(t, id)
	if p.Source != db.HashSourceServer || p.RehashState.Valid || p.RehashAttempts != 1 {
		t.Errorf("provenance = %+v, want SERVER with no failure state after one attempt", p)
	}
	// Settled: nothing left to claim.
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats != (Stats{}) {
		t.Errorf("second pass = %+v, %v; want nothing to do", stats, err)
	}
}

// TestMismatchStaysAgent: the bytes hash to something else. The asserted
// value stays AGENT (authorising nothing), the computed value is kept for
// an operator, and media_nodes.full_hash is untouched. The row is only
// re-read after the mismatch window, which doubles with every mismatch,
// and restoring good bytes then verifies it.
func TestMismatchStaysAgent(t *testing.T) {
	e := newEnv(t)
	content := "what is really on disk"
	asserted := hashOf(t, "what the agent claimed")
	id := e.node(t, e.tier3ID, e.archive, "b.mov", strp(content), asserted, db.HashSourceAgent)

	now := time.Now()
	w := NewWorker(e.db, e.guard, nil, WithRetryAfter(0), WithMismatchRetryAfter(24*time.Hour),
		WithClock(func() time.Time { return now }))
	stats, err := w.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if stats.Mismatched != 1 {
		t.Fatalf("stats = %+v, want one mismatch", stats)
	}
	p := e.provenance(t, id)
	if p.Source != db.HashSourceAgent || p.RehashState.String != stateMismatch || p.RehashComputed.String != hashOf(t, content) {
		t.Errorf("provenance = %+v, want AGENT/MISMATCH with the computed hash", p)
	}
	n, err := e.db.Reader.GetMediaNodeByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if n.FullHash == nil || *n.FullHash != asserted {
		t.Errorf("media_nodes.full_hash = %v, want the asserted value left in place", n.FullHash)
	}
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats != (Stats{}) {
		t.Errorf("pass inside the mismatch window = %+v, %v; a MISMATCH must not be re-claimed", stats, err)
	}

	// Past the window the same bytes are read again and still mismatch.
	now = now.Add(25 * time.Hour)
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats.Mismatched != 1 {
		t.Fatalf("pass after the mismatch window = %+v, %v; want the same mismatch again", stats, err)
	}
	if p := e.provenance(t, id); p.Source != db.HashSourceAgent || p.RehashAttempts != 2 || p.RehashMismatches != 2 {
		t.Fatalf("provenance = %+v, want still AGENT after two mismatches", p)
	}

	// An operator restores a good copy of the file: the next re-read after
	// the window verifies the stored value, with no database edit.
	if err := os.WriteFile(filepath.Join(e.archive, "b.mov"), []byte("what the agent claimed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The second mismatch doubled the wait to two days (rehash_retry_at is
	// set from the worker's clock): a day later the row is still held
	// back, two days later the restored bytes verify.
	now = now.Add(25 * time.Hour)
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats != (Stats{}) {
		t.Fatalf("pass inside the doubled window = %+v, %v; want nothing (backoff is two days)", stats, err)
	}
	now = now.Add(24 * time.Hour)
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats.Matched != 1 {
		t.Fatalf("pass after restoring the file = %+v, %v; want one match", stats, err)
	}
	if p := e.provenance(t, id); p.Source != db.HashSourceServer || p.RehashState.Valid || p.RehashComputed.Valid ||
		p.RehashMismatches != 0 || p.RehashRetryAt.Valid {
		t.Errorf("provenance = %+v, want SERVER with the mismatch bookkeeping cleared", p)
	}
}

// TestReadErrorsDontGrowTheMismatchBackoff: a share unmounted for a day
// piles up ERROR attempts; the first real mismatch after it still waits
// only DefaultMismatchRetryAfter, since only mismatches count.
func TestReadErrorsDontGrowTheMismatchBackoff(t *testing.T) {
	e := newEnv(t)
	asserted := hashOf(t, "what the agent claimed")
	id := e.node(t, e.tier3ID, e.archive, "e.mov", nil, asserted, db.HashSourceAgent)

	// WithRetryAfter(0) compresses the hourly ERROR retries: every pass
	// whose clock has moved on re-reads the (still missing) file.
	now := time.Now()
	w := NewWorker(e.db, e.guard, nil, WithRetryAfter(0), WithClock(func() time.Time { return now }))
	for i := 0; i < 24; i++ {
		if stats, err := w.ProcessBatch(context.Background()); err != nil || stats.Errored != 1 {
			t.Fatalf("pass %d: %+v, %v; want one read error", i, stats, err)
		}
		now = now.Add(time.Hour)
	}
	if err := os.WriteFile(filepath.Join(e.archive, "e.mov"), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats.Mismatched != 1 {
		t.Fatalf("pass once the share is back = %+v, %v; want one mismatch", stats, err)
	}
	p := e.provenance(t, id)
	if p.RehashAttempts != 25 || p.RehashMismatches != 1 {
		t.Fatalf("provenance = %+v, want 25 attempts and 1 mismatch", p)
	}
	if want := now.Add(DefaultMismatchRetryAfter).Unix(); !p.RehashRetryAt.Valid || p.RehashRetryAt.Int64 != want {
		t.Errorf("rehash_retry_at = %+v, want %d (one day, not the cap)", p.RehashRetryAt, want)
	}
}

// TestReadErrorRetriesAfterWindow: an unreadable file is recorded as ERROR
// and only retried once RetryAfter has passed; a later successful read
// then verifies it.
func TestReadErrorRetriesAfterWindow(t *testing.T) {
	e := newEnv(t)
	content := "arrives later"
	id := e.node(t, e.tier3ID, e.archive, "c.mov", nil, hashOf(t, content), db.HashSourceAgent)

	now := time.Now()
	w := NewWorker(e.db, e.guard, nil, WithRetryAfter(time.Hour), WithClock(func() time.Time { return now }))
	stats, err := w.ProcessBatch(context.Background())
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if stats.Errored != 1 {
		t.Fatalf("stats = %+v, want one read error", stats)
	}
	if p := e.provenance(t, id); p.Source != db.HashSourceAgent || p.RehashState.String != stateError {
		t.Fatalf("provenance = %+v, want AGENT/ERROR", p)
	}
	if stats, err := w.ProcessBatch(context.Background()); err != nil || stats != (Stats{}) {
		t.Fatalf("pass inside the retry window = %+v, %v; want nothing", stats, err)
	}

	if err := os.WriteFile(filepath.Join(e.archive, "c.mov"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	stats, err = w.ProcessBatch(context.Background())
	if err != nil || stats.Matched != 1 {
		t.Fatalf("pass after the retry window = %+v, %v; want one match", stats, err)
	}
	if p := e.provenance(t, id); p.Source != db.HashSourceServer || p.RehashAttempts != 2 {
		t.Errorf("provenance = %+v, want SERVER after two attempts", p)
	}
}

// TestOnlyClaimsLiveTier3AgentRows: server-computed rows, other tiers and
// MISSING nodes are never read.
func TestOnlyClaimsLiveTier3AgentRows(t *testing.T) {
	e := newEnv(t)
	c := "x"
	server := e.node(t, e.tier3ID, e.archive, "server.mov", strp(c), hashOf(t, c), db.HashSourceServer)
	tier1 := e.node(t, e.tier1ID, e.scratch, "scratch.mov", strp("y"), hashOf(t, "y"), db.HashSourceAgent)
	missing := e.node(t, e.tier3ID, e.archive, "missing.mov", strp("z"), hashOf(t, "z"), db.HashSourceAgent)
	if err := e.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		return q.MarkNodeMissing(context.Background(), missing)
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := NewWorker(e.db, e.guard, nil).ProcessBatch(context.Background())
	if err != nil || stats != (Stats{}) {
		t.Fatalf("stats = %+v, %v; want nothing claimed", stats, err)
	}
	for _, id := range []int64{server, tier1, missing} {
		if p := e.provenance(t, id); p.RehashAttempts != 0 {
			t.Errorf("node %d provenance = %+v, want untouched", id, p)
		}
	}
}

// TestResumesAcrossBatches: a backlog larger than one batch is worked
// through in id order across passes (More reports the backlog), and a new
// worker -- as after a restart -- picks up exactly what is left.
func TestResumesAcrossBatches(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := range 5 {
		c := fmt.Sprintf("content-%d", i)
		ids = append(ids, e.node(t, e.tier3ID, e.archive, fmt.Sprintf("n%d.mov", i), strp(c), hashOf(t, c), db.HashSourceAgent))
	}

	w := NewWorker(e.db, e.guard, nil, WithBatchSize(2))
	stats, err := w.ProcessBatch(context.Background())
	if err != nil || stats.Matched != 2 || !stats.More {
		t.Fatalf("first batch = %+v, %v; want 2 matched and More", stats, err)
	}
	for i, id := range ids {
		want := db.HashSourceAgent
		if i < 2 {
			want = db.HashSourceServer
		}
		if p := e.provenance(t, id); p.Source != want {
			t.Errorf("after batch 1, node %d source = %s, want %s", id, p.Source, want)
		}
	}

	// "Restart": a fresh worker with no cursor resumes from what is left.
	w2 := NewWorker(e.db, e.guard, nil, WithBatchSize(2))
	total := 0
	for range 3 {
		stats, err := w2.ProcessBatch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		total += stats.Matched
	}
	if total != 3 {
		t.Errorf("resumed worker matched %d, want the remaining 3", total)
	}
	for _, id := range ids {
		if p := e.provenance(t, id); p.Source != db.HashSourceServer || p.RehashAttempts != 1 {
			t.Errorf("node %d provenance = %+v, want SERVER after exactly one read", id, p)
		}
	}
}

// TestCancelledHashRecordsNothing: shutdown during a read abandons it
// without recording an ERROR, so the row is still a fresh candidate.
func TestCancelledHashRecordsNothing(t *testing.T) {
	e := newEnv(t)
	c := "big master"
	id := e.node(t, e.tier3ID, e.archive, "d.mov", strp(c), hashOf(t, c), db.HashSourceAgent)
	rows, err := e.db.Reader.ListRehashCandidates(context.Background(), sqlcgen.ListRehashCandidatesParams{BatchLimit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("candidates = %+v, %v", rows, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := NewWorker(e.db, e.guard, nil)
	var stats Stats
	if err := w.processOne(ctx, rows[0], &stats); !errors.Is(err, context.Canceled) {
		t.Fatalf("processOne = %v, want context.Canceled", err)
	}
	if stats != (Stats{}) {
		t.Errorf("stats = %+v, want nothing counted", stats)
	}
	if p := e.provenance(t, id); p.RehashAttempts != 0 || p.RehashState.Valid || p.Source != db.HashSourceAgent {
		t.Errorf("provenance = %+v, want untouched", p)
	}
}

// TestStartVerifiesAndWaitJoinsOnCancel: the background loop verifies a
// candidate on its first pass, and Wait returns once ctx is cancelled.
func TestStartVerifiesAndWaitJoinsOnCancel(t *testing.T) {
	e := newEnv(t)
	c := "background"
	id := e.node(t, e.tier3ID, e.archive, "e.mov", strp(c), hashOf(t, c), db.HashSourceAgent)

	ctx, cancel := context.WithCancel(context.Background())
	w := NewWorker(e.db, e.guard, nil)
	w.Start(ctx, time.Hour)
	w.Start(ctx, time.Hour) // second call is a no-op

	deadline := time.Now().Add(5 * time.Second)
	for e.provenance(t, id).Source != db.HashSourceServer {
		if time.Now().After(deadline) {
			t.Fatal("background pass never verified the candidate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { w.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

// TestCtxReaderStopsOnCancel pins the read-side half of clean shutdown.
func TestCtxReaderStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &ctxReader{ctx: ctx, r: bytes.NewReader([]byte("abc"))}
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("Read before cancel: %v", err)
	}
	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after cancel = %v, want context.Canceled", err)
	}
}

// TestMismatchBackoffDoublesToTheCap: one day after the first mismatch,
// doubling per attempt, never past MaxMismatchRetryAfter.
func TestMismatchBackoffDoublesToTheCap(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		attempts int64
		want     time.Duration
	}{
		{0, day}, {1, day}, {2, 2 * day}, {3, 4 * day}, {5, 16 * day}, {6, MaxMismatchRetryAfter}, {1000, MaxMismatchRetryAfter},
	} {
		if got := mismatchBackoff(day, tc.attempts); got != tc.want {
			t.Errorf("mismatchBackoff(1d, %d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
	if got := mismatchBackoff(0, 3); got != 0 {
		t.Errorf("mismatchBackoff(0, 3) = %s, want 0 (tests disable the window)", got)
	}
	if got := mismatchBackoff(60*day, 3); got != 60*day {
		t.Errorf("mismatchBackoff(60d, 3) = %s, want the base when it exceeds the cap", got)
	}
}
