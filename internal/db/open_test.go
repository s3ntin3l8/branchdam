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

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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

	if err := goose.DownTo(writerDB, migrationsDir, 0); err != nil {
		t.Fatalf("goose DownTo 0: %v", err)
	}
	assertTablesAbsent(t, writerDB)

	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Up (2nd): %v", err)
	}
	assertTablesExist(t, writerDB)
}

func TestResolveSnapshotMigrationSingleStepRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolve-roundtrip.db")
	writerDB := openRawWriter(t, path)
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 26); err != nil {
		t.Fatalf("up to resolve snapshot: %v", err)
	}
	if err := goose.Down(writerDB, migrationsDir); err != nil {
		t.Fatalf("down resolve snapshot: %v", err)
	}
	if _, err := writerDB.Exec("SELECT is_active FROM media_edges LIMIT 0"); err == nil {
		t.Fatal("down migration retained is_active")
	}
	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("re-up resolve snapshot: %v", err)
	}
	if _, err := writerDB.Exec("SELECT is_active FROM media_edges LIMIT 0"); err != nil {
		t.Fatalf("re-up missing is_active: %v", err)
	}
}

func TestResolveSnapshotMigrationRefusesDowngradeWithInactiveEdges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolve-no-resurrection.db")
	writerDB := openRawWriter(t, path)
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 26); err != nil {
		t.Fatalf("up to resolve snapshot: %v", err)
	}
	statements := []string{
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'media', '/media', 'TIER2_EXPORTS')`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name) VALUES (1, '018f0000-0000-7000-8000-000000000101', 1, '/media/a.mov', 'a.mov')`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name) VALUES (2, '018f0000-0000-7000-8000-000000000102', 1, '/media/timeline', 'timeline')`,
		`INSERT INTO media_edges (source_node_id, target_node_id, relationship_type, confidence, tier, resolver, is_active) VALUES (1, 2, 'PROJECT_SIDECAR', 1, 1, 'resolve_project_db', 0)`,
	}
	for _, statement := range statements {
		if _, err := writerDB.Exec(statement); err != nil {
			t.Fatalf("seed inactive edge: %v", err)
		}
	}
	if err := goose.Down(writerDB, migrationsDir); err == nil {
		t.Fatal("downgrade with inactive edge unexpectedly succeeded")
	}
	var inactive int
	if err := writerDB.QueryRow(`SELECT count(*) FROM media_edges WHERE is_active = 0`).Scan(&inactive); err != nil || inactive != 1 {
		t.Fatalf("failed downgrade changed inactive audit row: count=%d err=%v", inactive, err)
	}
}

// TestMigration00032WithReferencingRows verifies that the media_nodes table
// rebuild works when foreign-key child tables contain data. SQLite cannot
// drop a referenced table while foreign_keys is enabled, so this is the
// production-shaped regression case for issue #472.
func TestMigration00032WithReferencingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trashed-lifecycle.db")
	writerDB := openRawWriter(t, path)

	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 31); err != nil {
		t.Fatalf("goose UpTo 31: %v", err)
	}

	statements := []string{
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'media', '/media', 'TIER2_EXPORTS')`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name) VALUES (1, '018f0000-0000-7000-8000-000000000101', 1, '/media/a.mov', 'a.mov')`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name) VALUES (2, '018f0000-0000-7000-8000-000000000102', 1, '/media/timeline', 'timeline')`,
		`INSERT INTO media_edges (source_node_id, target_node_id, relationship_type, confidence, tier, resolver) VALUES (1, 2, 'PROJECT_SIDECAR', 1, 1, 'migration-test')`,
		`INSERT INTO node_metadata (node_id, source, key, value) VALUES (1, 'internal', 'migration-test', 'present')`,
	}
	for _, statement := range statements {
		if _, err := writerDB.Exec(statement); err != nil {
			t.Fatalf("seed migration fixture: %v", err)
		}
	}

	if err := goose.UpTo(writerDB, migrationsDir, 32); err != nil {
		t.Fatalf("goose UpTo 32 with referencing rows: %v", err)
	}

	var foreignKeys int
	if err := writerDB.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys after Up: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys after Up = %d, want 1", foreignKeys)
	}

	var violations int
	if err := writerDB.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check after Up: %v", err)
	}
	if violations != 0 {
		t.Fatalf("foreign_key_check after Up returned %d violations", violations)
	}

	var edgeCount, metadataCount int
	if err := writerDB.QueryRow("SELECT count(*) FROM media_edges WHERE source_node_id = 1 AND target_node_id = 2").Scan(&edgeCount); err != nil {
		t.Fatalf("count preserved media edge: %v", err)
	}
	if err := writerDB.QueryRow("SELECT count(*) FROM node_metadata WHERE node_id = 1 AND key = 'migration-test'").Scan(&metadataCount); err != nil {
		t.Fatalf("count preserved node metadata: %v", err)
	}
	if edgeCount != 1 || metadataCount != 1 {
		t.Fatalf("preserved child rows = media_edges:%d node_metadata:%d, want 1:1", edgeCount, metadataCount)
	}

	if _, err := writerDB.Exec(`UPDATE media_nodes SET lifecycle_state = 'TRASHED' WHERE id = 1`); err != nil {
		t.Fatalf("set TRASHED lifecycle state: %v", err)
	}
	if err := goose.Down(writerDB, migrationsDir); err == nil {
		t.Fatal("downgrade with TRASHED row unexpectedly succeeded")
	}

	var version int64
	if err := writerDB.QueryRow(`
		SELECT version_id
		FROM goose_db_version
		WHERE is_applied = 1
		ORDER BY version_id DESC
		LIMIT 1
	`).Scan(&version); err != nil {
		t.Fatalf("query migration version after refused downgrade: %v", err)
	}
	if version != 32 {
		t.Fatalf("migration version after refused downgrade = %d, want 32", version)
	}

	if _, err := writerDB.Exec(`UPDATE media_nodes SET lifecycle_state = 'ACTIVE' WHERE id = 1`); err != nil {
		t.Fatalf("clear TRASHED lifecycle state: %v", err)
	}

	if err := goose.Down(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Down 32: %v", err)
	}
	if err := writerDB.QueryRow(`SELECT version_id FROM goose_db_version WHERE is_applied = 1 ORDER BY version_id DESC LIMIT 1`).Scan(&version); err != nil {
		t.Fatalf("query migration version after successful downgrade: %v", err)
	}
	if version != 31 {
		t.Fatalf("migration version after successful downgrade = %d, want 31", version)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 32); err != nil {
		t.Fatalf("goose Up 32 after Down: %v", err)
	}

	if _, err := writerDB.Exec(`
		INSERT INTO media_edges (source_node_id, target_node_id, relationship_type, confidence, tier, resolver)
		VALUES (1, 999, 'PROJECT_SIDECAR', 1, 1, 'foreign-key-test')
	`); err == nil {
		t.Fatal("insert with dangling media_edges target succeeded, want FOREIGN KEY constraint failure")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("dangling media_edges insert error = %q, want FOREIGN KEY constraint failure", err)
	}
}

