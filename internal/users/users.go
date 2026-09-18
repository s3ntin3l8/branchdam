// Package users owns the multi-user attribution layer: lazy-provisioning
// of `users` rows keyed on (auth_provider, external_uid), refresh-on-sight
// of denormalized display fields (username/email), and the system user
// used by every background path that has no request principal.
//
// The package sits *between* internal/auth (which produces a Principal)
// and the rest of the server: HTTP handlers, the agent drainer, the
// pairing path, and the scan path all call ResolveOrCreate(ctx, p) to
// turn a request Principal into a stable users.id they can write into
// media_nodes.uploaded_by_user_id, scan_jobs.started_by_user_id, or
// device_pairings.user_id.
//
// Distinct from internal/auth/users, which owns local-auth (argon2id
// passwords, sessions, login audit) and was designed before this PR
// landed. The two packages share the same `users` table -- that's
// PR #407's design -- but each owns its own slice of concerns:
// internal/auth/users owns login/session; internal/users owns
// attribution and the system sentinel.
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// AuthProviderAuthentik is the auth_provider value for users who
// authenticate via Authentik ForwardAuth. The stable per-user id header
// (set by the identity proxy) is carried in the Principal's ExternalUID
// (or the username fallback, see auth.BrowserChain.pickExternalUID).
const AuthProviderAuthentik = "authentik"

// AuthProviderLocal is the auth_provider value for users who authenticate
// via the local session-cookie chain (source='local' rows in the users
// table). These rows already exist when ResolveOrCreate is called, so
// the local branch does a lookup+refresh instead of an INSERT (the
// INSERT path uses source='forward-link' which would violate the CHECK
// constraint on source/password_hash for local rows).
const AuthProviderLocal = "local"

// PrincipalKindUser is the only Principal kind that resolves to a real
// users row via ResolveOrCreate. KindMachine (agent) sessions do not own
// user-attributed rows -- the agent's paired device_pairings row is what
// carries ownership for paired uploads.
const PrincipalKindUser = auth.KindUser

// SystemUserID is the sentinel auth_provider/external_uid used as the
// attribution owner for background work (SweeperSupervisor's INCREMENTAL
// passes, the prune engine, anything that runs without a request
// principal). It's resolved once at boot via EnsureSystemUser and cached
// for the lifetime of the process.
//
// 'system' cannot collide with a real Authentik uid (those are UUIDs).
const (
	systemAuthProvider = "system"
	systemExternalUID  = "system"
)

// ErrInvalidPrincipal is returned by ResolveOrCreate when the Principal is
// not a KindUser, is unauthenticated, or has an empty ExternalUID. Callers
// should treat this as "no attribution will be written" -- background
// paths use SystemUserID for the same situation, but HTTP handlers
// shouldn't silently paper over a misconfigured ForwardAuth.
var ErrInvalidPrincipal = errors.New("users: principal is not a usable attribution subject")

// Attribution is the public shape returned by ResolveOrCreate: just the
// stable users.id, with no DB row leakage. Internal callers that need the
// full row use ResolveOrCreateFull.
type Attribution struct {
	ID           int64
	AuthProvider string
	ExternalUID  string
	Username     string
	Email        sql.NullString
}

// Service is the per-process users attribution service. It holds the db
// handle and the cached system user id; HTTP handlers and background
// workers each get one at construction time.
type Service struct {
	db          *db.DB
	systemUser  Attribution
	systemCache bool // true once EnsureSystemUser has run successfully
	log         *slog.Logger
}

// NewService constructs an attribution service. It does NOT provision the
// system user -- callers that need background attribution must call
// EnsureSystemUser at boot before starting any workers. The service
// starts with a discard logger; wire a real one with WithLogger at boot
// so resolveLocal's reconciliation path has somewhere to surface drift.
func NewService(database *db.DB) *Service {
	return &Service{
		db:  database,
		log: slog.New(slog.DiscardHandler),
	}
}

// WithLogger installs a structured logger on the service. Chainable, so
// cmd/branchdam can do NewService(database).WithLogger(log) at boot.
// A nil logger is ignored (the discard default stays in place) so
// callers don't need to nil-guard.
func (s *Service) WithLogger(log *slog.Logger) *Service {
	if log != nil {
		s.log = log
	}
	return s
}

