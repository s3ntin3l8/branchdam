package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// TestMigration00041BackfillsFullHashProvenance covers 00041's backfill:
// every node with a 64-character full_hash gets a row vouching for that
// value. Every Tier-3 node is AGENT, scanned or not (a pre-#503 device
// assertion has no node_creators row and cannot be told apart), as is a
// device-created node on any tier; only a scanned node off Tier 3 is
// SERVER. Nodes without a full hash get none.
//
// Migrates to 40 first (a no-op past 37 on a branch without 00038-00040)
// so the seed sees the schema exactly as 00041 will.
func TestMigration00041BackfillsFullHashProvenance(t *testing.T) {
	writerDB := openRawWriter(t, filepath.Join(t.TempDir(), "provenance-backfill.db"))
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 40); err != nil {
		t.Fatalf("up to 40: %v", err)
	}
	hash := func(c string) string { return "'" + strings.Repeat(c, 64) + "'" }
	node := func(id, fullHash string) string {
		return `INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, full_hash) VALUES (` +
			id + `, 'uuid-` + id + `', 1, '/archive/` + id + `.mov', '` + id + `.mov', ` + fullHash + `)`
	}
	for _, stmt := range []string{
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'archive', '/archive', 'TIER3_MASTER_ARCHIVE')`,
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (2, 'scratch', '/scratch', 'TIER1_LOCAL_SCRATCH')`,
		node("1", hash("a")), // Tier 3, no node_creators row (scanned, or a pre-#503 device assertion)
		node("2", hash("b")), // Tier 3, device-created
		node("3", "NULL"),    // device-created, no hash
		node("4", "NULL"),    // scanned, no hash
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, full_hash) VALUES (5, 'uuid-5', 2, '/scratch/5.mov', '5.mov', ` + hash("c") + `)`, // Tier 1, scanned
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, full_hash) VALUES (6, 'uuid-6', 2, '/scratch/6.mov', '6.mov', ` + hash("d") + `)`, // Tier 1, device-created
		`INSERT INTO node_creators (node_uuid, agent_id) VALUES ('uuid-2', 'dev'), ('uuid-3', 'dev'), ('uuid-6', 'dev')`,
	} {
		if _, err := writerDB.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := goose.UpTo(writerDB, migrationsDir, 41); err != nil {
		t.Fatalf("up to 41: %v", err)
	}

	got := map[int64]string{}
	rows, err := writerDB.Query(`SELECT node_id, source, full_hash FROM full_hash_provenance`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test
	for rows.Next() {
		var id int64
		var source, fh string
		if err := rows.Scan(&id, &source, &fh); err != nil {
			t.Fatal(err)
		}
		got[id] = source + ":" + fh[:1]
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{1: "AGENT:a", 2: "AGENT:b", 5: "SERVER:c", 6: "AGENT:d"}
	if len(got) != len(want) {
		t.Errorf("full_hash_provenance = %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("node %d: provenance %q, want %q", id, got[id], w)
		}
	}
}

// TestMigration00042BackfillsMismatchBackoff: a MISMATCH recorded under
// 00041 counts as the first mismatch and is due a day after it was
// checked; other rows start with no mismatches and no retry time.
func TestMigration00042BackfillsMismatchBackoff(t *testing.T) {
	writerDB := openRawWriter(t, filepath.Join(t.TempDir(), "mismatch-backfill.db"))
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(writerDB, migrationsDir, 41); err != nil {
		t.Fatalf("up to 41: %v", err)
	}
	hash := func(c string) string { return "'" + strings.Repeat(c, 64) + "'" }
	for _, stmt := range []string{
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'archive', '/archive', 'TIER3_MASTER_ARCHIVE')`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, full_hash) VALUES (1, 'u1', 1, '/archive/1.mov', '1.mov', ` + hash("a") + `)`,
		`INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, full_hash) VALUES (2, 'u2', 1, '/archive/2.mov', '2.mov', ` + hash("b") + `)`,
		`INSERT INTO full_hash_provenance (node_id, full_hash, source, rehash_state, rehash_checked_at, rehash_attempts, rehash_computed)
		 VALUES (1, ` + hash("a") + `, 'AGENT', 'MISMATCH', 1000, 30, ` + hash("c") + `)`,
		`INSERT INTO full_hash_provenance (node_id, full_hash, source, rehash_state, rehash_checked_at, rehash_attempts)
		 VALUES (2, ` + hash("b") + `, 'AGENT', 'ERROR', 1000, 5)`,
	} {
		if _, err := writerDB.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := goose.UpTo(writerDB, migrationsDir, 42); err != nil {
		t.Fatalf("up to 42: %v", err)
	}
	for _, tc := range []struct {
		id         int64
		mismatches int64
		retryAt    sql.NullInt64
	}{
		{1, 1, sql.NullInt64{Int64: 1000 + 86400, Valid: true}},
		{2, 0, sql.NullInt64{}},
	} {
		var mismatches int64
		var retryAt sql.NullInt64
		if err := writerDB.QueryRow(`SELECT rehash_mismatches, rehash_retry_at FROM full_hash_provenance WHERE node_id = ?`, tc.id).
			Scan(&mismatches, &retryAt); err != nil {
			t.Fatal(err)
		}
		if mismatches != tc.mismatches || retryAt != tc.retryAt {
			t.Errorf("node %d: mismatches=%d retry_at=%+v, want %d %+v", tc.id, mismatches, retryAt, tc.mismatches, tc.retryAt)
		}
	}
}

// TestRecordFullHashSource pins RecordFullHashProvenance's upsert rules:
// AGENT never downgrades a SERVER row for the same value, a new value
// replaces the row and its re-hash bookkeeping, SERVER always wins, and a
// nil or malformed hash records nothing.
func TestRecordFullHashSource(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	h1, h2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
	var nodeID int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{Name: "a", RootPath: "/a", Tier: "TIER3_MASTER_ARCHIVE"})
		if err != nil {
			return err
		}
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-n", StorageLocationID: loc.ID, FilePath: "/a/n.mov", FileName: "n.mov",
			IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		nodeID = n.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record := func(h *string, source string) {
		t.Helper()
		if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			return RecordFullHashSource(ctx, q, nodeID, h, source)
		}); err != nil {
			t.Fatalf("record %s: %v", source, err)
		}
	}
	get := func() sqlcgen.FullHashProvenance {
		t.Helper()
		p, err := database.Reader.GetFullHashProvenance(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	record(nil, HashSourceServer)
	short := "abc"
	record(&short, HashSourceServer)
	if _, err := database.Reader.GetFullHashProvenance(ctx, nodeID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("nil/short hash recorded a row (err = %v)", err)
	}

	record(&h1, HashSourceAgent)
	if p := get(); p.Source != HashSourceAgent || p.FullHash != h1 {
		t.Fatalf("after AGENT h1: %+v", p)
	}
	// A failed re-hash, then the same assertion again: bookkeeping kept.
	if _, err := database.InTxResult(ctx, func(q *sqlcgen.Queries) (int64, error) {
		return q.RecordRehashMismatch(ctx, sqlcgen.RecordRehashMismatchParams{
			NodeID: nodeID, FullHash: h1,
			RehashComputed: sql.NullString{String: h2, Valid: true},
			RehashRetryAt:  sql.NullInt64{Int64: 5_000, Valid: true},
		})
	}); err != nil {
		t.Fatal(err)
	}
	record(&h1, HashSourceAgent)
	if p := get(); p.RehashState.String != "MISMATCH" || p.RehashAttempts != 1 || p.RehashMismatches != 1 || p.RehashRetryAt.Int64 != 5_000 {
		t.Fatalf("re-asserting the same value reset bookkeeping: %+v", p)
	}

	record(&h1, HashSourceServer)
	if p := get(); p.Source != HashSourceServer || p.RehashState.Valid || p.RehashAttempts != 0 ||
		p.RehashMismatches != 0 || p.RehashRetryAt.Valid {
		t.Fatalf("after SERVER h1: %+v", p)
	}
	record(&h1, HashSourceAgent)
	if p := get(); p.Source != HashSourceServer {
		t.Fatalf("AGENT downgraded a SERVER row for the same value: %+v", p)
	}
	record(&h2, HashSourceAgent)
	if p := get(); p.Source != HashSourceAgent || p.FullHash != h2 {
		t.Fatalf("a new asserted value must replace the row: %+v", p)
	}
}