// TestMigration00032ForeignKeyGuardRejectsDanglingRows verifies that the
// pre-commit foreign-key guard fails the migration before Goose records
// version 32 when existing child data is invalid.
func TestMigration00032ForeignKeyGuardRejectsDanglingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trashed-lifecycle-invalid-fk.db")
	writerDB := openRawWriter(t, path)

	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 31); err != nil {
		t.Fatalf("goose UpTo 31: %v", err)
	}

	statements := []string{
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'media', '/media', 'TIER2_EXPORTS')`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name) VALUES (1, '018f0000-0000-7000-8000-000000000201', 1, '/media/a.mov', 'a.mov')`,
	}
	for _, statement := range statements {
		if _, err := writerDB.Exec(statement); err != nil {
			t.Fatalf("seed migration fixture: %v", err)
		}
	}
	if _, err := writerDB.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("disable foreign keys for invalid fixture: %v", err)
	}
	if _, err := writerDB.Exec(`
		INSERT INTO media_edges (source_node_id, target_node_id, relationship_type, confidence, tier, resolver)
		VALUES (1, 999, 'PROJECT_SIDECAR', 1, 1, 'invalid-fk-test')
	`); err != nil {
		t.Fatalf("seed dangling media edge: %v", err)
	}
	if _, err := writerDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("restore foreign keys before migration: %v", err)
	}

	err := goose.UpTo(writerDB, migrationsDir, 32)
	if err == nil {
		t.Fatal("goose UpTo 32 with dangling child row unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed: ok = 1") {
		t.Errorf("goose UpTo 32 error = %q, want FK guard CHECK failure", err)
	}

	var version int64
	if err := writerDB.QueryRow(`SELECT version_id FROM goose_db_version WHERE is_applied = 1 ORDER BY version_id DESC LIMIT 1`).Scan(&version); err != nil {
		t.Fatalf("query migration version after rejected migration: %v", err)
	}
	if version != 31 {
		t.Fatalf("migration version after rejected migration = %d, want 31", version)
	}
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

// TestDowngradeIndexSuffixStemEdges backs issue #132's migration 00006:
// AUTO_ACCEPTED filename_stem edges already written for an index-suffix
// ("-N"/"(N)") pair must be downgraded to NEEDS_REVIEW at confidence 0.89
// on migrate-up; edges from a role-suffix ("_proxy" etc) pair are the
// resolver's originally-intended AUTO_ACCEPTED case and must be left
// untouched. The media_nodes fixture rows are inserted via raw SQL scoped
// to goose version 5's column set, not the generated sqlcgen.InsertMediaNode
// (which always targets the CURRENT/HEAD schema) -- since migration 00007
// widened media_nodes, the generated INSERT would otherwise reference
// columns that don't exist yet at version 5, where these rows must
// originate so migration 00006's UPDATE has pre-existing data to act on.
// media_edges itself is unaffected by 00007, so CreateMediaEdge/GetMediaEdge
// still use the normal generated helpers.
func TestDowngradeIndexSuffixStemEdges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "downgrade.db")
	writerDB := openRawWriter(t, path)

	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 5); err != nil {
		t.Fatalf("goose UpTo 5: %v", err)
	}

	q := sqlcgen.New(writerDB)

	// Raw SQL, deliberately not the generated sqlcgen.CreateStorageLocation:
	// that helper's column list always matches the CURRENT (HEAD) schema,
	// which since migration 00008 (#238) includes cache_ttl_hours -- a
	// column that doesn't exist yet at goose version 5, where this fixture
	// row must originate (same reasoning as insertNode below for
	// media_nodes).
	res, err := writerDB.ExecContext(ctx, `INSERT INTO storage_locations (
		name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
	) VALUES ('downgrade_test', '/tmp/downgrade_test', 'TIER2_EXPORTS', 0, 0, 1, unixepoch(), unixepoch())`)
	if err != nil {
		t.Fatalf("insert storage_locations fixture: %v", err)
	}
	locID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId for storage_locations fixture: %v", err)
	}
	loc := sqlcgen.StorageLocation{ID: locID}

	// Raw SQL, deliberately not the generated sqlcgen.InsertMediaNode: that
	// helper's column list always matches the CURRENT (HEAD) schema, but
	// these fixture rows must exist at goose version 5 specifically --
	// version 6 is what this test migrates *to*, to observe its downgrade
	// effect, so the rows have to predate it. Migration 00007 (unrelated:
	// promotes thumb_state/thumb_attempts) later widened media_nodes, which
	// would otherwise make the generated INSERT reference columns that
	// don't exist yet at version 5.
	insertNode := func(uuidSuffix, path, fileName, stem string) int64 {
		t.Helper()
		res, err := writerDB.ExecContext(ctx, `INSERT INTO media_nodes (
			node_uuid, storage_location_id, file_path, file_name, file_ext,
			indexing_status, graph_status, lifecycle_state, filename_stem,
			first_seen_at, last_seen_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'jpg', 'INDEXED_SHALLOW', 'LINKED', 'ACTIVE', ?,
			unixepoch(), unixepoch(), unixepoch(), unixepoch())`,
			"00000000-0000-7000-8000-00000000"+uuidSuffix, loc.ID, path, fileName, stem)
		if err != nil {
			t.Fatalf("insert media_nodes fixture %s: %v", path, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId for %s: %v", path, err)
		}
		return id
	}

	// An index-suffix pair, mimicking a pre-#132 AUTO_ACCEPTED mesh edge:
	// The current generated edge DTO includes is_active (migration 26),
	// while this historical fixture intentionally stops at version 5.
	// Add only that later column so generated edge queries can still seed
	// and inspect the fixture without changing migration 6's behavior.
	if _, err := writerDB.Exec("ALTER TABLE media_edges ADD COLUMN is_active INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1));"); err != nil {
		t.Fatalf("alter table add is_active: %v", err)
	}
	// "photo.jpg" (anchor) -> "photo-2.jpg" (index-suffixed).
	anchor := insertNode("0001", "/photo.jpg", "photo.jpg", "photo")
	indexChild := insertNode("0002", "/photo-2.jpg", "photo-2.jpg", "photo")
	indexEdge, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
		SourceNodeID: anchor, TargetNodeID: indexChild, RelationshipType: "DERIVED_FROM",
		Confidence: 0.90, Tier: 2, Resolver: "filename_stem", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
	})
	if err != nil {
		t.Fatalf("CreateMediaEdge (index pair): %v", err)
	}

	// A role-suffix pair -- the resolver's originally-intended case, must
	// be left untouched: "render.jpg" -> "render_proxy.jpg".
	renderParent := insertNode("0003", "/render.jpg", "render.jpg", "render")
	renderChild := insertNode("0004", "/render_proxy.jpg", "render_proxy.jpg", "render")
	roleEdge, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
		SourceNodeID: renderParent, TargetNodeID: renderChild, RelationshipType: "PROXY_OF",
		Confidence: 0.90, Tier: 2, Resolver: "filename_stem", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
	})
	if err != nil {
		t.Fatalf("CreateMediaEdge (role pair): %v", err)
	}

	// UpTo 7, not just 6: migration 00007 is unrelated additive DDL (adds
	// thumb_state/thumb_attempts with defaults) that doesn't touch any row
	// this test asserts on, but the generated Queries below (GetMediaEdge,
	// GetMediaNodeByID) are built against the current schema and would
	// otherwise fail with "no such column" against a database still sitting
	// at version 6.
	if err := goose.UpTo(writerDB, migrationsDir, 7); err != nil {
		t.Fatalf("goose UpTo 7: %v", err)
	}
	// Add additive source_path_hash column (from migration 15) so GetMediaNodeByID succeeds.
	if _, err := writerDB.Exec("ALTER TABLE media_nodes ADD COLUMN source_path_hash TEXT CHECK (source_path_hash IS NULL OR length(source_path_hash) = 64);"); err != nil {
		t.Fatalf("alter table add source_path_hash: %v", err)
	}
	// Add additive uploaded_by_user_id column (from migration 20) so GetMediaNodeByID succeeds.
	if _, err := writerDB.Exec("ALTER TABLE media_nodes ADD COLUMN uploaded_by_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT;"); err != nil {
		t.Fatalf("alter table add uploaded_by_user_id: %v", err)
	}

	gotIndex, err := q.GetMediaEdge(ctx, indexEdge.ID)
	if err != nil {
		t.Fatalf("GetMediaEdge (index pair) after migration: %v", err)
	}
	if gotIndex.ReviewState != "NEEDS_REVIEW" {
		t.Errorf("index pair review_state = %q, want NEEDS_REVIEW", gotIndex.ReviewState)
	}
	if gotIndex.Confidence != 0.89 {
		t.Errorf("index pair confidence = %v, want 0.89", gotIndex.Confidence)
	}

	gotRole, err := q.GetMediaEdge(ctx, roleEdge.ID)
	if err != nil {
		t.Fatalf("GetMediaEdge (role pair) after migration: %v", err)
	}
	if gotRole.ReviewState != "AUTO_ACCEPTED" {
		t.Errorf("role pair review_state = %q, want AUTO_ACCEPTED (untouched)", gotRole.ReviewState)
	}
	if gotRole.Confidence != 0.90 {
		t.Errorf("role pair confidence = %v, want 0.90 (untouched)", gotRole.Confidence)
	}

	indexChildAfter, err := q.GetMediaNodeByID(ctx, indexChild)
	if err != nil {
		t.Fatalf("GetMediaNodeByID (index child) after migration: %v", err)
	}
	if indexChildAfter.GraphStatus != "NEEDS_REVIEW" {
		t.Errorf("index child graph_status = %q, want NEEDS_REVIEW", indexChildAfter.GraphStatus)
	}

	renderChildAfter, err := q.GetMediaNodeByID(ctx, renderChild)
	if err != nil {
		t.Fatalf("GetMediaNodeByID (role child) after migration: %v", err)
	}
	if renderChildAfter.GraphStatus != "LINKED" {
		t.Errorf("role child graph_status = %q, want LINKED (untouched)", renderChildAfter.GraphStatus)
	}
}

