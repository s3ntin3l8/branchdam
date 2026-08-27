package settings

import (
	"context"
	"log/slog"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func writeRawAppSetting(t *testing.T, database *db.DB, key, jsonValue string) {
	t.Helper()
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.UpsertAppSetting(context.Background(), sqlcgen.UpsertAppSettingParams{
			Key: key, Value: jsonValue, IsSecret: 0, UpdatedBy: "tester",
		})
		return err
	})
	if err != nil {
		t.Fatalf("writeRawAppSetting(%q): %v", key, err)
	}
}

func TestIsStorageLocationField(t *testing.T) {
	for _, f := range []string{"name", "watch", "sweep", "sweepIntervalSecs", "cacheTtlHours", "enabled"} {
		if !IsStorageLocationField(f) {
			t.Errorf("IsStorageLocationField(%q) = false, want true", f)
		}
	}
	for _, f := range []string{"rootPath", "tier", "readOnly", "prunable", "bogus"} {
		if IsStorageLocationField(f) {
			t.Errorf("IsStorageLocationField(%q) = true, want false (config-only or unrecognized)", f)
		}
	}
}

func TestLoadStorageLocationOverrides(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	writeRawAppSetting(t, database, "storageLocation./data/tier1.name", `"Renamed Tier 1"`)
	writeRawAppSetting(t, database, "storageLocation./data/tier1.watch", `true`)
	writeRawAppSetting(t, database, "storageLocation./data/tier1.sweepIntervalSecs", `120`)
	writeRawAppSetting(t, database, "storageLocation./data/tier2.enabled", `false`)

	overrides, err := LoadStorageLocationOverrides(ctx, database)
	if err != nil {
		t.Fatalf("LoadStorageLocationOverrides: %v", err)
	}

	tier1, ok := overrides["/data/tier1"]
	if !ok {
		t.Fatal("no override loaded for /data/tier1")
	}
	if tier1.Name == nil || *tier1.Name != "Renamed Tier 1" {
		t.Errorf("tier1.Name = %v, want \"Renamed Tier 1\"", tier1.Name)
	}
	if tier1.Watch == nil || !*tier1.Watch {
		t.Errorf("tier1.Watch = %v, want true", tier1.Watch)
	}
	if tier1.SweepIntervalSecs == nil || *tier1.SweepIntervalSecs != 120 {
		t.Errorf("tier1.SweepIntervalSecs = %v, want 120", tier1.SweepIntervalSecs)
	}
	if tier1.Sweep != nil {
		t.Errorf("tier1.Sweep = %v, want nil (never set)", tier1.Sweep)
	}

	tier2, ok := overrides["/data/tier2"]
	if !ok {
		t.Fatal("no override loaded for /data/tier2")
	}
	if tier2.Enabled == nil || *tier2.Enabled {
		t.Errorf("tier2.Enabled = %v, want false", tier2.Enabled)
	}
}

func TestLoadStorageLocationOverridesIgnoresUnrelatedKeys(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	writeRawAppSetting(t, database, "immich.apiUrl", `"http://immich:2283"`)

	overrides, err := LoadStorageLocationOverrides(ctx, database)
	if err != nil {
		t.Fatalf("LoadStorageLocationOverrides: %v", err)
	}
	if len(overrides) != 0 {
		t.Errorf("overrides = %v, want empty (immich.apiUrl is not in the storageLocation. namespace)", overrides)
	}
}

func TestResolveStorageLocationsAppliesOverrides(t *testing.T) {
	base := []config.StorageLocation{
		{Name: "Tier 1", RootPath: "/data/tier1", Tier: "TIER1_WORKING", Prunable: true, Watch: false, Sweep: false},
	}
	name := "Renamed"
	watch := true
	sweepSecs := 300
	ttl := 48
	overrides := map[string]StorageLocationOverride{
		"/data/tier1": {Name: &name, Watch: &watch, SweepIntervalSecs: &sweepSecs, CacheTTLHours: &ttl},
	}

	got := ResolveStorageLocations(base, overrides, slog.New(slog.DiscardHandler))
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Name != "Renamed" {
		t.Errorf("Name = %q, want %q", got[0].Name, "Renamed")
	}
	if !got[0].Watch {
		t.Error("Watch = false, want true")
	}
	if got[0].SweepIntervalSecs != 300 {
		t.Errorf("SweepIntervalSecs = %d, want 300", got[0].SweepIntervalSecs)
	}
	if got[0].CacheTTLHours != 48 {
		t.Errorf("CacheTTLHours = %d, want 48", got[0].CacheTTLHours)
	}
	// RootPath is not part of any override -- must survive untouched.
	if got[0].RootPath != "/data/tier1" {
		t.Errorf("RootPath = %q, want unchanged", got[0].RootPath)
	}
}

func TestResolveStorageLocationsDropsDisabledLocation(t *testing.T) {
	base := []config.StorageLocation{
		{Name: "Tier 1", RootPath: "/data/tier1"},
		{Name: "Tier 2", RootPath: "/data/tier2"},
	}
	disabled := false
	overrides := map[string]StorageLocationOverride{
		"/data/tier1": {Enabled: &disabled},
	}

	got := ResolveStorageLocations(base, overrides, slog.New(slog.DiscardHandler))
	if len(got) != 1 || got[0].RootPath != "/data/tier2" {
		t.Fatalf("got = %+v, want only /data/tier2", got)
	}
}

