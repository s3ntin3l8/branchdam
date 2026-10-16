package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// migrationUpSQL returns the "-- +goose Up" section of an embedded
// migration, so a data-repair migration's statements can be replayed
// against rows seeded after the schema exists (goose applies it once, at
// db.Open time, against an empty test database). Reading the embedded
// file -- rather than hand-copying its SQL into the test -- keeps the two
// from drifting.
func migrationUpSQL(t *testing.T, name string) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	s := string(raw)
	up := strings.Index(s, "-- +goose Up")
	down := strings.Index(s, "-- +goose Down")
	if up < 0 || down < up {
		t.Fatalf("migration %s has no Up/Down sections", name)
	}
	return s[up+len("-- +goose Up") : down]
}

// TestRepairPATPhantomUsers covers migration 00039: rows the old PAT
// middleware attributed to a phantom ('authentik', 'pat:<hash prefix>')
// user are re-pointed at the token's owner, the phantom is demoted and
// disabled (never deleted), and look-alike rows that fail any of the
// phantom predicates are left untouched.
func TestRepairPATPhantomUsers(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	const prefix = "abcdef12"
	var (
		ownerID, phantomID, unmatchedID, realUserID int64
		pairingID, scanID, nodeID                   int64
	)
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		if ownerID, err = q.EnsureBootstrapUser(ctx); err != nil {
			return err
		}
		if _, err = q.CreateUserPAT(ctx, sqlcgen.CreateUserPATParams{
			UserID: ownerID, Name: "ops", HashedKey: prefix + strings.Repeat("0", 56), ScopesJson: `["*"]`,
		}); err != nil {
			return err
		}
		// The phantom the old code provisioned: ExternalUID is the
		// token's hash prefix, Username the owner's id.
		if phantomID, err = q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik", ExternalUid: "pat:" + prefix, Username: fmt.Sprintf("pat:%d", ownerID), IsAdmin: 1,
		}); err != nil {
			return err
		}
		// Same shape, but no PAT of the named owner has this prefix:
		// must not be touched.
		if unmatchedID, err = q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik", ExternalUid: "pat:99999999", Username: "pat:424242", IsAdmin: 1,
		}); err != nil {
			return err
		}
		if realUserID, err = q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik", ExternalUid: "uid-real", Username: "carol", IsAdmin: 1,
		}); err != nil {
			return err
		}

		pairing, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID: "agent-pat", FriendlyLabel: "PAT phone", CreatedAt: 1000, CreatedBy: "user:pat",
			QrSvg: []byte("<svg/>"), UserID: sql.NullInt64{Int64: phantomID, Valid: true},
		})
		if err != nil {
			return err
		}
		pairingID = pairing.ID
		scan, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			Kind: "FULL_SCAN", StartedByUserID: sql.NullInt64{Int64: phantomID, Valid: true},
		})
		if err != nil {
			return err
		}
		scanID = scan.ID
		sl, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "pat_repair_loc", RootPath: "/tmp/pat_repair", Tier: "PROJECTS",
		})
		if err != nil {
			return err
		}
		node, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "00000000-0000-7000-8000-000000000039", StorageLocationID: sl.ID,
			FilePath: "/tmp/pat_repair/a.jpg", FileName: "a.jpg", FileExt: "jpg", SizeBytes: 1, MtimeUnix: 1,
			IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
			UploadedByUserID: sql.NullInt64{Int64: phantomID, Valid: true},
		})
		if err != nil {
			return err
		}
		nodeID = node.ID
		for _, uid := range []int64{phantomID, unmatchedID, realUserID} {
			if err := q.InsertActorAudit(ctx, sqlcgen.InsertActorAuditParams{
				ActorUserID: sql.NullInt64{Int64: uid, Valid: true}, ActorKind: "user",
				ActorName: fmt.Sprintf("u%d", uid), Event: "test.event", ResourceType: "test", DetailsJson: "{}",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := database.ExecInTx(ctx, migrationUpSQL(t, "00039_repair_pat_phantom_users.sql")); err != nil {
		t.Fatalf("apply 00039 Up: %v", err)
	}

	scalar := func(query string, args ...any) int64 {
		t.Helper()
		var v sql.NullInt64
		if err := database.reader.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v.Int64
	}

	if got := scalar(`SELECT user_id FROM device_pairings WHERE id = ?`, pairingID); got != ownerID {
		t.Errorf("device_pairings.user_id = %d, want owner %d", got, ownerID)
	}
	if got := scalar(`SELECT started_by_user_id FROM scan_jobs WHERE id = ?`, scanID); got != ownerID {
		t.Errorf("scan_jobs.started_by_user_id = %d, want owner %d", got, ownerID)
	}
	if got := scalar(`SELECT uploaded_by_user_id FROM media_nodes WHERE id = ?`, nodeID); got != ownerID {
		t.Errorf("media_nodes.uploaded_by_user_id = %d, want owner %d", got, ownerID)
	}
	if got := scalar(`SELECT count(*) FROM actor_audit WHERE actor_user_id = ?`, phantomID); got != 0 {
		t.Errorf("%d actor_audit rows still point at the phantom", got)
	}
	if got := scalar(`SELECT count(*) FROM actor_audit WHERE actor_user_id = ?`, ownerID); got != 1 {
		t.Errorf("actor_audit rows re-pointed at owner = %d, want 1", got)
	}
	for _, id := range []int64{unmatchedID, realUserID} {
		if got := scalar(`SELECT count(*) FROM actor_audit WHERE actor_user_id = ?`, id); got != 1 {
			t.Errorf("actor_audit row of non-phantom user %d was moved", id)
		}
	}

	// The phantom survives (rows are never deleted), demoted and disabled.
	if got := scalar(`SELECT is_admin FROM users WHERE id = ?`, phantomID); got != 0 {
		t.Errorf("phantom is_admin = %d, want 0", got)
	}
	if got := scalar(`SELECT disabled_at IS NOT NULL FROM users WHERE id = ?`, phantomID); got != 1 {
		t.Error("phantom must be disabled")
	}
	for _, id := range []int64{unmatchedID, realUserID, ownerID} {
		if got := scalar(`SELECT disabled_at IS NULL FROM users WHERE id = ?`, id); got != 1 {
			t.Errorf("non-phantom user %d was disabled", id)
		}
	}

	// Idempotent: a second pass changes nothing and does not fail.
	if _, err := database.ExecInTx(ctx, migrationUpSQL(t, "00039_repair_pat_phantom_users.sql")); err != nil {
		t.Fatalf("re-apply 00039 Up: %v", err)
	}
}
