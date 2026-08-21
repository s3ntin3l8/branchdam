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
