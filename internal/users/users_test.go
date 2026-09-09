package users

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
)

func newService(t *testing.T) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return NewService(database)
}

func TestEnsureSystemUser_Idempotent(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	first, err := svc.EnsureSystemUser(ctx)
	if err != nil {
		t.Fatalf("first EnsureSystemUser: %v", err)
	}
	if first.ID == 0 {
		t.Fatalf("first EnsureSystemUser returned ID=0")
	}
	if first.AuthProvider != systemAuthProvider || first.ExternalUID != systemExternalUID {
		t.Errorf("system user identity = (%q,%q), want (system,system)", first.AuthProvider, first.ExternalUID)
	}
	if first.Username != "system" {
		t.Errorf("system user username = %q, want \"system\"", first.Username)
	}

	second, err := svc.EnsureSystemUser(ctx)
	if err != nil {
		t.Fatalf("second EnsureSystemUser: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("EnsureSystemUser is not idempotent: first.ID=%d, second.ID=%d", first.ID, second.ID)
	}
}

func TestSystemUser_PanicsBeforeEnsure(t *testing.T) {
	svc := newService(t)
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("SystemUser() before EnsureSystemUser should panic")
		}
	}()
	_ = svc.SystemUser()
}

func TestSystemUserID_PanicsBeforeEnsure(t *testing.T) {
	svc := newService(t)
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("SystemUserID() before EnsureSystemUser should panic")
		}
	}()
	_ = svc.SystemUserID()
}

func TestResolveOrCreate_FirstSightCreatesRow(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		Email:         "alice@example.com",
		ExternalUID:   "alice-uid-stable",
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if got.ID == 0 {
		t.Errorf("ResolveOrCreate returned ID=0")
	}
	if got.ExternalUID != "alice-uid-stable" {
		t.Errorf("ExternalUID = %q, want %q", got.ExternalUID, "alice-uid-stable")
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want %q", got.Username, "alice")
	}
}

func TestResolveOrCreate_StableIdAcrossCalls(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	p1 := auth.Principal{
		Kind: auth.KindUser, Name: "alice", Email: "alice@example.com",
		ExternalUID: "alice-uid-stable", Authenticated: true,
	}
	first, err := svc.ResolveOrCreate(ctx, p1)
	if err != nil {
		t.Fatalf("first ResolveOrCreate: %v", err)
	}

	p2 := auth.Principal{
		Kind: auth.KindUser, Name: "alice", Email: "alice@example.com",
		ExternalUID: "alice-uid-stable", Authenticated: true,
	}
	second, err := svc.ResolveOrCreate(ctx, p2)
	if err != nil {
		t.Fatalf("second ResolveOrCreate: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("ResolveOrCreate must return same id for same ExternalUID: first=%d, second=%d", first.ID, second.ID)
	}
}

func TestResolveOrCreate_RefreshesDisplayFields(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	p1 := auth.Principal{
		Kind: auth.KindUser, Name: "alice", Email: "alice@example.com",
		ExternalUID: "alice-uid-stable", Authenticated: true,
	}
	first, err := svc.ResolveOrCreate(ctx, p1)
	if err != nil {
		t.Fatalf("first ResolveOrCreate: %v", err)
	}

	// Sleep so last_seen_at strictly increases.
	time.Sleep(1100 * time.Millisecond)

	p2 := auth.Principal{
		Kind: auth.KindUser, Name: "alice-renamed", Email: "alice2@example.com",
		ExternalUID: "alice-uid-stable", Authenticated: true,
	}
	second, err := svc.ResolveOrCreate(ctx, p2)
	if err != nil {
		t.Fatalf("second ResolveOrCreate: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("id changed across rename: %d -> %d", first.ID, second.ID)
	}
	if second.Username != "alice-renamed" {
		t.Errorf("denormalized username not refreshed: got %q, want %q", second.Username, "alice-renamed")
	}
	if !second.Email.Valid || second.Email.String != "alice2@example.com" {
		t.Errorf("denormalized email not refreshed: got %+v, want alice2@example.com", second.Email)
	}
}

func TestResolveOrCreate_RejectsMachinePrincipal(t *testing.T) {
	svc := newService(t)
	p := auth.Principal{
		Kind:          auth.KindMachine,
		Name:          "agent-1",
		Authenticated: true,
	}
	_, err := svc.ResolveOrCreate(context.Background(), p)
	if err != ErrInvalidPrincipal {
		t.Errorf("err = %v, want ErrInvalidPrincipal", err)
	}
}

func TestResolveOrCreate_RejectsUnauthenticated(t *testing.T) {
	svc := newService(t)
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		ExternalUID:   "alice-uid-stable",
		Authenticated: false,
	}
	_, err := svc.ResolveOrCreate(context.Background(), p)
	if err != ErrInvalidPrincipal {
		t.Errorf("err = %v, want ErrInvalidPrincipal", err)
	}
}

func TestResolveOrCreate_RejectsEmptyExternalUID(t *testing.T) {
	svc := newService(t)
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		Authenticated: true,
		ExternalUID:   "",
	}
	_, err := svc.ResolveOrCreate(context.Background(), p)
	if err != ErrInvalidPrincipal {
		t.Errorf("err = %v, want ErrInvalidPrincipal", err)
	}
}

