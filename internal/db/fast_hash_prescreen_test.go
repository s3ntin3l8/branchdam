package db

import (
	"strings"
	"testing"
)

// TestHasLiveNodeByFastHashUsesIndex pins that the check-content fast-hash
// pre-screen (queries/media_nodes.sql HasLiveNodeByFastHash) is served by
// the partial index ix_media_nodes_fast_hash rather than a media_nodes
// table scan: agents call it once per file on every card ingest.
//
// The query is hand-duplicated from internal/db/queries/media_nodes.sql
// (sqlcgen's constant is unexported). If its WHERE shape changes there,
// update it here too.
func TestHasLiveNodeByFastHashUsesIndex(t *testing.T) {
	database := openTestDB(t)
	const query = `SELECT EXISTS(
    SELECT 1
    FROM media_nodes
    WHERE fast_hash = ?1
      AND lifecycle_state IN ('ACTIVE', 'HIDDEN')
      AND (CAST(?2 AS INTEGER) = 0
           OR size_bytes = 0
           OR size_bytes = CAST(?2 AS INTEGER))
) AS possible_match`

	rows, err := database.reader.Query("EXPLAIN QUERY PLAN "+query, "0123456789abcdef", int64(1024))
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
	if !strings.Contains(got, "ix_media_nodes_fast_hash") {
		t.Errorf("query plan does not use ix_media_nodes_fast_hash: %s", got)
	}
	if strings.Contains(got, "SCAN media_nodes") {
		t.Errorf("query plan falls back to a full table scan: %s", got)
	}
}
