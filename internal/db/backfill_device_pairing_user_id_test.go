package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// backfillDevicePairingUserIDSQL is hand-duplicated from
// internal/db/migrations/00027_backfill_device_pairing_user_id.sql, matching
// the pattern in remote_sync_state_index_test.go: the migration only ever
// runs once, against whatever data existed at goose-apply time (empty, for a
// freshly created test DB), so exercising its actual data-transformation
// logic means replaying the same statement against seeded rows after the
// schema exists. If the migration's UPDATE changes, update it here too.
const backfillDevicePairingUserIDSQL = `
UPDATE device_pairings
SET user_id = (
    SELECT u.id FROM users u
    WHERE 'user:' || u.username = (
        SELECT a.actor FROM companion_pairing_audit a
        WHERE a.pairing_id = device_pairings.id
          AND a.event = 'PAIR_CREATED'
        ORDER BY a.created_at ASC LIMIT 1
    )
)
WHERE user_id IS NULL
  AND EXISTS (
    SELECT 1 FROM users u
    WHERE 'user:' || u.username = (
        SELECT a.actor FROM companion_pairing_audit a
        WHERE a.pairing_id = device_pairings.id
          AND a.event = 'PAIR_CREATED'
        ORDER BY a.created_at ASC LIMIT 1
    )
  )
`

// TestBackfillDevicePairingUserID backs the 00020_users_and_audit.sql
// follow-up promised at lines 20-24 and finally shipped in migration 00027:
// pairings created before multi-user attribution existed have user_id NULL,
// and the intended recovery path is resolving companion_pairing_audit's
// PAIR_CREATED actor against users.username.
//
// Actor values below use the real production shape written by actorFromCtx
// (internal/httpapi/companion_pairings.go): "user:" + p.Name for a KindUser
// principal, bare agent_id for a KindMachine principal. An earlier version
// of this test seeded bare usernames as the actor, which happened to match
// the (buggy) migration under test and masked the mismatch -- Hermes review
// caught that the migration's join would never match in production. Case 4
// below exists specifically to keep that regression caught: a bare-agent_id
// actor must never match a users.username of the same string.
func TestBackfillDevicePairingUserID(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	var (
		matchedUserID       int64
		alreadySetUserID    int64
		pairingWithMatch    int64
		pairingNoMatch      int64
		pairingAlreadySet   int64
		pairingMachineActor int64
	)

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		matchedUserID, err = q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik",
			ExternalUid:  "uid-matched",
			Username:     "alice",
			Email:        sql.NullString{String: "alice@example.com", Valid: true},
		})
		if err != nil {
			return err
		}
		alreadySetUserID, err = q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik",
			ExternalUid:  "uid-already-set",
			Username:     "bob",
			Email:        sql.NullString{String: "bob@example.com", Valid: true},
		})
		if err != nil {
			return err
		}

		// Case 1: pairing has NULL user_id; audit actor "user:alice" (the
		// real actorFromCtx shape for a KindUser principal) matches an
		// existing users.username="alice" -> expect backfill to matchedUserID.
		pairing1, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID:       "agent-match",
			FriendlyLabel: "Match Phone",
			CreatedAt:     1000,
			CreatedBy:     "user:alice",
			QrSvg:         []byte("<svg/>"),
			UserID:        sql.NullInt64{},
		})
		if err != nil {
			return err
		}
		pairingWithMatch = pairing1.ID
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingWithMatch,
			Actor:     "user:alice",
			Event:     "PAIR_CREATED",
			Details:   "{}",
			CreatedAt: 1000,
		}); err != nil {
			return err
		}

		// Case 2: pairing has NULL user_id; audit actor "user:ghost" matches
		// no users.username -> expect the row to stay NULL, not be clobbered.
		pairing2, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID:       "agent-no-match",
			FriendlyLabel: "Orphan Phone",
			CreatedAt:     2000,
			CreatedBy:     "user:ghost",
			QrSvg:         []byte("<svg/>"),
			UserID:        sql.NullInt64{},
		})
		if err != nil {
			return err
		}
		pairingNoMatch = pairing2.ID
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingNoMatch,
			Actor:     "user:ghost",
			Event:     "PAIR_CREATED",
			Details:   "{}",
			CreatedAt: 2000,
		}); err != nil {
			return err
		}

		// Case 3: pairing already has a non-NULL user_id (e.g. created after
		// migration 00020 shipped) -> the migration must leave it untouched
		// even if the audit actor would resolve to a different user.
		pairing3, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID:       "agent-already-set",
			FriendlyLabel: "Already Attributed Phone",
			CreatedAt:     3000,
			CreatedBy:     "user:bob",
			QrSvg:         []byte("<svg/>"),
			UserID:        sql.NullInt64{Int64: alreadySetUserID, Valid: true},
		})
		if err != nil {
			return err
		}
		pairingAlreadySet = pairing3.ID
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingAlreadySet,
			Actor:     "user:alice",
			Event:     "PAIR_CREATED",
			Details:   "{}",
			CreatedAt: 3000,
		}); err != nil {
			return err
		}

		// Case 4: pairing has NULL user_id; audit actor is a bare agent_id
		// ("alice", no "user:" prefix) -- the shape actorFromCtx writes for a
		// KindMachine principal. Even though a users row with username
		// "alice" exists (from case 1), this must NOT match: a machine
		// principal creating a pairing is not the same thing as the human
		// user "alice", and the whole point of the "user:" prefix check is
		// to not conflate the two string spaces.
		pairing4, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID:       "agent-machine-actor",
			FriendlyLabel: "Machine-Paired Phone",
			CreatedAt:     4000,
			CreatedBy:     "alice",
			QrSvg:         []byte("<svg/>"),
			UserID:        sql.NullInt64{},
		})
		if err != nil {
			return err
		}
		pairingMachineActor = pairing4.ID
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingMachineActor,
			Actor:     "alice",
			Event:     "PAIR_CREATED",
			Details:   "{}",
			CreatedAt: 4000,
		}); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := database.writer.ExecContext(ctx, backfillDevicePairingUserIDSQL); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	cases := []struct {
		name       string
		pairingID  int64
		wantValid  bool
		wantUserID int64
	}{
		{"matched actor backfills owner", pairingWithMatch, true, matchedUserID},
		{"unmatched actor stays NULL", pairingNoMatch, false, 0},
		{"already-set owner is untouched", pairingAlreadySet, true, alreadySetUserID},
		{"bare-agent_id actor never matches a same-named user", pairingMachineActor, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var userID sql.NullInt64
			err := database.reader.QueryRowContext(ctx,
				"SELECT user_id FROM device_pairings WHERE id = ?", tc.pairingID,
			).Scan(&userID)
			if err != nil {
				t.Fatalf("query device_pairings: %v", err)
			}
			if userID.Valid != tc.wantValid {
				t.Fatalf("user_id.Valid = %v, want %v (userID=%+v)", userID.Valid, tc.wantValid, userID)
			}
			if tc.wantValid && userID.Int64 != tc.wantUserID {
				t.Errorf("user_id = %d, want %d", userID.Int64, tc.wantUserID)
			}
		})
	}
}
