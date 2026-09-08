package httpapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
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
	})
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
