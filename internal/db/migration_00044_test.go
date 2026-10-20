package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigration00044AddsLiveNodeLookupIndexes covers migration 00044: the
// two live-node lookups that had no supporting index.
//
//   - ListLiveNodesByFileName   WHERE file_name = ?1
//   - ListLiveNodesByDocumentID WHERE document_id = ?1
//
// Both are read on every lineage resolution, so without an index each
// resolution scanned the whole media_nodes table. document_id sat next
// to ix_media_nodes_origdocid, but that index is on
// original_document_id -- a different column.
//
// The test asserts the indexes exist AND that SQLite's planner actually
// picks them for the two queries: an index nobody uses is not an
// optimisation, and the planner is the only thing that decides.
func TestMigration00044AddsLiveNodeLookupIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live-node-lookup-indexes.db")
	writerDB := openRawWriter(t, path)
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	for _, name := range []string{"idx_media_nodes_file_name", "idx_media_nodes_document_id"} {
		found := false
		rows, err := writerDB.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name)
		if err != nil {
			t.Fatalf("index lookup %s: %v", name, err)
		}
		for rows.Next() {
			var got string
			if err := rows.Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got == name {
				found = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if !found {
			t.Errorf("index %s was not created", name)
		}
	}

	// The planner must use the new indexes, not fall back to a scan.
	plans := map[string]struct {
		query string
		index string
	}{
		"file name": {
			query: `SELECT id FROM media_nodes WHERE file_name = 'x.jpg' AND lifecycle_state NOT IN ('ARCHIVED','TRASHED') LIMIT 500`,
			index: "idx_media_nodes_file_name",
		},
		"document id": {
			query: `SELECT id FROM media_nodes WHERE document_id = 'x' AND lifecycle_state NOT IN ('ARCHIVED','TRASHED') LIMIT 500`,
			index: "idx_media_nodes_document_id",
		},
	}
	for label, p := range plans {
		rows, err := writerDB.Query("EXPLAIN QUERY PLAN " + p.query)
		if err != nil {
			t.Fatalf("%s: explain: %v", label, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if !strings.Contains(plan.String(), p.index) {
			t.Errorf("%s: query plan does not use %s:\n%s", label, p.index, plan.String())
		}
		if strings.Contains(plan.String(), "SCAN media_nodes") {
			t.Errorf("%s: query plan still scans media_nodes:\n%s", label, plan.String())
		}
	}
}
