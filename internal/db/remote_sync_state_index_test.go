package db

import (
	"strings"
	"testing"
)

// TestRemoteSyncStateIndexExists backs #165: every hot query against
// remote_sync_state (internal/db/queries/remote_sync_state.sql) filters on
// remote + sync_status, but the table's only index used to be the
// (node_id, remote) PRIMARY KEY, which doesn't lead with either column --
// every one of those queries was a full table scan. This confirms the new
// composite index is actually created by the migration, not just present
// in the .sql source.
func TestRemoteSyncStateIndexExists(t *testing.T) {
	database := openTestDB(t)

	var name string
	err := database.reader.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name=?",
		"ix_remote_sync_state_remote_status",
	).Scan(&name)
	if err != nil {
		t.Fatalf("ix_remote_sync_state_remote_status not found: %v", err)
	}
}

// TestRemoteSyncStateQueriesUseIndex backs #165 more strongly than existence
// alone: it confirms SQLite's query planner actually SELECTS the new index
// for the exact WHERE/ORDER BY shapes ListRemoteSyncStateByStatus,
// ResetRemoteSyncStateStale, and ResetRemoteSyncStateFailed use
// (remote_sync_state.sql), rather than falling back to a full table scan
// despite the index being present -- an index that exists but is never
// chosen closes none of the gap this migration is for.
//
// For ListRemoteSyncStateByStatus specifically, this also asserts there is
// no residual "TEMP B-TREE" sort left over from the ORDER BY's node_id
// tiebreaker: last_attempt_at is a second-granularity unixepoch() column,
// so ties within one worker poll's claim batch are the normal case, not an
// edge case, and the index's trailing node_id column exists precisely to
// let SQLite walk it in the query's exact ORDER BY order instead of
// collecting matches and sorting them afterward.
func TestRemoteSyncStateQueriesUseIndex(t *testing.T) {
	database := openTestDB(t)

	cases := []struct {
		name           string
		query          string
		wantNoTempSort bool
	}{
		{
			name:           "ListRemoteSyncStateByStatus",
			query:          "SELECT node_id FROM remote_sync_state WHERE remote = 'IMMICH' AND sync_status = 'PENDING_CLOUD_PUSH' ORDER BY last_attempt_at ASC, node_id ASC LIMIT 50",
			wantNoTempSort: true,
		},
		{
			name:  "ResetRemoteSyncStateStale",
			query: "UPDATE remote_sync_state SET sync_status = 'PENDING_CLOUD_PUSH' WHERE sync_status = 'PUSHING' AND remote = 'IMMICH' AND last_attempt_at < 0",
		},
		{
			name:  "ResetRemoteSyncStateFailed",
			query: "UPDATE remote_sync_state SET sync_status = 'PENDING_CLOUD_PUSH' WHERE sync_status = 'PUSH_FAILED' AND remote = 'IMMICH' AND last_attempt_at < 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := database.reader.Query("EXPLAIN QUERY PLAN " + tc.query)
			if err != nil {
				t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
			}
			defer func() { _ = rows.Close() }()

			var plan strings.Builder
			for rows.Next() {
				var id, parent, notUsed int
				var detail string
				if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
					t.Fatalf("scan query plan row: %v", err)
				}
				plan.WriteString(detail)
				plan.WriteString("; ")
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate query plan: %v", err)
			}

			got := plan.String()
			if !strings.Contains(got, "ix_remote_sync_state_remote_status") {
				t.Errorf("query plan does not use ix_remote_sync_state_remote_status: %s", got)
			}
			if strings.Contains(got, "SCAN remote_sync_state") {
				t.Errorf("query plan falls back to a full table scan: %s", got)
			}
			if tc.wantNoTempSort && strings.Contains(got, "TEMP B-TREE") {
				t.Errorf("query plan still needs a temp sort despite the index -- the index doesn't fully satisfy the ORDER BY: %s", got)
			}
		})
	}
}
