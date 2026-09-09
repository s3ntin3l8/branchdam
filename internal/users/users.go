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

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// AuthProviderAuthentik is the only auth_provider this server understands
// in this PR. Authentik-ForwardAuth sets the stable per-user id header,
// which the Principal's ExternalUID carries (or the username fallback, see
// auth.BrowserChain.pickExternalUID), and we lazily insert a row at first
// sight.
const AuthProviderAuthentik = "authentik"

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
}

// NewService constructs an attribution service. It does NOT provision the
// system user -- callers that need background attribution must call
// EnsureSystemUser at boot before starting any workers.
func NewService(database *db.DB) *Service {
	return &Service{db: database}
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
