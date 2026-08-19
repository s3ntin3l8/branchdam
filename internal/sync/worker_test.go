package sync

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// recordingHandler is a minimal slog.Handler that captures every record's
// level and message so a test can assert on log output without parsing text.
type recordingHandler struct {
	records *[]slog.Record
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestDrainWarnsOnExhaustedPushesEveryTick verifies #182's observability gap
// fix: a row that has permanently stopped retrying (retry_count at the
// bound) produces a Warn log line on every drain() tick, not just the tick
// it first crossed the bound -- RecoverFailedPushes itself goes silent for
// such a row forever after, so this is the only recurring signal.
func TestDrainWarnsOnExhaustedPushesEveryTick(t *testing.T) {
	database := openTestDB(t)
	mgr := NewManager(database, nil)
	ctx := context.Background()
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)
	node := seedNode(t, database, locID, "/exports/immich/stuck.jpg")

	if err := mgr.Enqueue(ctx, Node{ID: node.ID, Checksum: "aaaaaaaaaaaaaaaa"}, RemoteImmich); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for i := 0; i < DefaultMaxSyncRetries; i++ {
		if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.MarkRemoteSyncStateFailed(ctx, sqlcgen.MarkRemoteSyncStateFailedParams{
				NodeID: node.ID, Remote: RemoteImmich, LastError: sql.NullString{String: "boom", Valid: true},
			})
		}); err != nil {
			t.Fatalf("mark failed (%d): %v", i, err)
		}
	}

	var records []slog.Record
	log := slog.New(recordingHandler{records: &records})
	w := NewWorker(mgr, RemoteImmich, "/exports/immich", 16, 0, func(context.Context, []Node) error { return nil }, log)

	warnCount := func() int {
		n := 0
		for _, r := range records {
			if r.Level == slog.LevelWarn && strings.Contains(r.Message, "permanently abandoned") {
				n++
			}
		}
		return n
	}

	w.drain(ctx)
	if n := warnCount(); n != 1 {
		t.Fatalf("warn count after first drain = %d, want 1", n)
	}

	w.drain(ctx)
	if n := warnCount(); n != 2 {
		t.Fatalf("warn count after second drain = %d, want 2 (must warn every tick, not just the first)", n)
	}
}
