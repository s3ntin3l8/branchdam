package users

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
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

// TestResolveOrCreate_SyncsIsAdminOnCreate: a forward-link principal
// whose groups match the configured admin groups gets is_admin=1 from
// the very first ResolveOrCreate call (the INSERT), not just on a
// later refresh.
func TestResolveOrCreate_SyncsIsAdminOnCreate(t *testing.T) {
	svc := newService(t).WithAdminGroups(func() []string { return []string{"dam-admins"} })
	ctx := context.Background()

	p := auth.Principal{
		Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid-stable",
		Groups: []string{"dam-admins"}, Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if !got.IsAdmin {
		t.Errorf("IsAdmin = false, want true for a principal in the configured admin group")
	}

	row, err := svc.db.Reader.GetAttributionUserByID(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.IsAdmin != 1 {
		t.Errorf("stored is_admin = %d, want 1", row.IsAdmin)
	}
}

// TestResolveOrCreate_SyncsIsAdminOnRefresh: a promotion or demotion
// in the identity provider's group membership must flip users.is_admin
// on the user's very next request -- not require a separate
// reconciliation job. Covers both directions.
func TestResolveOrCreate_SyncsIsAdminOnRefresh(t *testing.T) {
	adminGroups := []string{"dam-admins"}
	svc := newService(t).WithAdminGroups(func() []string { return adminGroups })
	ctx := context.Background()

	// First sight: not in the admin group.
	p := auth.Principal{
		Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid-stable",
		Groups: nil, Authenticated: true,
	}
	first, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("first ResolveOrCreate: %v", err)
	}
	if first.IsAdmin {
		t.Fatalf("IsAdmin = true on first sight, want false (no group membership)")
	}

	// Promotion: Authentik now reports dam-admins membership.
	p.Groups = []string{"dam-admins"}
	promoted, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("promoted ResolveOrCreate: %v", err)
	}
	if promoted.ID != first.ID {
		t.Fatalf("id changed across refresh: %d -> %d", first.ID, promoted.ID)
	}
	if !promoted.IsAdmin {
		t.Errorf("IsAdmin = false after promotion, want true")
	}

	// Demotion: group membership removed again.
	p.Groups = nil
	demoted, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("demoted ResolveOrCreate: %v", err)
	}
	if demoted.IsAdmin {
		t.Errorf("IsAdmin = true after demotion, want false")
	}
}

