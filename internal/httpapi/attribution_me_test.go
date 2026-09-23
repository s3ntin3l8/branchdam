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
	"github.com/s3ntin3l8/branchdam/internal/settings"
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

// newAttributionServerForAdminSyncTest builds a Server the way
// cmd/branchdam wires it at boot: a settings.Store providing the live
// authz.groups list, and the attribution service's WithAdminGroups
// pointed at that same store, so ResolveOrCreate's persisted is_admin
// and handleMe's live auth.IsAdmin verdict are computed from the same
// source (issue #485).
func newAttributionServerForAdminSyncTest(t *testing.T, adminGroups []string) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attribution-admin.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{Authz: config.Authz{Groups: adminGroups}}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}

	usersSvc := attributionusers.NewService(database).
		WithAdminGroups(func() []string { return store.Effective().Authz.Groups })
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	auditSvc := audit.NewService(database, usersSvc)

	srv := New(Deps{
		Settings:    store,
		DB:          database,
		Version:     "test",
		Attribution: usersSvc,
		Audit:       auditSvc,

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	return srv
}

// TestHandleMe_ForwardAuth_IsAdminSyncedToAttributionRow: issue #485 --
// a forward-auth Principal whose Groups match the configured admin
// groups must, via ResolveOrCreate inside handleMe, persist
// users.is_admin=1 -- not just the live-computed /me isAdmin value.
// GET /api/v1/users (handleListUsers) reads is_admin straight from the
// DB column, so this is what makes the Users page's Role column agree
// with what handleMe itself reports for the same Principal.
func TestHandleMe_ForwardAuth_IsAdminSyncedToAttributionRow(t *testing.T) {
	srv := newAttributionServerForAdminSyncTest(t, []string{"dam-admins"})
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          "alice",
		ExternalUID:   "alice-uid-stable",
		Groups:        []string{"dam-admins"},
		Authenticated: true,
	}
	ctx := auth.WithPrincipal(context.Background(), p)
	out, err := srv.handleMe(ctx, nil)
	if err != nil {
		t.Fatalf("handleMe: %v", err)
	}
	if !out.Body.IsAdmin {
		t.Fatalf("handleMe reported IsAdmin=false for a dam-admins member")
	}
	if out.Body.AttributionUserID == 0 {
		t.Fatalf("AttributionUserID = 0, want > 0")
	}

	dbRow, err := srv.db.Reader.GetAttributionUserByID(ctx, out.Body.AttributionUserID)
	if err != nil {
		t.Fatalf("GetAttributionUserByID: %v", err)
	}
	if dbRow.IsAdmin != 1 {
		t.Errorf("stored is_admin = %d, want 1 to match handleMe's live IsAdmin=true", dbRow.IsAdmin)
	}
}

// TestHandleMe_ForwardAuth_IsAdminSyncedOnDemotion: a Principal that
// loses admin-group membership between requests must see its persisted
// is_admin flip back to 0 on the very next ResolveOrCreate -- not just
// its live /me verdict, which was already correct before this fix.
func TestHandleMe_ForwardAuth_IsAdminSyncedOnDemotion(t *testing.T) {
	srv := newAttributionServerForAdminSyncTest(t, []string{"dam-admins"})

	admin := auth.Principal{
		Kind: auth.KindUser, Name: "bob", ExternalUID: "bob-uid",
		Groups: []string{"dam-admins"}, Authenticated: true,
	}
	firstCtx := auth.WithPrincipal(context.Background(), admin)
	first, err := srv.handleMe(firstCtx, nil)
	if err != nil {
		t.Fatalf("handleMe (admin): %v", err)
	}
	if !first.Body.IsAdmin {
		t.Fatalf("handleMe reported IsAdmin=false on first (admin) request")
	}

	demoted := admin
	demoted.Groups = []string{"everyone"}
	secondCtx := auth.WithPrincipal(context.Background(), demoted)
	second, err := srv.handleMe(secondCtx, nil)
	if err != nil {
		t.Fatalf("handleMe (demoted): %v", err)
	}
	if second.Body.IsAdmin {
		t.Fatalf("handleMe reported IsAdmin=true after demotion out of dam-admins")
	}

	dbRow, err := srv.db.Reader.GetAttributionUserByID(context.Background(), second.Body.AttributionUserID)
	if err != nil {
		t.Fatalf("GetAttributionUserByID: %v", err)
	}
	if dbRow.IsAdmin != 0 {
		t.Errorf("stored is_admin = %d, want 0 after demotion", dbRow.IsAdmin)
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