// EnsureSystemUser lazy-provisions the system sentinel row and caches its
// id. Idempotent: re-running is a single no-op INSERT plus a SELECT, and
// the cached id is updated if a concurrent caller raced ahead. Call this
// once at boot from cmd/branchdam before starting background supervisors.
func (s *Service) EnsureSystemUser(ctx context.Context) (Attribution, error) {
	if s.systemCache {
		return s.systemUser, nil
	}
	var sys Attribution
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.EnsureSystemUser(ctx)
		if err != nil {
			return fmt.Errorf("ensure system user: %w", err)
		}
		row, err := q.GetAttributionUserByID(ctx, id)
		if err != nil {
			return fmt.Errorf("load system user: %w", err)
		}
		sys = Attribution{
			ID:           row.ID,
			AuthProvider: row.AuthProvider,
			ExternalUID:  row.ExternalUid,
			Username:     row.Username,
			Email:        row.Email,
		}
		return nil
	})
	if err != nil {
		return Attribution{}, err
	}
	s.systemUser = sys
	s.systemCache = true
	return sys, nil
}

// SystemUser returns the cached system user Attribution. Panics if
// EnsureSystemUser hasn't run -- the boot sequence guarantees it has, and
// a panic is the right signal for "you started a background worker before
// the system user existed".
func (s *Service) SystemUser() Attribution {
	if !s.systemCache {
		panic("users.SystemUser called before EnsureSystemUser")
	}
	return s.systemUser
}

// SystemUserID returns just the cached system user id. Panics like
// SystemUser if EnsureSystemUser hasn't run.
func (s *Service) SystemUserID() int64 {
	return s.SystemUser().ID
}

// SystemUserSafe is like SystemUser but returns (Attribution, error)
// instead of panicking. Used by the audit log when writing background
// events from inside a request handler that may have arrived before
// EnsureSystemUser ran (test setups, very early boot paths).
func (s *Service) SystemUserSafe() (Attribution, error) {
	if !s.systemCache {
		return Attribution{}, fmt.Errorf("users: system user not yet provisioned")
	}
	return s.systemUser, nil
}

// ResolveOrCreate turns a request Principal into a stable users.id.
//
// For a KindUser Principal with a non-empty ExternalUID (the normal
// browser path through BrowserChain), this:
//
//  1. INSERTs ON CONFLICT DO NOTHING on (auth_provider, external_uid),
//     returning the row id either way.
//  2. UPDATEs the denormalized username/email and bumps last_seen_at so
//     a username rename or email change shows up on the next request
//     without a separate background refresh job.
//
// For local-session Principals (AuthProvider == "local"), the user row
// already exists (source='local', password_hash set) so this skips the
// INSERT and does a lookup+refresh instead -- the INSERT path uses
// source='forward-link' with NULL password_hash, which would violate the
// CHECK constraint on local rows.
//
// The two writes happen in the same write transaction (single-connection
// writer pool, AGENTS.md invariant #2), so a slow scan/insert doesn't
// observe a half-updated attribution row. The principal lookup and the
// row write are sequenced inside InTx.
//
// Returns ErrInvalidPrincipal for KindMachine, Authenticated=false, or
// empty ExternalUID. The latter two are configuration problems (missing
// ForwardAuth, old Authentik without the stable uid header AND no
// username header to fall back to) and should be surfaced, not
// silently rewritten to the system user.
func (s *Service) ResolveOrCreate(ctx context.Context, p auth.Principal) (Attribution, error) {
	if p.Kind != PrincipalKindUser {
		return Attribution{}, ErrInvalidPrincipal
	}
	if !p.Authenticated {
		return Attribution{}, ErrInvalidPrincipal
	}
	if p.ExternalUID == "" {
		return Attribution{}, ErrInvalidPrincipal
	}

	// Local-session branch: the row already exists (source='local'),
	// so do a lookup+refresh instead of INSERT. The INSERT path
	// hardcodes source='forward-link' + password_hash=NULL, which
	// violates the CHECK constraint for source='local' rows.
	provider := p.AuthProvider
	if provider == "" {
		provider = AuthProviderAuthentik
	}
	if provider == AuthProviderLocal {
		return s.resolveLocal(ctx, p)
	}

	var out Attribution
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: AuthProviderAuthentik,
			ExternalUid:  p.ExternalUID,
			Username:     p.Name,
			Email:        sql.NullString{String: p.Email, Valid: p.Email != ""},
		})
		if err != nil {
			return fmt.Errorf("create attribution user: %w", err)
		}
		if err := q.RefreshAttributionUserSeen(ctx, sqlcgen.RefreshAttributionUserSeenParams{
			ID:       id,
			Username: p.Name,
			Email:    sql.NullString{String: p.Email, Valid: p.Email != ""},
		}); err != nil {
			return fmt.Errorf("refresh attribution user: %w", err)
		}
		row, err := q.GetAttributionUserByID(ctx, id)
		if err != nil {
			return fmt.Errorf("load attribution user: %w", err)
		}
		out = Attribution{
			ID:           row.ID,
			AuthProvider: row.AuthProvider,
			ExternalUID:  row.ExternalUid,
			Username:     row.Username,
			Email:        row.Email,
		}
		return nil
	})
	if err != nil {
		return Attribution{}, err
	}
	return out, nil
}

