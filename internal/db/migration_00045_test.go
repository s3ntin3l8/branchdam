package db

import (
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// Intentionally has no build constraint: an untagged test binary must fail.
func TestFTS5CompiledIn(t *testing.T) {
	database := openRawWriter(t, filepath.Join(t.TempDir(), "fts5.db"))
	if _, err := database.Exec(`CREATE VIRTUAL TABLE fts5_probe USING fts5(text)`); err != nil {
		t.Fatalf("FTS5 is required; build with -tags sqlite_fts5: %v", err)
	}
}

func TestMigration00045BackfillAndMaintenance(t *testing.T) {
	database := openRawWriter(t, filepath.Join(t.TempDir(), "search.db"))
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(database, migrationsDir, 44); err != nil {
		t.Fatal(err)
	}
	exec := func(statement string) {
		t.Helper()
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`INSERT INTO storage_locations(id,name,root_path,tier) VALUES(1,'archive','/archive','TIER1_LOCAL_SCRATCH')`)
	exec(`INSERT INTO media_nodes(id,node_uuid,storage_location_id,file_path,file_name,camera_model,lens_model)
 VALUES(1,'one',1,'/archive/holiday/clip.mov','clip.mov','Alpha','Telephoto'),
       (2,'two',1,'/archive/other.mov','other.mov',NULL,NULL)`)
	exec(`INSERT INTO node_metadata(node_id,source,key,value) VALUES
 (1,'exiftool','EXIF:Make','Sony'), (1,'exiftool','EXIF:DateTimeOriginal','2026:07:15'),
 (1,'exiftool','XMP:Subject','UnindexedTag'), (1,'internal','project_name','Sunset'),
 (1,'internal','timeline_name','Roughcut')`)
	if err := goose.UpTo(database, migrationsDir, 45); err != nil {
		t.Fatal(err)
	}
	match := func(query string, want int) {
		t.Helper()
		var count int
		if err := database.QueryRow(`SELECT count(*) FROM media_nodes_fts WHERE media_nodes_fts MATCH ?`, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("query %q count = %d, want %d", query, count, want)
		}
	}
	for _, q := range []string{"clip", "holiday", "sony", "alpha", "telephoto", "2026", "sunset", "roughcut"} {
		match(q, 1)
	}
	match("archive", 0) // Search relative paths, never the storage root.
	match("UnindexedTag", 0)
	exec(`UPDATE node_metadata SET value='Canon' WHERE key='EXIF:Make'`)
	match("sony", 0)
	match("canon", 1)
	exec(`UPDATE node_metadata SET node_id=2 WHERE key='EXIF:Make'`)
	var id int
	if err := database.QueryRow(`SELECT rowid FROM media_nodes_fts WHERE media_nodes_fts MATCH 'canon'`).Scan(&id); err != nil || id != 2 {
		t.Fatalf("metadata move: id=%d err=%v", id, err)
	}
	exec(`DELETE FROM node_metadata WHERE key='EXIF:Make'`)
	match("canon", 0)
	exec(`INSERT INTO node_metadata(node_id,source,key,value) VALUES(1,'exiftool','EXIF:Model','Zebra') ON CONFLICT(node_id,source,key) DO UPDATE SET value=excluded.value`)
	match("zebra", 1)
	exec(`INSERT INTO node_metadata(node_id,source,key,value) VALUES(1,'exiftool','EXIF:Model','Nikon') ON CONFLICT(node_id,source,key) DO UPDATE SET value=excluded.value`)
	match("zebra", 0)
	match("nikon", 1)
	exec(`UPDATE node_metadata SET key='XMP:Label' WHERE key='EXIF:Model'`)
	match("nikon", 0)
	exec(`UPDATE node_metadata SET key='EXIF:Model' WHERE value='Nikon'`)
	match("nikon", 1)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO node_metadata(node_id,source,key,value) VALUES(1,'exiftool','EXIF:Make','Rollbackbrand')`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	match("rollbackbrand", 0)
	exec(`UPDATE media_nodes SET file_name='renamed.mov', file_path='/archive/travel/renamed.mov', camera_model='Lumix', lens_model='Prime' WHERE id=1`)
	match("clip", 0)
	match("holiday", 0)
	match("alpha", 0)
	match("telephoto", 0)
	match("renamed", 1)
	match("travel", 1)
	match("lumix", 1)
	match("prime", 1)
	exec(`UPDATE storage_locations SET root_path='/archive/travel' WHERE id=1`)
	match("travel", 0)
	exec(`DELETE FROM node_metadata WHERE node_id=1`)
	exec(`DELETE FROM media_nodes WHERE id=1`)
	match("renamed", 0)
	match("nikon", 0)
	// Down removes every object, and the next Up backfills the remaining node.
	if err := goose.Down(database, migrationsDir); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := database.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE '%fts%' OR name='v_media_nodes_search'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("Down left search objects: count=%d err=%v", remaining, err)
	}
	if err := goose.UpTo(database, migrationsDir, 45); err != nil {
		t.Fatal(err)
	}
	match("other", 1)
}