func TestListTier3Candidates(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		sl, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "test_tier3_loc",
			RootPath: "/tmp/test_tier3",
			Tier:     "PROJECTS",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}

		node1, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "00000000-0000-7000-8000-000000000001",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_tier3/file1.jpg",
			FileName:          "file1.jpg",
			FileExt:           "jpg",
			IndexingStatus:    "INDEXED_SHALLOW",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
			CapturedAtUnix:    sql.NullInt64{Int64: 1000, Valid: true},
			CameraSerial:      sql.NullString{String: "SER123", Valid: true},
			LensModel:         sql.NullString{String: "Lens 50mm", Valid: true},
		})
		if err != nil {
			return err
		}

		node2, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "00000000-0000-7000-8000-000000000002",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_tier3/file2.jpg",
			FileName:          "file2.jpg",
			FileExt:           "jpg",
			IndexingStatus:    "INDEXED_SHALLOW",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
			CapturedAtUnix:    sql.NullInt64{Int64: 1001, Valid: true},
			CameraSerial:      sql.NullString{String: "SER123", Valid: true},
			LensModel:         sql.NullString{String: "Lens 50mm", Valid: true},
		})
		if err != nil {
			return err
		}

		candidates, err := q.ListTier3Candidates(ctx, sqlcgen.ListTier3CandidatesParams{
			CameraSerial:     sql.NullString{String: "SER123", Valid: true},
			CapturedAtUnix:   sql.NullInt64{Int64: 999, Valid: true},
			CapturedAtUnix_2: sql.NullInt64{Int64: 1003, Valid: true},
			ID:               node1.ID,
		})
		if err != nil {
			return err
		}
		if len(candidates) != 1 {
			t.Fatalf("got %d candidates, want 1", len(candidates))
		}
		if candidates[0].ID != node2.ID {
			t.Errorf("candidate ID = %d, want %d", candidates[0].ID, node2.ID)
		}
		if candidates[0].CameraSerial.String != "SER123" {
			t.Errorf("camera serial = %q, want SER123", candidates[0].CameraSerial.String)
		}
		if candidates[0].LensModel.String != "Lens 50mm" {
			t.Errorf("lens model = %q, want Lens 50mm", candidates[0].LensModel.String)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction failed: %v", err)
	}
}

