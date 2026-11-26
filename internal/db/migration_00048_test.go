package db

import (
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration00048AssetTags(t *testing.T) {
	database := openRawWriter(t, filepath.Join(t.TempDir(), "tags.db"))
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(database, migrationsDir, 47); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'asset_tags'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("asset_tags present before 00048: %d", before)
	}
	if err := goose.UpTo(database, migrationsDir, 48); err != nil {
		t.Fatal(err)
	}

	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := database.Exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`INSERT INTO users (username, email, is_admin, source, created_at, created_by, auth_provider, external_uid, password_hash)
		VALUES ('alice', 'alice@example.com', 1, 'local', unixepoch(), 'test', 'local', 'uid:alice', 'hash')`)
	exec(`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'archive', '/archive', 'TIER1_LOCAL_SCRATCH')`)
	exec(`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name) VALUES (1, 'node-1', 1, '/archive/plain.jpg', 'plain.jpg')`)
	exec(`INSERT INTO asset_tags (node_id, tag, source, created_by_user_id) VALUES (1, 'zebrastripe', 'user', 1)`)
	exec(`INSERT INTO asset_annotations (node_id, kind, text, source, created_by_user_id) VALUES (1, 'description', 'cobalt lantern', 'assistant', 1)`)

	var hits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM media_nodes_fts WHERE media_nodes_fts MATCH 'zebrastripe'`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("fts tag hits = %d, want 1", hits)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM media_nodes_fts WHERE media_nodes_fts MATCH 'lantern'`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("fts description hits = %d, want 1", hits)
	}
	if _, err := database.Exec(`INSERT INTO asset_tags (node_id, tag, source, created_by_user_id) VALUES (1, 'Zebra', 'user', 1)`); err == nil {
		t.Fatal("uppercase tag inserted, want CHECK failure")
	}
	if _, err := database.Exec(`INSERT INTO asset_tags (node_id, tag, source, created_by_user_id) VALUES (1, 'ok', 'robot', 1)`); err == nil {
		t.Fatal("source robot inserted, want CHECK failure")
	}
	if _, err := database.Exec(`INSERT INTO asset_annotations (node_id, kind, text, source, created_by_user_id) VALUES (1, 'description', 'second', 'assistant', 1)`); err == nil {
		t.Fatal("second assistant description inserted, want unique failure")
	}

	if err := goose.DownTo(database, migrationsDir, 47); err != nil {
		t.Fatal(err)
	}
	var leftover int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('asset_tags', 'asset_annotations')`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("Down left tag tables (%d)", leftover)
	}
	var nameHits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM media_nodes_fts WHERE media_nodes_fts MATCH 'plain'`).Scan(&nameHits); err != nil {
		t.Fatal(err)
	}
	if nameHits != 1 {
		t.Fatalf("filename fts after Down = %d, want 1", nameHits)
	}
	var tagHits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM media_nodes_fts WHERE media_nodes_fts MATCH 'zebrastripe'`).Scan(&tagHits); err != nil {
		t.Fatal(err)
	}
	if tagHits != 0 {
		t.Fatalf("tag still indexed after Down: %d", tagHits)
	}
}