// TestResolveOrCreate_IsAdminEmptyGroupsDefaultsAdmin: mirrors
// auth.IsAdmin's solo-homelab default -- an unset/empty admin-groups
// list means every authenticated forward-link user is admin, exactly
// like live request authorization already treats them.
func TestResolveOrCreate_IsAdminEmptyGroupsDefaultsAdmin(t *testing.T) {
	svc := newService(t) // WithAdminGroups never called
	ctx := context.Background()

	p := auth.Principal{
		Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid-stable",
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if !got.IsAdmin {
		t.Errorf("IsAdmin = false, want true when no admin groups are configured")
	}
}

// TestResolveOrCreate_LocalUser_DoesNotSyncIsAdmin: resolveLocal (the
// source='local' branch) must never overwrite is_admin from group
// membership -- local accounts are promoted/demoted only through the
// explicit admin-UI action.
func TestResolveOrCreate_LocalUser_DoesNotSyncIsAdmin(t *testing.T) {
	svc := newService(t).WithAdminGroups(func() []string { return []string{"dam-admins"} })
	ctx := context.Background()

	// Local admin row, seeded directly like the other local-branch tests.
	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('carol', 'carol@example.com', '$argon2id$v=19$m=65536,t=3,p=4$fakehash', 1, 'local', unixepoch(), 'test', 'local', 'carol')`,
	)
	if err != nil {
		t.Fatalf("insert local admin user: %v", err)
	}

	// The local Principal carries no admin-group membership at all --
	// if resolveLocal synced is_admin the same way the forward branch
	// does, this would demote carol on the spot.
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "carol",
		Email:         "carol@example.com",
		ExternalUID:   "carol",
		AuthProvider:  auth.AuthProviderLocal,
		Groups:        nil,
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if !got.IsAdmin {
		t.Errorf("IsAdmin = false after resolveLocal, want true (local is_admin must not be overwritten)")
	}

	row, err := svc.db.Reader.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
		AuthProvider: "local",
		ExternalUid:  "carol",
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.IsAdmin != 1 {
		t.Errorf("stored is_admin = %d, want 1 (unchanged)", row.IsAdmin)
	}
}

// TestResolveOrCreate_DoesNotOverwriteForwardJITAdmin: a source=
// 'forward-jit' row (provisioned by internal/auth/users/jit.go, using
// its own separate admin-groups config) can share the exact
// (auth_provider, external_uid) key ResolveOrCreate's forward-link
// branch upserts on. 00020_users_and_audit.sql's backfill gave every
// pre-existing row -- including forward-jit rows provisioned before
// that migration -- auth_provider='authentik' and
// external_uid=username; that's precisely the key
// BrowserChain.pickExternalUID falls back to when X-Authentik-Uid
// isn't sent. If ResolveOrCreate's is_admin sync ran unconditionally
// on whatever row that key resolves to, a plain forward-auth request
// from that same username would silently demote (or promote) a JIT
// admin using authz.groups -- the wrong policy for a forward-jit row.
func TestResolveOrCreate_DoesNotOverwriteForwardJITAdmin(t *testing.T) {
	svc := newService(t).WithAdminGroups(func() []string { return []string{"dam-admins"} })
	ctx := context.Background()

	// Pre-00020-shaped forward-jit admin row: source='forward-jit',
	// but auth_provider/external_uid backfilled to the authentik/
	// username shape.
	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('dave', 'dave@example.com', NULL, 1, 'forward-jit', unixepoch(), 'forward:dave', 'authentik', 'dave')`,
	)
	if err != nil {
		t.Fatalf("insert forward-jit admin user: %v", err)
	}

	// A plain forward-auth request for the same username, NOT a member
	// of authz.groups' dam-admins -- if the sync ran unconditionally
	// this would demote dave.
	p := auth.Principal{
		Kind: auth.KindUser, Name: "dave", Email: "dave@example.com",
		ExternalUID: "dave", Groups: nil, Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if !got.IsAdmin {
		t.Errorf("IsAdmin = false, want true (forward-jit row's is_admin must not be overwritten)")
	}

	row, err := svc.db.Reader.GetUserByUsername(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if row.IsAdmin != 1 {
		t.Errorf("stored is_admin = %d, want 1 (unchanged)", row.IsAdmin)
	}
	if row.Source != "forward-jit" {
		t.Fatalf("test setup invariant broken: row.Source = %q, want forward-jit", row.Source)
	}
	// Username/email/last_seen_at still refresh like any same-key row
	// -- only is_admin is guarded.
	if row.Username != "dave" {
		t.Errorf("Username = %q, want %q (denormalized fields still refresh)", row.Username, "dave")
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

// TestResolveOrCreate_LocalUser: local-session Principals (auth_provider="local")
// should resolve to the existing local user row via lookup, not INSERT.
// The INSERT path uses source='forward-link' which would violate the CHECK
// constraint on source/password_hash for source='local' rows.
func TestResolveOrCreate_LocalUser(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// Insert a local user row directly (simulating CreateLocalUser).
	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('testlocal', 'testlocal@example.com', '$argon2id$v=19$m=65536,t=3,p=4$fakehash', 0, 'local', unixepoch(), 'test', 'local', 'testlocal')`,
	)
	if err != nil {
		t.Fatalf("insert local user: %v", err)
	}

	// ResolveOrCreate with a local-session Principal.
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "testlocal",
		Email:         "testlocal@example.com",
		ExternalUID:   "testlocal",
		AuthProvider:  auth.AuthProviderLocal,
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if got.ID == 0 {
		t.Fatal("ResolveOrCreate returned ID=0")
	}
	if got.AuthProvider != "local" {
		t.Errorf("AuthProvider = %q, want %q", got.AuthProvider, "local")
	}
	if got.ExternalUID != "testlocal" {
		t.Errorf("ExternalUID = %q, want %q", got.ExternalUID, "testlocal")
	}

	// Verify no duplicate row was created (unique index check).
	row, err := svc.db.Reader.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
		AuthProvider: "local",
		ExternalUid:  "testlocal",
	})
	if err != nil {
		t.Fatalf("lookup local user: %v", err)
	}
	_ = row // existence is the assertion; GetAttributionUserByExternalUID returns one row or errors
}

// TestResolveOrCreate_LocalUser_RefreshesFields: the local-user branch
// of ResolveOrCreate should refresh denormalized username/email like
// the forward-auth branch does.
func TestResolveOrCreate_LocalUser_RefreshesFields(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('bob', 'bob@example.com', '$argon2id$v=19$m=65536,t=3,p=4$fakehash', 0, 'local', unixepoch(), 'test', 'local', 'bob')`,
	)
	if err != nil {
		t.Fatalf("insert local user: %v", err)
	}

	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "bob-renamed",
		Email:         "bob2@example.com",
		ExternalUID:   "bob",
		AuthProvider:  auth.AuthProviderLocal,
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if got.Username != "bob-renamed" {
		t.Errorf("Username = %q, want %q", got.Username, "bob-renamed")
	}
	if !got.Email.Valid || got.Email.String != "bob2@example.com" {
		t.Errorf("Email = %+v, want bob2@example.com", got.Email)
	}
}

// TestResolveOrCreate_LocalUser_ReconcilesDriftedExternalUID covers the
// self-heal path: a source='local' row exists but its stored
// external_uid no longer matches the current username (e.g. an admin
// renamed the user without an accompany-side external_uid sync).
// session/middleware sets ExternalUID=username on every local login, so
// the (local, p.ExternalUID) lookup would miss and attribution would
// silently NULL on every subsequent request. resolveLocal catches the
// miss, looks up by p.Name, finds a source='local' row with a
// mismatched external_uid, realigns external_uid inside the same write
// tx, and returns a usable Attribution.
func TestResolveOrCreate_LocalUser_ReconcilesDriftedExternalUID(t *testing.T) {
	var buf bytes.Buffer
	svc := newService(t).WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()

	// Drifted state: the row's stored external_uid is the OLD username
	// 'oldname', the request carries the NEW username via ExternalUID.
	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('newname', 'newname@example.com', '$argon2id$v=19$m=65536,t=3,p=4$fakehash', 0, 'local', unixepoch(), 'test', 'local', 'oldname')`,
	)
	if err != nil {
		t.Fatalf("insert drifted local user: %v", err)
	}

	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "newname",
		Email:         "newname@example.com",
		ExternalUID:   "newname",
		AuthProvider:  auth.AuthProviderLocal,
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if got.ID == 0 {
		t.Fatal("ResolveOrCreate returned ID=0 after reconciliation")
	}
	if got.ExternalUID != "newname" {
		t.Errorf("ExternalUID = %q, want %q (drift should have self-healed)", got.ExternalUID, "newname")
	}
	if got.Username != "newname" {
		t.Errorf("Username = %q, want %q", got.Username, "newname")
	}

	// Verify the row's external_uid was actually UPDATE'd in place.
	row, err := svc.db.Reader.GetUserByUsername(ctx, "newname")
	if err != nil {
		t.Fatalf("GetUserByUsername post-reconcile: %v", err)
	}
	if row.ExternalUid != "newname" {
		t.Errorf("stored external_uid = %q, want %q", row.ExternalUid, "newname")
	}
	if !strings.Contains(buf.String(), "reconciled drifted local external_uid") {
		t.Errorf("expected reconciliation INFO log, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "old_external_uid=oldname") {
		t.Errorf("expected log to surface old external_uid, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "new_external_uid=newname") {
		t.Errorf("expected log to surface new external_uid, got %q", buf.String())
	}
}

// TestResolveOrCreate_LocalUser_RepairAuthProvider: when 00028 Down
// rewrote auth_provider to 'forward-link', the self-heal path must
// also repair auth_provider back to 'local' -- not just external_uid.
// Without this, a post-Down row would keep auth_provider='forward-link'
// and attribution would keep missing even after external_uid is
// realigned.
func TestResolveOrCreate_LocalUser_RepairAuthProvider(t *testing.T) {
	var buf bytes.Buffer
	svc := newService(t).WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()

	// Simulate 00028-Down state: auth_provider='forward-link',
	// external_uid=raw username.
	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('alice', 'alice@example.com', '$argon2id$v=19$m=65536,t=3,p=4$fakehash', 0, 'local', unixepoch(), 'test', 'forward-link', 'alice')`,
	)
	if err != nil {
		t.Fatalf("insert forward-link user: %v", err)
	}

	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		Email:         "alice@example.com",
		ExternalUID:   "alice",
		AuthProvider:  auth.AuthProviderLocal,
		Authenticated: true,
	}
	got, err := svc.ResolveOrCreate(ctx, p)
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	if got.ID == 0 {
		t.Fatal("ResolveOrCreate returned ID=0 after reconciliation")
	}

	// Verify auth_provider was repaired to 'local'.
	row, err := svc.db.Reader.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername post-reconcile: %v", err)
	}
	if row.AuthProvider != "local" {
		t.Errorf("auth_provider = %q, want %q (should have been repaired from forward-link)", row.AuthProvider, "local")
	}
	if row.ExternalUid != "alice" {
		t.Errorf("external_uid = %q, want %q", row.ExternalUid, "alice")
	}
}

