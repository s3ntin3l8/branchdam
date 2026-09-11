// Package audit owns the cross-cutting actor_audit log writes and reads
// for admin actions that aren't otherwise audited (scan start, settings
// PUT, storage location PUT, prune execute, restart, pairing lifecycle
// events that aren't already in companion_pairing_audit, ...).
//
// Distinct from internal/auth/users.WriteLoginAudit (PR #407's
// login_audit table), which is a domain-specific log keyed on the
// authentication surface (login success/failure, password reset).
// actor_audit is a cross-cutting event log keyed on whoever-did-what
// across any HTTP route or background worker; login_audit stays where
// it is so the existing login audit views keep working.
//
// The HTTP layer reads both tables through one merged
// GET /api/v1/audit?type=activity|login route; this package owns the
// activity (actor_audit) side.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/users"
)

// ActorKind is the discriminator for actor_audit.actor_kind. CHECK'd at
// the schema layer; values outside this set are rejected by SQLite.
const (
	ActorKindUser      = "user"
	ActorKindMachine   = "machine"
	ActorKindSystem    = "system"
	ActorKindAnonymous = "anonymous"
)

// Well-known event strings. Not enforced by the schema -- any string is
// allowed -- but consolidating the canonical names here keeps the audit
// views filterable without stringly-typed drift across handlers.
const (
	EventScanStarted        = "scan.started"
	EventScanFinished       = "scan.finished"
	EventRestart            = "restart.executed"
	EventSettingsUpdated    = "settings.updated"
	EventStorageLocationPut = "storage_location.upserted"
	EventStorageLocationDel = "storage_location.deleted"
	EventPruneExecuted      = "prune.executed"
	EventPairingCreated     = "pairing.created"
	EventPairingRotated     = "pairing.rotated"
	EventPairingRevoked     = "pairing.revoked"
	EventPairingDeleted     = "pairing.deleted"
	EventActorAuditExported = "actor_audit.exported"
	EventAssetArchived      = "asset.archived"
	EventAssetRestored      = "asset.restored"
	EventUserCreated        = "user.created"
	EventUserDisabled       = "user.disabled"
)

// Event is a typed event name. Anything that wants to write a new kind
// of event should add a constant here so the audit views stay
// filterable. Free-form event names are still allowed via
// Service.WriteActorAudit(..., event string, ...) for ad-hoc admin
// tools that legitimately need a one-off label, but the canonical
// names are the recommended path.
type Event struct {
	Name string
}

// Service is the per-process audit writer/reader. Holds the db handle
// and a cached reference to the users service for resolving Principal
// -> Attribution so each Write call doesn't have to plumb a user_id
// through the call site.
type Service struct {
	db    *db.DB
	users *users.Service
}

// NewService constructs an audit service. users may be nil for tests
// that don't exercise attribution; production code (cmd/branchdam)
// always wires it.
func NewService(database *db.DB, userSvc *users.Service) *Service {
	return &Service{db: database, users: userSvc}
}

// Entry is one row in actor_audit, in the shape the merged audit read
// route renders. Times are unix epoch seconds (matching the schema).
type Entry struct {
	ID           int64
	ActorUserID  sql.NullInt64
	ActorKind    string
	ActorName    string
	Event        string
	ResourceType string
	ResourceID   sql.NullString
	DetailsJSON  string
	CreatedAt    int64
	CreatedAtRFC time.Time
}

// Filter is the input to ListActivity. Zero values mean "no filter on
// this column". SinceUnix / UntilUnix are inclusive lower / exclusive
// upper bounds (unix epoch seconds), matching the query.
type Filter struct {
	ActorUserID  sql.NullInt64
	ActorKind    string
	Event        string
	ResourceType string
	ResourceID   string
	SinceUnix    sql.NullInt64
	UntilUnix    sql.NullInt64
}

// WriteActorAudit logs a single actor_audit row. resourceID and
// details may be empty; details is serialized as JSON, with a "null"
// rendered as "{}" if the caller passes nil so the column's NOT NULL
// constraint is always satisfied.
//
// The actor_user_id + actor_kind + actor_name triple is derived from
// the Principal: an authenticated KindUser becomes ("user", name, uid);
// a KindMachine becomes ("machine", agent_id, ""); an
// unauthenticated request (e.g. the public healthz probe, a future
// anonymous public route) becomes ("anonymous", "", ""). The system
// user sentinel is "system" + ("system", "system", id). Callers that
// want to log a background event without a Principal should pass
// SystemActor instead of a Principal.
func (s *Service) WriteActorAudit(ctx context.Context, p auth.Principal, event, resourceType, resourceID string, details any) error {
	userID, kind, name := resolveActor(ctx, p, s.users)
	detailsJSON, err := marshalDetails(details)
	if err != nil {
		return fmt.Errorf("audit: marshal details: %w", err)
	}
	return s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var userIDArg sql.NullInt64
		if userID != 0 {
			userIDArg = sql.NullInt64{Int64: userID, Valid: true}
		}
		var ridArg sql.NullString
		if resourceID != "" {
			ridArg = sql.NullString{String: resourceID, Valid: true}
		}
		return q.InsertActorAudit(ctx, sqlcgen.InsertActorAuditParams{
			ActorUserID:  userIDArg,
			ActorKind:    kind,
			ActorName:    name,
			Event:        event,
			ResourceType: resourceType,
			ResourceID:   ridArg,
			DetailsJson:  detailsJSON,
		})
	})
}