func TestGetMediaNodeByFullHash(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		sl, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "loc",
			RootPath: "/tmp/test_hashes",
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}

		fastHash := "0123456789abcdef"
		fullHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		sourcePathHash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

		node, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "00000000-0000-7000-8000-000000000010",
			StorageLocationID: sl.ID,
			FilePath:          "/tmp/test_hashes/file.jpg",
			FileName:          "file.jpg",
			FileExt:           "jpg",
			FastHash:          &fastHash,
			FullHash:          &fullHash,
			SourcePathHash:    &sourcePathHash,
			IndexingStatus:    "INDEXED_SHALLOW",
			GraphStatus:       "UNLINKED",
			LifecycleState:    "ACTIVE",
		})
		if err != nil {
			return err
		}

		byFull, err := q.GetMediaNodeByFullHash(ctx, &fullHash)
		if err != nil {
			return err
		}
		if byFull.ID != node.ID || byFull.NodeUuid != node.NodeUuid {
			t.Errorf("GetMediaNodeByFullHash got ID=%d, want %d", byFull.ID, node.ID)
		}

		fastNodeID, err := q.GetMediaNodeByFastHash(ctx, &fastHash)
		if err != nil {
			return err
		}
		if fastNodeID != node.ID {
			t.Errorf("GetMediaNodeByFastHash got ID=%d, want %d", fastNodeID, node.ID)
		}

		bySource, err := q.GetMediaNodeBySourcePathHash(ctx, &sourcePathHash)
		if err != nil {
			return err
		}
		if bySource.ID != node.ID || bySource.NodeUuid != node.NodeUuid {
			t.Errorf("GetMediaNodeBySourcePathHash got ID=%d, want %d", bySource.ID, node.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction failed: %v", err)
	}
}