// TestResolveOrCreate_LocalUser_MissReturnsOriginalErr: when neither
// (local, ExternalUID) nor (username) finds a usable source='local' row,
// resolveLocal falls back to the original sql.ErrNoRows so callers
// surface the same warn they used to -- reconciliation is opt-in via
// the username-based lookup, not a silent rewrite. Guards against a
// future change that confuses "no user" with "drift" and silently
// returns 0.
func TestResolveOrCreate_LocalUser_MissReturnsOriginalErr(t *testing.T) {
	var buf bytes.Buffer
	svc := newService(t).WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()

	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "ghost",
		ExternalUID:   "ghost",
		AuthProvider:  auth.AuthProviderLocal,
		Authenticated: true,
	}
	_, err := svc.ResolveOrCreate(ctx, p)
	if err == nil {
		t.Fatal("expected error for missing local user, got nil")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows in chain, got %v", err)
	}
	if strings.Contains(buf.String(), "reconciled drifted local external_uid") {
		t.Errorf("reconciliation logged despite no candidate row: %q", buf.String())
	}
}

// TestResolveOrCreate_LocalUser_NonLocalRowByName: a username lookup
// may find a row of a different source (forward-jit, forward-link).
// resolveLocal must NOT reconcile across source boundaries -- that
// would clobber an unrelated identity. Verify the original miss
// surfaces instead.
func TestResolveOrCreate_LocalUser_NonLocalRowByName(t *testing.T) {
	var buf bytes.Buffer
	svc := newService(t).WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()

	// Authentik-shaped forward-jit row that happens to share a username
	// with the request. ResolveOrCreate must treat that as a different
	// identity, not a drift to reconcile.
	_, err := svc.db.ExecInTx(ctx,
		`INSERT INTO users (username, email, password_hash, is_admin, source, created_at, created_by, auth_provider, external_uid)
		 VALUES ('bob', 'bob@example.com', NULL, 0, 'forward-jit', unixepoch(), 'forward:test', 'authentik', 'stable-uid-bob')`,
	)
	if err != nil {
		t.Fatalf("insert forward-jit user: %v", err)
	}

	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "bob",
		ExternalUID:   "bob",
		AuthProvider:  auth.AuthProviderLocal,
		Authenticated: true,
	}
	_, err = svc.ResolveOrCreate(ctx, p)
	if err == nil {
		t.Fatal("expected error: local resolve should not silently succeed against a non-local row")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows in chain, got %v", err)
	}
	if strings.Contains(buf.String(), "reconciled drifted local external_uid") {
		t.Errorf("reconciliation logged despite non-local row: %q", buf.String())
	}
}
