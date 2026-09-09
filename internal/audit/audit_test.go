package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/users"
)

func newAuditService(t *testing.T) (*Service, *users.Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	users := users.NewService(database)
	if _, err := users.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	return NewService(database, users), users
}

func TestWriteActorAudit_UserPrincipalResolves(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{
		Kind: auth.KindUser, Name: "alice", Email: "alice@example.com",
		ExternalUID: "alice-uid-stable", Authenticated: true,
	}
	if err := svc.WriteActorAudit(ctx, p, EventSettingsUpdated, "app_setting", "media.thumbnail_size", map[string]string{"old": "256", "new": "512"}); err != nil {
		t.Fatalf("WriteActorAudit: %v", err)
	}
	entries, total, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.Event != EventSettingsUpdated {
		t.Errorf("event = %q, want %q", got.Event, EventSettingsUpdated)
	}
	if got.ActorKind != ActorKindUser {
		t.Errorf("actor_kind = %q, want %q", got.ActorKind, ActorKindUser)
	}
	if got.ActorName != "alice" {
		t.Errorf("actor_name = %q, want %q", got.ActorName, "alice")
	}
	if !got.ActorUserID.Valid {
		t.Errorf("actor_user_id not set for an authenticated user")
	}
	if got.ResourceType != "app_setting" {
		t.Errorf("resource_type = %q, want %q", got.ResourceType, "app_setting")
	}
	if got.ResourceID.String != "media.thumbnail_size" {
		t.Errorf("resource_id = %q, want %q", got.ResourceID.String, "media.thumbnail_size")
	}
	var details map[string]string
	if err := json.Unmarshal([]byte(got.DetailsJSON), &details); err != nil {
		t.Fatalf("details json: %v", err)
	}
	if details["old"] != "256" || details["new"] != "512" {
		t.Errorf("details = %v, want old=256 new=512", details)
	}
}

func TestWriteActorAudit_SystemActor(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	if err := svc.WriteActorAudit(ctx, SystemActor, EventScanStarted, "scan_job", "42", nil); err != nil {
		t.Fatalf("WriteActorAudit: %v", err)
	}
	entries, _, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].ActorKind != ActorKindSystem {
		t.Errorf("actor_kind = %q, want %q", entries[0].ActorKind, ActorKindSystem)
	}
	if !entries[0].ActorUserID.Valid {
		t.Errorf("system actor must have actor_user_id set to the system user id")
	}
}

func TestWriteActorAudit_MachinePrincipal(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindMachine, Name: "agent-1", Authenticated: true}
	if err := svc.WriteActorAudit(ctx, p, EventScanStarted, "scan_job", "1", nil); err != nil {
		t.Fatalf("WriteActorAudit: %v", err)
	}
	entries, _, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if entries[0].ActorKind != ActorKindMachine {
		t.Errorf("actor_kind = %q, want %q", entries[0].ActorKind, ActorKindMachine)
	}
	if entries[0].ActorName != "agent-1" {
		t.Errorf("actor_name = %q, want %q", entries[0].ActorName, "agent-1")
	}
}

func TestWriteActorAudit_UnauthenticatedBecomesAnonymous(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, Name: "alice", Authenticated: false}
	if err := svc.WriteActorAudit(ctx, p, EventRestart, "system", "", nil); err != nil {
		t.Fatalf("WriteActorAudit: %v", err)
	}
	entries, _, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if entries[0].ActorKind != ActorKindAnonymous {
		t.Errorf("actor_kind = %q, want %q", entries[0].ActorKind, ActorKindAnonymous)
	}
}

func TestWriteActorAudit_NilDetailsJSONIsEmptyObject(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true}
	if err := svc.WriteActorAudit(ctx, p, EventRestart, "system", "", nil); err != nil {
		t.Fatalf("WriteActorAudit: %v", err)
	}
	entries, _, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if entries[0].DetailsJSON != "{}" {
		t.Errorf("details_json = %q, want %q", entries[0].DetailsJSON, "{}")
	}
}