// TestDedupExistingHashesMigration verifies migration 00013's survivor selection
// logic: among duplicate non-empty full_hash rows in live states (ACTIVE, HIDDEN),
// the lowest ID (MIN(id)) is preserved as the survivor while other duplicate live
// rows are updated to ARCHIVED. Nodes in MISSING or ARCHIVED states, as well as
// rows with NULL full_hash, are handled properly without erroneous transitions,
// enabling migration 00014's partial unique index creation to succeed.
func TestDedupExistingHashesMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dedup_migration.db")
	writerDB := openRawWriter(t, path)

	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	// Migrate up to version 12 (pre-dedup state)
	if err := goose.UpTo(writerDB, migrationsDir, 12); err != nil {
		t.Fatalf("goose UpTo 12: %v", err)
	}

	// Insert storage location fixture
	res, err := writerDB.ExecContext(ctx, `INSERT INTO storage_locations (
		name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
	) VALUES ('dedup_test', '/tmp/dedup_test', 'TIER3_MASTER_ARCHIVE', 0, 0, 1, unixepoch(), unixepoch())`)
	if err != nil {
		t.Fatalf("insert storage_locations fixture: %v", err)
	}
	locID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId for storage_locations: %v", err)
	}

	insertRawNode := func(id int64, uuidSuffix, filePath, state, fullHash string) {
		t.Helper()
		var hashVal any
		if fullHash == "<NULL>" {
			hashVal = nil
		} else {
			hashVal = fullHash
		}
		_, err := writerDB.ExecContext(ctx, `INSERT INTO media_nodes (
			id, node_uuid, storage_location_id, file_path, file_name, file_ext,
			indexing_status, graph_status, lifecycle_state, full_hash,
			first_seen_at, last_seen_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'file.jpg', 'jpg', 'INDEXED_FULL', 'UNLINKED', ?, ?,
			unixepoch(), unixepoch(), unixepoch(), unixepoch())`,
			id, "00000000-0000-7000-8000-00000000"+uuidSuffix, locID, filePath, state, hashVal)
		if err != nil {
			t.Fatalf("insert media_node id=%d (%s): %v", id, filePath, err)
		}
	}

	hashA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hashC := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hashD := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	// Scenario A: Multiple ACTIVE duplicates -> lowest ID (10) survives, others (11, 12) archived
	insertRawNode(10, "0010", "/tmp/dedup_test/a1.jpg", "ACTIVE", hashA)
	insertRawNode(11, "0011", "/tmp/dedup_test/a2.jpg", "ACTIVE", hashA)
	insertRawNode(12, "0012", "/tmp/dedup_test/a3.jpg", "ACTIVE", hashA)

	// Scenario B: MISSING node (id 20) with live duplicates (ACTIVE id 21, HIDDEN id 22) ->
	// survivor among live is id 21 (MIN(id)), id 22 archived, id 20 remains MISSING
	insertRawNode(20, "0020", "/tmp/dedup_test/b1.jpg", "MISSING", hashB)
	insertRawNode(21, "0021", "/tmp/dedup_test/b2.jpg", "ACTIVE", hashB)
	insertRawNode(22, "0022", "/tmp/dedup_test/b3.jpg", "HIDDEN", hashB)

	// Scenario C: Already ARCHIVED node (id 30) + single ACTIVE node (id 31) -> id 31 stays ACTIVE, id 30 stays ARCHIVED
	insertRawNode(30, "0030", "/tmp/dedup_test/c1.jpg", "ARCHIVED", hashC)
	insertRawNode(31, "0031", "/tmp/dedup_test/c2.jpg", "ACTIVE", hashC)

	// Scenario D: Unique ACTIVE node -> stays ACTIVE
	insertRawNode(40, "0040", "/tmp/dedup_test/d1.jpg", "ACTIVE", hashD)

	// Scenario E: Multiple ACTIVE nodes with NULL full_hash -> both stay ACTIVE (NULLs ignored by dedup)
	insertRawNode(50, "0050", "/tmp/dedup_test/e1.jpg", "ACTIVE", "<NULL>")
	insertRawNode(51, "0051", "/tmp/dedup_test/e2.jpg", "ACTIVE", "<NULL>")

	// Run migration 00013
	if err := goose.UpTo(writerDB, migrationsDir, 13); err != nil {
		t.Fatalf("goose UpTo 13: %v", err)
	}

	getState := func(id int64) string {
		t.Helper()
		var state string
		if err := writerDB.QueryRowContext(ctx, "SELECT lifecycle_state FROM media_nodes WHERE id = ?", id).Scan(&state); err != nil {
			t.Fatalf("get lifecycle_state for id=%d: %v", id, err)
		}
		return state
	}

	// Verify Scenario A:
	if got := getState(10); got != "ACTIVE" {
		t.Errorf("node 10 (survivor) state = %q, want ACTIVE", got)
	}
	if got := getState(11); got != "ARCHIVED" {
		t.Errorf("node 11 (duplicate) state = %q, want ARCHIVED", got)
	}
	if got := getState(12); got != "ARCHIVED" {
		t.Errorf("node 12 (duplicate) state = %q, want ARCHIVED", got)
	}

	// Verify Scenario B:
	if got := getState(20); got != "MISSING" {
		t.Errorf("node 20 (missing) state = %q, want MISSING", got)
	}
	if got := getState(21); got != "ACTIVE" {
		t.Errorf("node 21 (survivor) state = %q, want ACTIVE", got)
	}
	if got := getState(22); got != "ARCHIVED" {
		t.Errorf("node 22 (duplicate) state = %q, want ARCHIVED", got)
	}

	// Verify Scenario C:
	if got := getState(30); got != "ARCHIVED" {
		t.Errorf("node 30 state = %q, want ARCHIVED", got)
	}
	if got := getState(31); got != "ACTIVE" {
		t.Errorf("node 31 state = %q, want ACTIVE", got)
	}

	// Verify Scenario D:
	if got := getState(40); got != "ACTIVE" {
		t.Errorf("node 40 state = %q, want ACTIVE", got)
	}

	// Verify Scenario E (NULLs):
	if got := getState(50); got != "ACTIVE" {
		t.Errorf("node 50 state = %q, want ACTIVE", got)
	}
	if got := getState(51); got != "ACTIVE" {
		t.Errorf("node 51 state = %q, want ACTIVE", got)
	}

	// Run migration 00014 to prove the partial unique index applies cleanly without constraint errors
	if err := goose.UpTo(writerDB, migrationsDir, 14); err != nil {
		t.Fatalf("goose UpTo 14 failed after migration 00013 dedup: %v", err)
	}

	// Verify unique index exists in sqlite_master
	var indexName string
	err = writerDB.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='ux_media_nodes_live_full_hash'",
	).Scan(&indexName)
	if err != nil {
		t.Fatalf("ux_media_nodes_live_full_hash index missing: %v", err)
	}

	// Verify inserting duplicate live node with hashA now violates unique constraint
	_, insertErr := writerDB.ExecContext(ctx, `INSERT INTO media_nodes (
		node_uuid, storage_location_id, file_path, file_name, file_ext,
		indexing_status, graph_status, lifecycle_state, full_hash,
		first_seen_at, last_seen_at, created_at, updated_at
	) VALUES ('00000000-0000-7000-8000-000000000099', ?, '/tmp/dedup_test/dup.jpg', 'file.jpg', 'jpg',
		'INDEXED_FULL', 'UNLINKED', 'ACTIVE', ?, unixepoch(), unixepoch(), unixepoch(), unixepoch())`,
		locID, hashA)
	if insertErr == nil {
		t.Fatal("insert of duplicate live full_hash succeeded, want UNIQUE constraint failure")
	}
	if !strings.Contains(insertErr.Error(), "UNIQUE constraint failed") {
		t.Errorf("error = %q, want UNIQUE constraint failure", insertErr)
	}
}

