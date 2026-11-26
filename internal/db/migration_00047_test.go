package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration00047ReviewSource(t *testing.T) {
	database := openRawWriter(t, filepath.Join(t.TempDir(), "review-source.db"))
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(database, migrationsDir, 46); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('media_edges') WHERE name = 'review_source'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("review_source present before 00047: %d", before)
	}
	if err := goose.UpTo(database, migrationsDir, 47); err != nil {
		t.Fatal(err)
	}

	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := database.Exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`INSERT INTO storage_locations(id, name, root_path, tier) VALUES (1, 'archive', '/archive', 'TIER1_LOCAL_SCRATCH')`)
	exec(`INSERT INTO media_nodes(id, node_uuid, storage_location_id, file_path, file_name) VALUES
		(1, 'parent', 1, '/archive/a.jpg', 'a.jpg'),
		(2, 'child', 1, '/archive/b.jpg', 'b.jpg')`)
	exec(`INSERT INTO media_edges(source_node_id, target_node_id, relationship_type, confidence, tier, resolver, evidence_json, review_state)
		VALUES (1, 2, 'DERIVED_FROM', 0.5, 2, 'test', '{}', 'NEEDS_REVIEW')`)

	var source sql.NullString
	if err := database.QueryRow(`SELECT review_source FROM v_media_edges_resolved WHERE source_node_id = 1`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source.Valid {
		t.Fatalf("new edge review_source = %v, want NULL", source)
	}
	for _, okSource := range []string{"human", "assistant"} {
		exec(`UPDATE media_edges SET review_source = ? WHERE source_node_id = 1`, okSource)
		if err := database.QueryRow(`SELECT review_source FROM v_media_edges_resolved WHERE source_node_id = 1`).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if !source.Valid || source.String != okSource {
			t.Fatalf("view review_source = %v, want %s", source, okSource)
		}
	}
	if _, err := database.Exec(`UPDATE media_edges SET review_source = 'robot' WHERE source_node_id = 1`); err == nil {
		t.Fatal("review_source robot inserted, want CHECK failure")
	}
	exec(`UPDATE media_edges SET review_source = NULL WHERE source_node_id = 1`)

	if err := goose.Down(database, migrationsDir); err != nil {
		t.Fatal(err)
	}
	var leftover int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('media_edges') WHERE name = 'review_source'`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("Down left review_source (%d)", leftover)
	}
	var viewCol int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('v_media_edges_resolved') WHERE name = 'review_source'`).Scan(&viewCol); err != nil {
		t.Fatal(err)
	}
	if viewCol != 0 {
		t.Fatalf("Down view still exposes review_source")
	}
	var id int64
	if err := database.QueryRow(`SELECT id FROM v_media_edges_resolved WHERE source_node_id = 1`).Scan(&id); err != nil {
		t.Fatalf("view after Down: %v", err)
	}

	if err := goose.Up(database, migrationsDir); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT review_source FROM media_edges WHERE source_node_id = 1`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source.Valid {
		t.Fatalf("review_source after down/up = %v, want NULL", source)
	}
}