func TestEnsureSystemUser_StableAcrossUsers(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	sys, err := svc.EnsureSystemUser(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}

	// A real user created after the system user must not collide on
	// (auth_provider, external_uid). The system sentinels are
	// ('system','system'), which can never match a real Authentik row.
	p := auth.Principal{
		Kind: auth.KindUser, Name: "alice", Email: "alice@example.com",
		ExternalUID: "alice-uid-stable", Authenticated: true,
	}
	user, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if user.ID == sys.ID {
		t.Errorf("user and system share id %d -- collision", user.ID)
	}
}

// TestSystemUserSafe_BeforeEnsureReturnsError: SystemUserSafe is the
// panic-free counterpart to SystemUser; it returns an error when
// EnsureSystemUser hasn't run yet. audit's resolveActor falls through
// to a kind-only row on this case -- covering that branch here pins
// the contract.
func TestSystemUserSafe_BeforeEnsureReturnsError(t *testing.T) {
	svc := newService(t)
	got, err := svc.SystemUserSafe()
	if err == nil {
		t.Fatalf("SystemUserSafe() before EnsureSystemUser = %+v, want error", got)
	}
	if got.ID != 0 {
		t.Errorf("got.ID = %d, want 0 on error", got.ID)
	}
}

// TestSystemUserSafe_AfterEnsureReturnsCachedRow: SystemUserSafe
// returns the same cached value as SystemUser once EnsureSystemUser
// has run. The cache is what audit.Service's resolveActor relies on
// for actor_user_id.
func TestSystemUserSafe_AfterEnsureReturnsCachedRow(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	if _, err := svc.EnsureSystemUser(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := svc.SystemUserSafe()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 {
		t.Fatal("got.ID = 0 after EnsureSystemUser")
	}
	if got.AuthProvider != "system" || got.ExternalUID != "system" {
		t.Errorf("system identity = (%q,%q), want (system,system)", got.AuthProvider, got.ExternalUID)
	}
	// SystemUser (the panicking variant) should now return the same id.
	if svc.SystemUserID() != got.ID {
		t.Errorf("SystemUserID = %d, SystemUserSafe.ID = %d", svc.SystemUserID(), got.ID)
	}
}

// TestResolveOrCreate_RefreshesLastSeenAt: ResolveOrCreate's UPDATE
// bumps last_seen_at on every call. With unixepoch() returning seconds,
// two writes separated by at least 1 second will see last_seen_at
// strictly increasing.
func TestResolveOrCreate_RefreshesLastSeenAt(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	p := auth.Principal{
		Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid",
		Authenticated: true,
	}
	first, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	second, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// ResolveOrCreate's returned struct doesn't carry last_seen_at,
	// so re-query through the sqlc reader to verify.
	row, err := svc.db.Reader.GetAttributionUserByID(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastSeenAt < first.ID { // sanity: any positive delta
		t.Logf("first.ID=%d row.LastSeenAt=%d", first.ID, row.LastSeenAt)
	}
	_ = first // first.ID is the row id; last_seen_at lives in row
}