// TestSourcePathHashIndexAndQueryPlan verifies that the partial index on
// (source_path_hash, id DESC) exists and that queries filtering on
// source_path_hash utilize the index without performing full table scans.
func TestSourcePathHashIndexAndQueryPlan(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	// 1. Verify index exists in sqlite_master
	var indexName string
	err := database.reader.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name=?",
		"idx_media_nodes_source_path_hash_id",
	).Scan(&indexName)
	if err != nil {
		t.Fatalf("idx_media_nodes_source_path_hash_id index not found in schema: %v", err)
	}

	// 2. Seed some media nodes with source_path_hash
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		sl, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name:     "sph_loc",
			RootPath: "/tmp/sph_test",
			Tier:     "PROJECTS",
			ReadOnly: 0,
			Prunable: 0,
		})
		if err != nil {
			return err
		}

		sph1 := "1111111111111111111111111111111111111111111111111111111111111111"
		sph2 := "2222222222222222222222222222222222222222222222222222222222222222"

		for i := 0; i < 5; i++ {
			h := sph1
			if i%2 == 1 {
				h = sph2
			}
			_, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          fmt.Sprintf("00000000-0000-7000-8000-%012d", i+1),
				StorageLocationID: sl.ID,
				FilePath:          fmt.Sprintf("/tmp/sph_test/file%d.jpg", i),
				FileName:          fmt.Sprintf("file%d.jpg", i),
				FileExt:           "jpg",
				SourcePathHash:    &h,
				IndexingStatus:    "INDEXED_SHALLOW",
				GraphStatus:       "UNLINKED",
				LifecycleState:    "ACTIVE",
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed media nodes: %v", err)
	}

	// 3. Explain query plan for GetMediaNodeBySourcePathHash (actual query in queries/media_nodes.sql)
	query := `EXPLAIN QUERY PLAN
		SELECT id, node_uuid, file_path, lifecycle_state, indexing_status
		FROM media_nodes
		WHERE source_path_hash = '1111111111111111111111111111111111111111111111111111111111111111'
		  AND lifecycle_state IN ('ACTIVE', 'HIDDEN')
		ORDER BY id DESC
		LIMIT 1;`

	rows, err := database.reader.Query(query)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("; ")
	}

	gotPlan := plan.String()
	t.Logf("Query plan: %s", gotPlan)
	if !strings.Contains(gotPlan, "idx_media_nodes_source_path_hash_id") {
		t.Errorf("query plan does not use idx_media_nodes_source_path_hash_id: %s", gotPlan)
	}
	if strings.Contains(gotPlan, "SCAN media_nodes") {
		t.Errorf("query plan falls back to a full table scan: %s", gotPlan)
	}
	if strings.Contains(gotPlan, "TEMP B-TREE") {
		t.Errorf("query plan requires a temporary sort: %s", gotPlan)
	}
}

