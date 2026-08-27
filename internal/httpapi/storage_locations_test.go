package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
)

func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

// seedTestStorageLocation inserts a storage_locations row directly (not
// through seedStorageLocations, which lives in cmd/branchdam and isn't
// reachable from this package) and returns its id.
func seedTestStorageLocation(t *testing.T, srv *Server, name, rootPath, tier string, prunable bool) int64 {
	t.Helper()
	pr := int64(0)
	if prunable {
		pr = 1
	}
	var loc sqlcgen.StorageLocation
	err := srv.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: name, RootPath: rootPath, Tier: tier, Prunable: pr,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed storage location: %v", err)
	}
	return loc.ID
}

func decodePutStorageLocationBody(t *testing.T, rr *httptest.ResponseRecorder) struct {
	OK bool `json:"ok"`
} {
	t.Helper()
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rr.Body.String())
	}
	return body
}

func TestPutStorageLocationRequiresAdmin(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), bytes.NewReader(settingsGetJSON(map[string]any{
		"set": map[string]any{"watch": true},
	})))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}

// TestPutStorageLocationRejectsMachinePrincipal pins requireSettingsAdmin's
// KindMachine == forbidden branch specifically through this route -- an
// agent holding only its API key must not be able to rename or disable a
// storage location, the same guarantee requireSettingsAdmin exists to give
// the generic settings routes.
func TestPutStorageLocationRejectsMachinePrincipal(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "storage-location-machine.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{
		Authz: config.Authz{Groups: []string{"dam-admins"}},
		Agent: config.Agent{APIKey: "01234567890123456789012345678901"}, // 33 chars, >= minAgentKeyLength
	}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	srv := New(Deps{Settings: store, DB: database, Hub: sse.New(), Version: "test"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), bytes.NewReader(settingsGetJSON(map[string]any{
		"set": map[string]any{"watch": true},
	})))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "01234567890123456789012345678901")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}

func TestPutStorageLocationSetThenGetReflectsOverride(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"watch": true, "sweepIntervalSecs": float64(600)},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !decodePutStorageLocationBody(t, rr).OK {
		t.Fatal("PUT response ok = false")
	}

	overrides, err := settings.LoadStorageLocationOverrides(context.Background(), srv.db)
	if err != nil {
		t.Fatalf("LoadStorageLocationOverrides: %v", err)
	}
	ov, ok := overrides["/data/tier1"]
	if !ok {
		t.Fatal("no override recorded for /data/tier1")
	}
	if ov.Watch == nil || !*ov.Watch {
		t.Errorf("Watch = %v, want true", ov.Watch)
	}
	if ov.SweepIntervalSecs == nil || *ov.SweepIntervalSecs != 600 {
		t.Errorf("SweepIntervalSecs = %v, want 600", ov.SweepIntervalSecs)
	}
}

func TestPutStorageLocationUnsetRemovesOverride(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"watch": true},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT (set) status = %d, body=%s", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"unset": []string{"watch"},
	})))
	if rr2.Code != http.StatusOK {
		t.Fatalf("PUT (unset) status = %d, body=%s", rr2.Code, rr2.Body.String())
	}

	overrides, err := settings.LoadStorageLocationOverrides(context.Background(), srv.db)
	if err != nil {
		t.Fatalf("LoadStorageLocationOverrides: %v", err)
	}
	if ov, ok := overrides["/data/tier1"]; ok && ov.Watch != nil {
		t.Errorf("Watch override = %v, want nil after unset", ov.Watch)
	}
}

func TestPutStorageLocationRejectsUnknownField(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"rootPath": "/etc/passwd"},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
}

func TestPutStorageLocationRejectsCacheTTLOnNonPrunable(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"cacheTtlHours": float64(24)},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
}

func TestPutStorageLocationAllowsCacheTTLOnPrunable(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1 Cache", "/data/cache", "TIER1_LOCAL_SCRATCH", true)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"cacheTtlHours": float64(24)},
	})))
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