func TestResolveStorageLocationsIgnoresOrphanedOverride(t *testing.T) {
	base := []config.StorageLocation{
		{Name: "Tier 1", RootPath: "/data/tier1"},
	}
	name := "Ghost"
	overrides := map[string]StorageLocationOverride{
		"/data/removed": {Name: &name}, // no longer in config.yaml
	}

	got := ResolveStorageLocations(base, overrides, slog.New(slog.DiscardHandler))
	if len(got) != 1 || got[0].Name != "Tier 1" {
		t.Fatalf("got = %+v, want base unchanged (orphaned override must be inert)", got)
	}
}

func TestResolveStorageLocationsFallsBackOnInvalidCacheTTLOverride(t *testing.T) {
	base := []config.StorageLocation{
		{Name: "Tier 1", RootPath: "/data/tier1", Prunable: false, CacheTTLHours: 0},
	}
	ttl := 24 // positive on a non-prunable location -- invalid
	overrides := map[string]StorageLocationOverride{
		"/data/tier1": {CacheTTLHours: &ttl},
	}

	got := ResolveStorageLocations(base, overrides, slog.New(slog.DiscardHandler))
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (location itself must not be dropped)", len(got))
	}
	if got[0].CacheTTLHours != 0 {
		t.Errorf("CacheTTLHours = %d, want 0 (invalid override must fall back to base, not brick validatePruneConfig)", got[0].CacheTTLHours)
	}
}

func TestResolveStorageLocationsFallsBackOnNegativeCacheTTLOverride(t *testing.T) {
	base := []config.StorageLocation{
		{Name: "Tier 1", RootPath: "/data/tier1", Prunable: true, CacheTTLHours: 12},
	}
	ttl := -1
	overrides := map[string]StorageLocationOverride{
		"/data/tier1": {CacheTTLHours: &ttl},
	}

	got := ResolveStorageLocations(base, overrides, slog.New(slog.DiscardHandler))
	if got[0].CacheTTLHours != 12 {
		t.Errorf("CacheTTLHours = %d, want 12 (negative override must fall back to base)", got[0].CacheTTLHours)
	}
}

func TestApplyStorageLocationOverrideSetAndUnset(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)

	if err := ApplyStorageLocationOverride(ctx, database, "/data/tier1",
		map[string]any{"watch": true, "sweepIntervalSecs": 60}, nil, "tester"); err != nil {
		t.Fatalf("ApplyStorageLocationOverride (set): %v", err)
	}

	overrides, err := LoadStorageLocationOverrides(ctx, database)
	if err != nil {
		t.Fatalf("LoadStorageLocationOverrides: %v", err)
	}
	tier1 := overrides["/data/tier1"]
	if tier1.Watch == nil || !*tier1.Watch {
		t.Fatalf("Watch = %v, want true after set", tier1.Watch)
	}
	if tier1.SweepIntervalSecs == nil || *tier1.SweepIntervalSecs != 60 {
		t.Fatalf("SweepIntervalSecs = %v, want 60 after set", tier1.SweepIntervalSecs)
	}

	if err := ApplyStorageLocationOverride(ctx, database, "/data/tier1",
		nil, []string{"watch"}, "tester"); err != nil {
		t.Fatalf("ApplyStorageLocationOverride (unset): %v", err)
	}

	overrides, err = LoadStorageLocationOverrides(ctx, database)
	if err != nil {
		t.Fatalf("LoadStorageLocationOverrides: %v", err)
	}
	tier1 = overrides["/data/tier1"]
	if tier1.Watch != nil {
		t.Errorf("Watch = %v, want nil after unset", tier1.Watch)
	}
	if tier1.SweepIntervalSecs == nil || *tier1.SweepIntervalSecs != 60 {
		t.Errorf("SweepIntervalSecs = %v, want unaffected by unsetting a different field", tier1.SweepIntervalSecs)
	}
}

// TestStoreReloadIgnoresStorageLocationNamespace pins Store.reload's skip
// branch for the storageLocation.* namespace: a row there must not be
// treated as an "unregistered key" (which would spam a permanent WARN on
// every reload for a namespace this package owns on purpose, see
// storagelocation.go) and must not appear in the generic
// registry-resolved config or IsOverridden/PendingRestart machinery.
func TestStoreReloadIgnoresStorageLocationNamespace(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	writeRawAppSetting(t, database, "storageLocation./data/tier1.watch", `true`)

	store, err := NewStore(ctx, database, config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store.IsOverridden("storageLocation./data/tier1.watch") {
		t.Error("IsOverridden reported a storageLocation.* key as a registry override")
	}
	if got := store.PendingRestart(); len(got) != 0 {
		t.Errorf("PendingRestart() = %v, want empty (storageLocation.* rows aren't registry fields)", got)
	}
}