// TestMigration00028UpDownRoundTrip verifies that migration 00028 is
// reversible and that the Down state matches the forward-link convention
// (auth_provider='forward-link', external_uid=username), and that Up
// repairs it back to auth_provider='local'.
func TestMigration00028UpDownRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m00028-roundtrip.db")
	writerDB := openRawWriter(t, path)

	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}

	// Up to 00028 (inclusive) to get the schema + backfill.
	if err := goose.UpTo(writerDB, migrationsDir, 28); err != nil {
		t.Fatalf("goose UpTo 28: %v", err)
	}

	// Insert a local user that already has auth_provider='local'
	// (simulates a row created after 00020).
	if _, err := writerDB.Exec(
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('alice', 'alice@example.com', 'hash', 0, 'local', unixepoch(), 'test', 'local', 'alice')`,
	); err != nil {
		t.Fatalf("insert local user: %v", err)
	}

	// Down 00028: should set auth_provider='forward-link'.
	if err := goose.Down(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Down 00028: %v", err)
	}

	var provider string
	if err := writerDB.QueryRow(`SELECT auth_provider FROM users WHERE username = 'alice'`).Scan(&provider); err != nil {
		t.Fatalf("scan auth_provider after Down: %v", err)
	}
	if provider != "forward-link" {
		t.Errorf("auth_provider after Down = %q, want %q", provider, "forward-link")
	}

	// Re-Up 00028: should repair auth_provider='local'.
	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("goose Up 00028 (re-up): %v", err)
	}

	if err := writerDB.QueryRow(`SELECT auth_provider FROM users WHERE username = 'alice'`).Scan(&provider); err != nil {
		t.Fatalf("scan auth_provider after re-Up: %v", err)
	}
	if provider != "local" {
		t.Errorf("auth_provider after re-Up = %q, want %q", provider, "local")
	}

	// Verify external_uid round-tripped correctly.
	var extUID string
	if err := writerDB.QueryRow(`SELECT external_uid FROM users WHERE username = 'alice'`).Scan(&extUID); err != nil {
		t.Fatalf("scan external_uid: %v", err)
	}
	if extUID != "alice" {
		t.Errorf("external_uid = %q, want %q", extUID, "alice")
	}
}