// settingsTestServerWithLocations builds a server whose store's effective
// config.StorageLocations matches locs -- countEnabledStorageLocations reads
// s.cfg().StorageLocations (the config.yaml-effective list), not the
// storage_locations table, so a test exercising the last-enabled guard must
// populate both the DB rows (via seedTestStorageLocation) and this config
// list, the same way main.go's resolve-then-seed keeps them in agreement.
func settingsTestServerWithLocations(t *testing.T, adminGroups []string, locs []config.StorageLocation) *Server {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "storage-locations.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{Authz: config.Authz{Groups: adminGroups}, StorageLocations: locs}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	return New(Deps{Settings: store, DB: database, Hub: sse.New(), Version: "test"})
}

func TestPutStorageLocationRejectsDisablingLastEnabledLocation(t *testing.T) {
	srv := settingsTestServerWithLocations(t, []string{"dam-admins"}, []config.StorageLocation{
		{Name: "Only Location", RootPath: "/data/only", Tier: "TIER1_LOCAL_SCRATCH"},
	})
	id := seedTestStorageLocation(t, srv, "Only Location", "/data/only", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"enabled": false},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
}

func TestPutStorageLocationAllowsDisablingWhenAnotherRemainsEnabled(t *testing.T) {
	srv := settingsTestServerWithLocations(t, []string{"dam-admins"}, []config.StorageLocation{
		{Name: "Location 1", RootPath: "/data/one", Tier: "TIER1_LOCAL_SCRATCH"},
		{Name: "Location 2", RootPath: "/data/two", Tier: "TIER1_LOCAL_SCRATCH"},
	})
	id1 := seedTestStorageLocation(t, srv, "Location 1", "/data/one", "TIER1_LOCAL_SCRATCH", false)
	seedTestStorageLocation(t, srv, "Location 2", "/data/two", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id1), settingsGetJSON(map[string]any{
		"set": map[string]any{"enabled": false},
	})))
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

// TestPutStorageLocationLastEnabledGuardIgnoresOrphanedDBRow pins the fix
// for the guard's data source: a DB row can outlive its rootPath being
// removed from config.yaml (deactivated, never deleted -- see the schema's
// "rows are never deleted" invariant). Before this fix,
// countEnabledStorageLocations walked storage_locations directly, so this
// orphaned row would count as "still enabled" and wrongly block disabling
// the only location config.yaml actually still lists.
func TestPutStorageLocationLastEnabledGuardIgnoresOrphanedDBRow(t *testing.T) {
	srv := settingsTestServerWithLocations(t, []string{"dam-admins"}, []config.StorageLocation{
		{Name: "Only Location", RootPath: "/data/only", Tier: "TIER1_LOCAL_SCRATCH"},
	})
	id := seedTestStorageLocation(t, srv, "Only Location", "/data/only", "TIER1_LOCAL_SCRATCH", false)
	// A row config.yaml no longer lists -- e.g. removed after a prior scan.
	seedTestStorageLocation(t, srv, "Orphaned", "/data/orphaned", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"enabled": false},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s -- an orphaned DB row must not count as \"still enabled\"", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
}

func TestPutStorageLocationReturns404ForUnknownID(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/999", settingsGetJSON(map[string]any{
		"set": map[string]any{"watch": true},
	})))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
}

func TestPutStorageLocationBroadcastsOnSuccess(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	id := seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)

	nudged, unsubscribe := srv.hub.Subscribe()
	defer unsubscribe()

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(id), settingsGetJSON(map[string]any{
		"set": map[string]any{"watch": true},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rr.Code, rr.Body.String())
	}

	select {
	case <-nudged:
	default:
		t.Error("no SSE nudge broadcast on a successful PUT")
	}
}

