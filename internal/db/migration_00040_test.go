package db

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigration00040BackfillsTrashTimesAndUploadIndexing covers both data
// halves of migration 00040:
//
//   - every node TRASHED at upgrade time gets a node_trash_times row whose
//     trashed_at is the updated_at MarkNodeTrashed stamped (never earlier
//     than the real trash time, so the purge can only be delayed by it);
//     other nodes get no row;
//   - an INDEXED_SHALLOW node that has a full hash (a pre-fix web or agent
//     upload) becomes INDEXED_FULL, the status the scanner assigns; shallow
//     nodes without a full hash and every other status are untouched.
func TestMigration00040BackfillsTrashTimesAndUploadIndexing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trash-times-backfill.db")
	writerDB := openRawWriter(t, path)
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 37); err != nil {
		t.Fatalf("up to 37: %v", err)
	}
	hash := func(c string) string { return "'" + strings.Repeat(c, 64) + "'" }
	node := func(id, uuidSuffix, lifecycle, indexing, fullHash string, updatedAt int) string {
		return `INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, lifecycle_state, indexing_status, full_hash, updated_at) VALUES (` +
			id + `, '018f0000-0000-7000-8000-0000000000` + uuidSuffix + `', 1, '/media/` + id + `.jpg', '` + id + `.jpg', '` +
			lifecycle + `', '` + indexing + `', ` + fullHash + `, ` + strconv.Itoa(updatedAt) + `)`
	}
	seed := []string{
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'media', '/media', 'TIER1_LOCAL_SCRATCH')`,
		node("1", "01", "TRASHED", "INDEXED_FULL", hash("a"), 5000),
		node("2", "02", "ACTIVE", "INDEXED_SHALLOW", hash("b"), 6000), // uploaded pre-fix
		node("3", "03", "ACTIVE", "INDEXED_SHALLOW", "NULL", 7000),    // genuinely shallow
		node("4", "04", "MISSING", "PENDING", "NULL", 8000),
		node("5", "05", "TRASHED", "INDEXED_SHALLOW", "NULL", 9000),
	}
	for _, stmt := range seed {
		if _, err := writerDB.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := goose.UpTo(writerDB, migrationsDir, 40); err != nil {
		t.Fatalf("up to 40: %v", err)
	}

	trashedAt := map[int64]int64{}
	rows, err := writerDB.Query(`SELECT node_id, trashed_at FROM node_trash_times`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test
	for rows.Next() {
		var id int64
		var at sql.NullInt64
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		trashedAt[id] = at.Int64
	}
	if len(trashedAt) != 2 || trashedAt[1] != 5000 || trashedAt[5] != 9000 {
		t.Errorf("node_trash_times = %v, want map[1:5000 5:9000]", trashedAt)
	}

	for id, want := range map[int]string{1: "INDEXED_FULL", 2: "INDEXED_FULL", 3: "INDEXED_SHALLOW", 4: "PENDING", 5: "INDEXED_SHALLOW"} {
		var got string
		if err := writerDB.QueryRow(`SELECT indexing_status FROM media_nodes WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("node %d indexing_status = %q, want %q", id, got, want)
		}
	}
}