func TestListActivity_FilterByActorUser(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p1 := auth.Principal{Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true}
	p2 := auth.Principal{Kind: auth.KindUser, Name: "bob", ExternalUID: "bob-uid", Authenticated: true}
	if err := svc.WriteActorAudit(ctx, p1, EventSettingsUpdated, "app_setting", "a", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteActorAudit(ctx, p2, EventRestart, "system", "", nil); err != nil {
		t.Fatal(err)
	}
	entries, total, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}

	var aliceID int64
	for _, e := range entries {
		if e.ActorName == "alice" {
			aliceID = e.ActorUserID.Int64
		}
	}
	filtered, filteredTotal, err := svc.ListActivity(ctx, Filter{ActorUserID: sql.NullInt64{Int64: aliceID, Valid: true}}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if filteredTotal != 1 {
		t.Errorf("filtered total = %d, want 1", filteredTotal)
	}
	if len(filtered) != 1 || filtered[0].ActorName != "alice" {
		t.Errorf("filtered = %v, want only alice", filtered)
	}
}

func TestListActivity_NewestFirst(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true}
	for i := 0; i < 3; i++ {
		if err := svc.WriteActorAudit(ctx, p, EventSettingsUpdated, "k", "v", nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := svc.ListActivity(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].CreatedAt < entries[i].CreatedAt {
			t.Errorf("entries not newest-first at %d: %d < %d", i, entries[i-1].CreatedAt, entries[i].CreatedAt)
		}
	}
}

// TestListActivity_FilterByEvent: ListActivity's event filter clause
// narrows the actor_audit query. The route handler maps the same
// parameter through, but pinning the SQL clause here catches a
// regression where the WHERE chain drifts between the two read paths.
func TestListActivity_FilterByEvent(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true}
	if err := svc.WriteActorAudit(ctx, p, EventSettingsUpdated, "app_setting", "k1", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteActorAudit(ctx, p, EventRestart, "system", "", nil); err != nil {
		t.Fatal(err)
	}
	rows, total, err := svc.ListActivity(ctx, Filter{Event: EventSettingsUpdated}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("event-filtered total = %d, want 1", total)
	}
	if len(rows) != 1 || rows[0].Event != EventSettingsUpdated {
		t.Errorf("rows = %+v, want one settings.updated row", rows)
	}
}

// TestListActivity_FilterByResourceID: ListActivity's resource_id
// filter clause narrows results. Exercises the LIKE/= comparison
// for the narg-shaped column.
func TestListActivity_FilterByResourceID(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true}
	if err := svc.WriteActorAudit(ctx, p, EventSettingsUpdated, "app_setting", "alpha", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteActorAudit(ctx, p, EventSettingsUpdated, "app_setting", "beta", nil); err != nil {
		t.Fatal(err)
	}
	rows, total, err := svc.ListActivity(ctx, Filter{ResourceID: "alpha"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("resource_id-filtered total = %d, want 1", total)
	}
	if len(rows) != 1 || !rows[0].ResourceID.Valid || rows[0].ResourceID.String != "alpha" {
		t.Errorf("rows = %+v", rows)
	}
}

// TestListActivity_FilterBySinceUntilUnix: the since/until clauses
// bound the result set by created_at. With since=tomorrow (a future
// second), zero rows come back; with since=0, all rows do.
func TestListActivity_FilterBySinceUntilUnix(t *testing.T) {
	svc, _ := newAuditService(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true}
	if err := svc.WriteActorAudit(ctx, p, EventSettingsUpdated, "app_setting", "k1", nil); err != nil {
		t.Fatal(err)
	}

	// since=0 returns the seeded row.
	rows, total, err := svc.ListActivity(ctx, Filter{
		SinceUnix: sql.NullInt64{Int64: 0, Valid: true},
	}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("since=0 total = %d, want 1", total)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1", len(rows))
	}

	// until=now+1h returns the seeded row (created_at < now+1h).
	_, total, err = svc.ListActivity(ctx, Filter{
		UntilUnix: sql.NullInt64{Int64: time.Now().Unix() + 3600, Valid: true},
	}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("until=now+1h total = %d, want 1", total)
	}

	// since=now+1h (future) returns 0 rows -- the seeded row's
	// created_at is in the past.
	rows, _, err = svc.ListActivity(ctx, Filter{
		SinceUnix: sql.NullInt64{Int64: time.Now().Unix() + 3600, Valid: true},
	}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// TestMarshalDetails_EmptyJSONObject: json.Marshal of a struct that
// marshals to "{}" (empty struct value) returns "{}" not "null", so
// the marshalDetails path falls through the empty-bytes branch.
func TestMarshalDetails_EmptyJSONObject(t *testing.T) {
	// An empty struct marshals to "{}" (2 bytes), not the zero/null
	// case the function guards against.
	if got, err := marshalDetails(struct{}{}); err != nil {
		t.Fatalf("marshalDetails: %v", err)
	} else if got != "{}" {
		t.Errorf("marshalDetails(struct{}{}) = %q, want \"{}\"", got)
	}
}

// TestEmptyToNil_BothBranches: emptyToNil returns nil for the empty
// case and the original value otherwise. Cover both branches.
func TestEmptyToNil_BothBranches(t *testing.T) {
	if got := emptyToNil(""); got != nil {
		t.Errorf("emptyToNil(\"\") = %v, want nil", got)
	}
	if got := emptyToNil("hello"); got != "hello" {
		t.Errorf("emptyToNil(\"hello\") = %v, want hello", got)
	}
}

// TestNullableToInterface_BothBranches: nullableToInterface returns
// nil when the value is invalid and the int64 when valid.
func TestNullableToInterface_BothBranches(t *testing.T) {
	if got := nullableToInterface(sql.NullInt64{}); got != nil {
		t.Errorf("nullableToInterface(invalid) = %v, want nil", got)
	}
	if got := nullableToInterface(sql.NullInt64{Int64: 42, Valid: true}); got != int64(42) {
		t.Errorf("nullableToInterface(42) = %v, want 42", got)
	}
}

// TestResolveActor_SystemWithUserSvcButNotProvisioned: when the
// audit Service is built but EnsureSystemUser hasn't been called
// yet (the audit writer is constructed before the boot sequence
// finishes), resolveActor must fall through to a kind-only row
// rather than panicking on a nil userSvc lookup.
func TestResolveActor_SystemBeforeEnsure(t *testing.T) {
	// Build the users service without calling EnsureSystemUser.
	path := filepath.Join(t.TempDir(), "no-ensure.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	users := users.NewService(database)
	_ = NewService(database, users)

	_, gotKind, _ := resolveActor(context.Background(), auth.Principal{Kind: auth.KindSystem, Name: "system", ExternalUID: "system", Authenticated: true}, users)
	if gotKind != "system" {
		t.Errorf("system-before-ensure actorKind = %q, want system", gotKind)
	}
}