// SystemActor is the Principal shape background workers should pass
// when calling WriteActorAudit: KindSystem, Authenticated=false,
// ExternalUID="system". resolveActor maps it to the cached system
// user Attribution so the row's actor_user_id matches the system
// sentinel id, and actor_kind/name carry "system"/"system" for
// display.
//
// For background workers that don't go through auth.Principal at all,
// pass this constant directly.
var SystemActor = auth.Principal{
	Kind:          auth.KindSystem,
	Name:          "system",
	ExternalUID:   "system",
	Authenticated: true,
}

// resolveActor returns (user_id, kind, name) for the Principal.
// Handles the special SystemActor mapping (above) and falls back to
// anonymous for empty Authenticated User principals.
func resolveActor(ctx context.Context, p auth.Principal, userSvc *users.Service) (int64, string, string) {
	switch {
	case p.Kind == auth.KindSystem:
		if userSvc != nil {
			// SystemActor only resolves correctly once EnsureSystemUser
			// has run; the boot sequence guarantees it before any
			// background worker starts. If it hasn't run yet (e.g. a
			// test), fall through to a kind-only log with id=0 so the
			// schema CHECK is still satisfied.
			if sys, err := userSvc.SystemUserSafe(); err == nil {
				return sys.ID, ActorKindSystem, sys.Username
			}
		}
		return 0, ActorKindSystem, "system"
	case p.Kind == auth.KindMachine:
		return 0, ActorKindMachine, p.Name
	case p.Kind == auth.KindUser && p.Authenticated && p.ExternalUID != "":
		if userSvc != nil {
			a, err := userSvc.ResolveOrCreate(ctx, p)
			if err == nil {
				return a.ID, ActorKindUser, p.Name
			}
		}
		return 0, ActorKindUser, p.Name
	}
	return 0, ActorKindAnonymous, ""
}

// marshalDetails serializes details as JSON. nil becomes "{}" so the
// column's NOT NULL DEFAULT '{}' constraint is always satisfied and
// downstream readers never have to handle null strings.
func marshalDetails(details any) (string, error) {
	if details == nil {
		return "{}", nil
	}
	b, err := json.Marshal(details)
	if err != nil {
		return "", err
	}
	if len(b) == 0 || string(b) == "null" {
		return "{}", nil
	}
	return string(b), nil
}

// ListActivity returns actor_audit rows matching filter, newest first.
// Pagination is offset-based; the route caps limit at 200 rows.
func (s *Service) ListActivity(ctx context.Context, f Filter, limit, offset int64) ([]Entry, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	var entries []Entry
	var total int64
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		params := sqlcgen.ListActorAuditParams{
			ActorUserID:  nullableToInterface(f.ActorUserID),
			ActorKind:    emptyToNil(f.ActorKind),
			Event:        emptyToNil(f.Event),
			ResourceType: emptyToNil(f.ResourceType),
			ResourceID:   emptyToNil(f.ResourceID),
			SinceUnix:    nullableToInterface(f.SinceUnix),
			UntilUnix:    nullableToInterface(f.UntilUnix),
			Limit:        limit,
			Offset:       offset,
		}
		rows, err := q.ListActorAudit(ctx, params)
		if err != nil {
			return fmt.Errorf("list actor_audit: %w", err)
		}
		for _, r := range rows {
			entries = append(entries, Entry{
				ID:           r.ID,
				ActorUserID:  r.ActorUserID,
				ActorKind:    r.ActorKind,
				ActorName:    r.ActorName,
				Event:        r.Event,
				ResourceType: r.ResourceType,
				ResourceID:   r.ResourceID,
				DetailsJSON:  r.DetailsJson,
				CreatedAt:    r.CreatedAt,
				CreatedAtRFC: time.Unix(r.CreatedAt, 0).UTC(),
			})
		}
		countParams := sqlcgen.CountActorAuditParams{
			ActorUserID:  params.ActorUserID,
			ActorKind:    params.ActorKind,
			Event:        params.Event,
			ResourceType: params.ResourceType,
			ResourceID:   params.ResourceID,
			SinceUnix:    params.SinceUnix,
			UntilUnix:    params.UntilUnix,
		}
		c, err := q.CountActorAudit(ctx, countParams)
		if err != nil {
			return fmt.Errorf("count actor_audit: %w", err)
		}
		total = c
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// ErrInvalidPrincipal is returned by WriteActorAudit when the Principal
// is so malformed that no audit row should be written. Today nothing
// in WriteActorAudit returns it (any Principal shape produces a
// valid row); it's exported so callers that wrap WriteActorAudit can
// distinguish "audit write was skipped" from "audit write failed".
var ErrInvalidPrincipal = errors.New("audit: principal shape invalid")

func emptyToNil(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullableToInterface(n sql.NullInt64) interface{} {
	if !n.Valid {
		return nil
	}
	return n.Int64
}
