package httpapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

// newAttributionServerForMeTest builds a Server with attribution +
// audit wired so /api/v1/me's handleMe resolves AttributionUserID.
func newAttributionServerForMeTest(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attribution.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	usersSvc := attributionusers.NewService(database)
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	auditSvc := audit.NewService(database, usersSvc)

	srv := New(Deps{
		Log:         nil,
		DB:          database,
		Version:     "test",
		Attribution: usersSvc,
		Audit:       auditSvc,

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	return srv
}

// TestHandleMe_AttributionUserID: with attribution wired and an
// authenticated browser Principal, handleMe surfaces AttributionUserID
// equal to the lazy-resolved users.id.
func TestHandleMe_AttributionUserID(t *testing.T) {
	srv := newAttributionServerForMeTest(t)
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		Email:         "alice@example.com",
		ExternalUID:   "alice-uid-stable",
		Authenticated: true,
	}
	ctx := auth.WithPrincipal(context.Background(), p)
	out, err := srv.handleMe(ctx, nil)
	if err != nil {
		t.Fatalf("handleMe: %v", err)
	}
	if out.Body.AttributionUserID == 0 {
		t.Errorf("AttributionUserID = 0, want > 0 (ResolveOrCreate should have provisioned alice)")
	}
}

// TestHandleMe_NoAttributionWhenNoPrincipal: with no Principal in
// context, handleMe returns AttributionUserID = 0.
func TestHandleMe_NoAttributionWhenNoPrincipal(t *testing.T) {
	srv := newAttributionServerForMeTest(t)
	out, err := srv.handleMe(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleMe: %v", err)
	}
	if out.Body.AttributionUserID != 0 {
		t.Errorf("AttributionUserID = %d, want 0 for anonymous request", out.Body.AttributionUserID)
	}
}

// TestHandleMe_AttributionStableAcrossCalls: two requests from the
// same Principal resolve to the same users.id -- the whole point of
// ResolveOrCreate's stable external_uid key.
func TestHandleMe_AttributionStableAcrossCalls(t *testing.T) {
	srv := newAttributionServerForMeTest(t)
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		ExternalUID:   "alice-uid-stable",
		Authenticated: true,
	}
	ctx := auth.WithPrincipal(context.Background(), p)
	first, err := srv.handleMe(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.handleMe(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Body.AttributionUserID != second.Body.AttributionUserID || first.Body.AttributionUserID == 0 {
		t.Errorf("attribution id changed across calls: first=%d, second=%d", first.Body.AttributionUserID, second.Body.AttributionUserID)
	}
}

// TestMeOutput_AttributionUserIDOmitEmpty: the JSON output omits
// AttributionUserID when zero so the SPA's "My uploads" toggle
// stays disabled rather than pinning to id=0.
func TestMeOutput_AttributionUserIDOmitEmpty(t *testing.T) {
	srv := newAttributionServerForMeTest(t)
	out, err := srv.handleMe(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleMe: %v", err)
	}
	bodyJSON, _ := json.Marshal(out.Body)
	// The json:omitempty tag on AttributionUserID means the key
	// shouldn't appear in the rendered JSON when it's zero.
	if got := string(bodyJSON); contains(got, `"attributionUserId":`) {
		t.Errorf("attributionUserId leaked into anonymous /me response: %s", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// newAttributionServerForAdminSyncTest builds a Server with the same
// admin-group policy wired on both sides of the sync this test suite
// covers: Deps.Config.Authz.Groups (what requireSettingsAdmin/handleMe's
// live IsAdmin check reads) and attribution.WithAdminGroups (what
// ResolveOrCreate reads to persist users.is_admin) -- mirroring how
// cmd/branchdam wires both from the same settingsStore.Effective().
func newAttributionServerForAdminSyncTest(t *testing.T, adminGroups []string) (*Server, *attributionusers.Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attribution-admin-sync.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	usersSvc := attributionusers.NewService(database).
		WithAdminGroups(func() []string { return adminGroups })
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	auditSvc := audit.NewService(database, usersSvc)

	srv := New(Deps{
		Config:      &config.Config{Authz: config.Authz{Groups: adminGroups}},
		DB:          database,
		Version:     "test",
		Attribution: usersSvc,
		Audit:       auditSvc,

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	return srv, usersSvc
}

// adminSyncPrincipal returns an authenticated forward-auth Principal
// with a stable external uid, so repeated calls resolve to the same
// users.id across group-membership changes (the promotion/demotion
// test flips Groups between calls on the same struct).
func adminSyncPrincipal(name string, groups []string) auth.Principal {
	return auth.Principal{
		Kind:          auth.KindUser,
		Name:          name,
		Email:         name + "@example.com",
		ExternalUID:   name + "-uid-stable",
		Groups:        groups,
		Authenticated: true,
	}
}

// TestHandleMe_ResolveOrCreateSyncsIsAdmin_GroupMatch: a forward-auth
// principal whose groups include the configured admin group is
// persisted with is_admin=1 by handleMe's ResolveOrCreate call --
// verified by reading it back through GET /api/v1/users, since that's
// the admin-only column the issue's UsersPage.tsx Role display depends
// on (handleMe's own IsAdmin field is computed independently and
// doesn't touch the DB).
func TestHandleMe_ResolveOrCreateSyncsIsAdmin_GroupMatch(t *testing.T) {
	srv, _ := newAttributionServerForAdminSyncTest(t, []string{"dam-admins"})
	p := adminSyncPrincipal("alice", []string{"dam-admins"})
	ctx := auth.WithPrincipal(context.Background(), p)

	if _, err := srv.handleMe(ctx, nil); err != nil {
		t.Fatalf("handleMe: %v", err)
	}

	adminCtx := auth.WithPrincipal(context.Background(), adminPrincipal())
	out, err := srv.handleListUsers(adminCtx, &ListUsersInput{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("handleListUsers: %v", err)
	}
	var found bool
	for _, u := range out.Body.Users {
		if u.Username != "alice" {
			continue
		}
		found = true
		if !u.IsAdmin {
			t.Errorf("alice.isAdmin = false, want true (member of configured admin group dam-admins)")
		}
	}
	if !found {
		t.Fatal("alice not found in GET /api/v1/users after handleMe provisioned her")
	}
}

// TestHandleMe_ResolveOrCreateSyncsIsAdmin_PromotionAndDemotion:
// group membership changes in the identity provider must flip
// users.is_admin (and therefore GET /api/v1/users' isAdmin) on the
// very next request, in both directions -- no separate reconciliation
// step required.
func TestHandleMe_ResolveOrCreateSyncsIsAdmin_PromotionAndDemotion(t *testing.T) {
	srv, _ := newAttributionServerForAdminSyncTest(t, []string{"dam-admins"})
	adminCtx := auth.WithPrincipal(context.Background(), adminPrincipal())

	isAdminFor := func(username string) bool {
		t.Helper()
		out, err := srv.handleListUsers(adminCtx, &ListUsersInput{Limit: 50, Offset: 0})
		if err != nil {
			t.Fatalf("handleListUsers: %v", err)
		}
		for _, u := range out.Body.Users {
			if u.Username == username {
				return u.IsAdmin
			}
		}
		t.Fatalf("%s not found in GET /api/v1/users", username)
		return false
	}

	// First sight: bob is not in the admin group.
	ctx := auth.WithPrincipal(context.Background(), adminSyncPrincipal("bob", nil))
	if _, err := srv.handleMe(ctx, nil); err != nil {
		t.Fatalf("handleMe (first sight): %v", err)
	}
	if isAdminFor("bob") {
		t.Fatal("bob.isAdmin = true on first sight, want false")
	}

	// Promotion.
	ctx = auth.WithPrincipal(context.Background(), adminSyncPrincipal("bob", []string{"dam-admins"}))
	if _, err := srv.handleMe(ctx, nil); err != nil {
		t.Fatalf("handleMe (promoted): %v", err)
	}
	if !isAdminFor("bob") {
		t.Error("bob.isAdmin = false after promotion, want true")
	}

	// Demotion.
	ctx = auth.WithPrincipal(context.Background(), adminSyncPrincipal("bob", nil))
	if _, err := srv.handleMe(ctx, nil); err != nil {
		t.Fatalf("handleMe (demoted): %v", err)
	}
	if isAdminFor("bob") {
		t.Error("bob.isAdmin = true after demotion, want false")
	}
}
