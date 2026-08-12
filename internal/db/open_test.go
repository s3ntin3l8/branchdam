package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "branchdam.db")
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return database
}

// openRawWriter opens just the writer pool with pragmas applied but WITHOUT
// running migrations, so TestMigrateUpDownUp can drive goose directly.
func openRawWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	registerDrivers()
	writerDB, err := sql.Open(driverRW, fmt.Sprintf("file:%s?_txlock=immediate", path))
	if err != nil {
		t.Fatalf("open raw writer: %v", err)
	}
	writerDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = writerDB.Close() })
	return writerDB
}

// TestForeignKeysEnforced is the load-bearing test for docs/schema.md fix #6:
// every FK in the schema is RESTRICT rather than CASCADE, which only means
// anything if foreign_keys enforcement is actually on -- it defaults OFF in
// SQLite and is connection-scoped, so it must be set on every pooled
// connection (see applyCommonPragmas), not once at startup.
func TestForeignKeysEnforced(t *testing.T) {
	database := openTestDB(t)

	var fkEnabled int
	if err := database.writer.QueryRow("PRAGMA foreign_keys").Scan(&fkEnabled); err != nil {
		t.Fatalf("query foreign_keys pragma (writer): %v", err)
	}
	if fkEnabled != 1 {
		t.Fatalf("writer PRAGMA foreign_keys = %d, want 1", fkEnabled)
	}
	if err := database.reader.QueryRow("PRAGMA foreign_keys").Scan(&fkEnabled); err != nil {
		t.Fatalf("query foreign_keys pragma (reader): %v", err)
	}
	if fkEnabled != 1 {
		t.Fatalf("reader PRAGMA foreign_keys = %d, want 1", fkEnabled)
	}

	// Insert a media_node referencing a storage_location_id that doesn't
	// exist. Without foreign_keys=ON this silently succeeds; with it on, it
	// must fail -- proving fix #6 (RESTRICT-only FKs) is actually enforced,
	// not just declared.
	_, execErr := database.writer.Exec(
		`INSERT INTO media_nodes (node_uuid, storage_location_id, file_path, file_name)
		 VALUES ('00000000-0000-7000-8000-000000000001', 999999, '/nope', 'nope')`,
	)
	if execErr == nil {
		t.Fatal("insert with dangling storage_location_id succeeded, want FOREIGN KEY constraint failure")
	}
	if !strings.Contains(execErr.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("error = %q, want a FOREIGN KEY constraint failure", execErr)
	}
}

// TestReaderRejectsWrites is the load-bearing test for the query_only design
// choice explained in applyReaderPragmas: the reader pool must reject writes
// at the connection level, independent of any application-level discipline
// about which pool a given code path uses.
func TestReaderRejectsWrites(t *testing.T) {
	database := openTestDB(t)

	_, err := database.reader.Exec(
		`INSERT INTO storage_locations (name, root_path, tier) VALUES ('x', '/x', 'PROJECTS')`,
	)
	if err == nil {
		t.Fatal("write through the reader pool succeeded, want a query_only rejection")
	}
	if !strings.Contains(err.Error(), "attempt to write a readonly database") {
		t.Errorf("error = %q, want a readonly-database rejection", err)
	}
}

// TestMigrateUpDownUp proves the migration is reversible and idempotent: Up,
// then Down, then Up again must all succeed cleanly. A goose migration that
// isn't cleanly reversible is a landmine for anyone who ever needs to roll
// back in production.
func TestMigrateUpDownUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roundtrip.db")
	writerDB := openRawWriter(t, path)

	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}

	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Up (1st): %v", err)
	}
	assertTablesExist(t, writerDB)

	if err := goose.Down(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Down: %v", err)
	}
	assertTablesAbsent(t, writerDB)

	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Up (2nd): %v", err)
	}
	assertTablesExist(t, writerDB)
}

// TestOpenIsIdempotent proves Open (which runs migrations at startup) can be
// called against an already-migrated database without error -- the normal
// case of restarting the server against an existing data volume.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")

	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second Open (restart): %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
}

func assertTablesExist(t *testing.T, writerDB *sql.DB) {
	t.Helper()
	for _, table := range []string{"storage_locations", "media_nodes", "media_edges", "remote_sync_state", "node_metadata", "event_queue", "scan_jobs"} {
		var name string
		err := writerDB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after migrate up: %v", table, err)
		}
	}
}

func assertTablesAbsent(t *testing.T, writerDB *sql.DB) {
	t.Helper()
	for _, table := range []string{"storage_locations", "media_nodes", "media_edges"} {
		var name string
		err := writerDB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("table %q still present after migrate down (err=%v)", table, err)
		}
	}
}
