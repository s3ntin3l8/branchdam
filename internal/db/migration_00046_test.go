package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigration00046ActorAuditCredential covers the nullable
// auth_provider / pat_id columns: old rows stay NULL, the CHECK and
// the user_pats foreign key are enforced, and Down removes both columns.
func TestMigration00046ActorAuditCredential(t *testing.T) {
	database := openRawWriter(t, filepath.Join(t.TempDir(), "audit-cred.db"))
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(database, migrationsDir, 45); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO actor_audit (actor_kind, actor_name, event, resource_type) VALUES ('system', 'system', 'scan.started', 'scan_job')`); err != nil {
		t.Fatalf("pre-migration insert: %v", err)
	}
	if err := goose.UpTo(database, migrationsDir, 46); err != nil {
		t.Fatal(err)
	}

	var provider sql.NullString
	var patID sql.NullInt64
	if err := database.QueryRow(`SELECT auth_provider, pat_id FROM actor_audit WHERE event = 'scan.started'`).Scan(&provider, &patID); err != nil {
		t.Fatal(err)
	}
	if provider.Valid || patID.Valid {
		t.Fatalf("pre-migration row = (%v, %v), want NULL, NULL", provider, patID)
	}

	res, err := database.Exec(
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('alice', 'alice@example.com', 'hash', 1, 'local', unixepoch(), 'test', 'local', 'alice')`,
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	res, err = database.Exec(`INSERT INTO user_pats (user_id, name, hashed_key, scopes_json) VALUES (?, 'claude-desktop', 'hash46', '[]')`, userID)
	if err != nil {
		t.Fatalf("insert pat: %v", err)
	}
	tokenID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO actor_audit (actor_kind, actor_name, event, resource_type, auth_provider, pat_id) VALUES ('user', 'alice', 'edge.review_confirmed', 'edge', 'pat', ?)`,
		tokenID,
	); err != nil {
		t.Fatalf("insert attributed row: %v", err)
	}
	for _, provider := range []string{"authentik", "local", "oauth"} {
		if _, err := database.Exec(
			`INSERT INTO actor_audit (actor_kind, actor_name, event, resource_type, auth_provider) VALUES ('user', 'alice', 'settings.updated', 'app_setting', ?)`,
			provider,
		); err != nil {
			t.Fatalf("insert auth_provider %q: %v", provider, err)
		}
	}
	for _, bad := range []string{"", "forward-link", "PAT"} {
		if _, err := database.Exec(
			`INSERT INTO actor_audit (actor_kind, actor_name, event, resource_type, auth_provider) VALUES ('user', 'alice', 'settings.updated', 'app_setting', ?)`,
			bad,
		); err == nil {
			t.Fatalf("auth_provider %q inserted, want CHECK failure", bad)
		}
	}
	if _, err := database.Exec(
		`INSERT INTO actor_audit (actor_kind, actor_name, event, resource_type, auth_provider, pat_id) VALUES ('user', 'alice', 'settings.updated', 'app_setting', 'pat', 999999)`,
	); err == nil {
		t.Fatal("dangling pat_id inserted, want FOREIGN KEY failure")
	}

	if err := goose.Down(database, migrationsDir); err != nil {
		t.Fatal(err)
	}
	var leftover int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('actor_audit') WHERE name IN ('auth_provider', 'pat_id')`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("Down left %d credential columns", leftover)
	}
	if err := goose.Up(database, migrationsDir); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT auth_provider, pat_id FROM actor_audit WHERE event = 'scan.started'`).Scan(&provider, &patID); err != nil {
		t.Fatal(err)
	}
	if provider.Valid || patID.Valid {
		t.Fatalf("row after down/up = (%v, %v), want NULL, NULL", provider, patID)
	}
}