// resolveLocal handles the auth_provider="local" case of ResolveOrCreate.
// Local users already exist in the users table (source='local',
// password_hash set) so we only need a lookup+refresh -- no INSERT.
//
// Drift reconciliation: the (auth_provider, external_uid) lookup uses
// p.ExternalUID, which session/middleware sets to the current
// username. If a username rename ever goes through without a parallel
// external_uid update (admin tool, direct SQL, a yet-to-land user-
// edit endpoint), the (local, p.ExternalUID) lookup would miss and
// attribution for that user would silently NULL on every subsequent
// request. To self-heal, a miss falls through to a username-based
// lookup; if that finds a source='local' row with a mismatched
// external_uid, we UPDATE external_uid to match p.ExternalUID (== p.Name
// for a local principal) inside the same write transaction and log the
// drift at INFO. The follow-up GetAttributionUserByExternalUID then
// hits, and ResolveOrCreate returns a usable Attribution. If the
// username lookup also misses, or finds a non-'local' row, we return
// the original sql.ErrNoRows wrapped (callers surface the warn as
// before).
func (s *Service) resolveLocal(ctx context.Context, p auth.Principal) (Attribution, error) {
	var out Attribution
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
			AuthProvider: AuthProviderLocal,
			ExternalUid:  p.ExternalUID,
		})
		if errors.Is(err, sql.ErrNoRows) {
			reconciledID, reconErr := s.reconcileLocalDrift(ctx, q, p)
			if reconErr != nil {
				return fmt.Errorf("resolve local user: %w", err)
			}
			if reconciledID == 0 {
				return fmt.Errorf("resolve local user: %w", err)
			}
			row, err = q.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
				AuthProvider: AuthProviderLocal,
				ExternalUid:  p.ExternalUID,
			})
			if err != nil {
				return fmt.Errorf("load local attribution user after reconciliation: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("resolve local user: %w", err)
		}
		if err := q.RefreshAttributionUserSeen(ctx, sqlcgen.RefreshAttributionUserSeenParams{
			ID:       row.ID,
			Username: p.Name,
			Email:    sql.NullString{String: p.Email, Valid: p.Email != ""},
		}); err != nil {
			return fmt.Errorf("refresh local attribution user: %w", err)
		}
		refreshed, err := q.GetAttributionUserByID(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("load local attribution user: %w", err)
		}
		out = Attribution{
			ID:           refreshed.ID,
			AuthProvider: refreshed.AuthProvider,
			ExternalUID:  refreshed.ExternalUid,
			Username:     refreshed.Username,
			Email:        refreshed.Email,
		}
		return nil
	})
	if err != nil {
		return Attribution{}, err
	}
	return out, nil
}

// reconcileLocalDrift is the self-heal path for a (local, external_uid)
// miss in resolveLocal: look up the user by username (p.Name ==
// p.ExternalUID for local-session Principals), confirm the row is
// source='local' with a mismatched external_uid, and UPDATE the column
// inside the caller's transaction. Returns the row's id on success,
// 0 when no row qualifies (caller falls back to the original
// sql.ErrNoRows), or a non-nil error. The mismatch-already-correct
// case returns 0 so the caller doesn't mask the original ErrNoRows
// with a logged "reconciliation succeeded" entry that the next
// request would re-trigger.
func (s *Service) reconcileLocalDrift(ctx context.Context, q *sqlcgen.Queries, p auth.Principal) (int64, error) {
	byName, err := q.GetUserByUsername(ctx, p.Name)
	if err != nil {
		return 0, nil
	}
	if byName.Source != AuthProviderLocal {
		return 0, nil
	}
	if byName.ExternalUid == p.ExternalUID {
		return 0, nil
	}
	if err := q.ReconcileLocalExternalUID(ctx, sqlcgen.ReconcileLocalExternalUIDParams{
		ID:          byName.ID,
		ExternalUid: p.ExternalUID,
	}); err != nil {
		return 0, err
	}
	s.log.Info("users: reconciled drifted local external_uid",
		"user_id", byName.ID,
		"username", p.Name,
		"old_external_uid", byName.ExternalUid,
		"new_external_uid", p.ExternalUID,
	)
	return byName.ID, nil
}
