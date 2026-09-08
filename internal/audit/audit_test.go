package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

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