// TestStorageHealthReflectsStorageLocationOverrides pins the
// handleStorageHealth merge logic: Watch/Sweep/SweepIntervalSecs come from
// the live resolved config (s.cfg().StorageLocations) with any
// storageLocation.* override layered on top, and Disabled is sourced from
// an "enabled": false override independently of IsActive -- an operator
// disabling a location via PUT must see it immediately, without waiting
// for a restart's seed to flip is_active.
func TestStorageHealthReflectsStorageLocationOverrides(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "storage-health-overrides.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{
		StorageLocations: []config.StorageLocation{
			{Name: "Tier 1", RootPath: "/data/tier1", Tier: "TIER1_LOCAL_SCRATCH", Watch: true, Sweep: false, SweepIntervalSecs: 300},
			{Name: "Tier 2", RootPath: "/data/tier2", Tier: "TIER1_LOCAL_SCRATCH", Watch: false, Sweep: true, SweepIntervalSecs: 600},
		},
	}
	// settingsTestKey (settings_test.go) is passed here purely for
	// consistency with settingsTestServer's construction -- no secret field
	// is touched in this test, so a nil box would behave identically.
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	srv := New(Deps{Settings: store, DB: database, Hub: sse.New(), Version: "test"})

	seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)
	seedTestStorageLocation(t, srv, "Tier 2", "/data/tier2", "TIER1_LOCAL_SCRATCH", false)

	if err := settings.ApplyStorageLocationOverride(context.Background(), database, "/data/tier1",
		map[string]any{"sweep": true, "name": "Renamed Tier 1"}, nil, "tester"); err != nil {
		t.Fatalf("ApplyStorageLocationOverride (tier1): %v", err)
	}
	if err := settings.ApplyStorageLocationOverride(context.Background(), database, "/data/tier2",
		map[string]any{"enabled": false}, nil, "tester"); err != nil {
		t.Fatalf("ApplyStorageLocationOverride (tier2): %v", err)
	}

	rr := httptest.NewRequest(http.MethodGet, "/api/v1/storage-health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, rr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Locations []struct {
			Name              string   `json:"name"`
			RootPath          string   `json:"rootPath"`
			Watch             bool     `json:"watch"`
			Sweep             bool     `json:"sweep"`
			SweepIntervalSecs int      `json:"sweepIntervalSecs"`
			Disabled          bool     `json:"disabled"`
			IsActive          bool     `json:"isActive"`
			OverriddenFields  []string `json:"overriddenFields"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rec.Body.String())
	}

	byRootPath := make(map[string]struct {
		Name              string   `json:"name"`
		RootPath          string   `json:"rootPath"`
		Watch             bool     `json:"watch"`
		Sweep             bool     `json:"sweep"`
		SweepIntervalSecs int      `json:"sweepIntervalSecs"`
		Disabled          bool     `json:"disabled"`
		IsActive          bool     `json:"isActive"`
		OverriddenFields  []string `json:"overriddenFields"`
	})
	for _, l := range body.Locations {
		byRootPath[l.RootPath] = l
	}

	tier1, ok := byRootPath["/data/tier1"]
	if !ok {
		t.Fatal("no /data/tier1 in response")
	}
	if tier1.Name != "Renamed Tier 1" {
		t.Errorf("tier1.Name = %q, want overridden value", tier1.Name)
	}
	if !tier1.Watch {
		t.Error("tier1.Watch = false, want true (from base config, untouched by override)")
	}
	if !tier1.Sweep {
		t.Error("tier1.Sweep = false, want true (overridden)")
	}
	if tier1.SweepIntervalSecs != 300 {
		t.Errorf("tier1.SweepIntervalSecs = %d, want 300 (from base config, untouched by override)", tier1.SweepIntervalSecs)
	}
	if tier1.Disabled {
		t.Error("tier1.Disabled = true, want false")
	}
	wantTier1Overridden := map[string]bool{"sweep": true, "name": true}
	if len(tier1.OverriddenFields) != len(wantTier1Overridden) {
		t.Errorf("tier1.OverriddenFields = %v, want exactly %v", tier1.OverriddenFields, wantTier1Overridden)
	}
	for _, f := range tier1.OverriddenFields {
		if !wantTier1Overridden[f] {
			t.Errorf("tier1.OverriddenFields contains unexpected field %q", f)
		}
	}

	tier2, ok := byRootPath["/data/tier2"]
	if !ok {
		t.Fatal("no /data/tier2 in response")
	}
	if !tier2.Disabled {
		t.Error("tier2.Disabled = false, want true (enabled: false override)")
	}
	if !tier2.IsActive {
		t.Error("tier2.IsActive = false, want true -- Disabled must be independent of IsActive until the next restart's seed catches up")
	}
	if len(tier2.OverriddenFields) != 1 || tier2.OverriddenFields[0] != "enabled" {
		t.Errorf("tier2.OverriddenFields = %v, want [\"enabled\"]", tier2.OverriddenFields)
	}
}
