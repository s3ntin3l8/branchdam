package settings

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "settings_test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=") // 32 bytes, fixed test key
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

func TestStoreApplyThenEffectiveReflectsOverride(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	base := config.Config{Immich: config.Immich{APIURL: "${IMMICH_API_URL}"}}

	store, err := NewStore(ctx, database, base, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := store.Effective().Immich.APIURL; got != "" {
		t.Fatalf("before Apply: Immich.APIURL = %q, want empty (unresolved ref normalized)", got)
	}

	if err := store.Apply(ctx, map[string]any{"immich.apiUrl": "http://immich.example:2283"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := store.Effective().Immich.APIURL; got != "http://immich.example:2283" {
		t.Errorf("after Apply: Immich.APIURL = %q, want override value", got)
	}
}

func TestStoreApplyEmptyStringDisablesOverPopulatedBase(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	base := config.Config{Immich: config.Immich{APIURL: "http://from-env.example:2283"}}

	store, err := NewStore(ctx, database, base, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Apply(ctx, map[string]any{"immich.apiUrl": ""}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := store.Effective().Immich.APIURL; got != "" {
		t.Errorf("Immich.APIURL = %q, want empty (explicit disable must beat populated env base)", got)
	}
}

func TestStoreRevertToConfigViaUnset(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	base := config.Config{Immich: config.Immich{APIURL: "http://from-env.example:2283"}}

	store, err := NewStore(ctx, database, base, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Apply(ctx, map[string]any{"immich.apiUrl": "http://override.example:2283"}, nil, "tester"); err != nil {
		t.Fatalf("Apply set: %v", err)
	}
	if err := store.Apply(ctx, nil, []string{"immich.apiUrl"}, "tester"); err != nil {
		t.Fatalf("Apply unset: %v", err)
	}
	if got := store.Effective().Immich.APIURL; got != "http://from-env.example:2283" {
		t.Errorf("after revert: Immich.APIURL = %q, want base value restored", got)
	}
}

// TestStoreSurvivesRestartWithOverridePresent is the seeder-does-not-clobber
// regression the plan calls out: an override applied before a "restart"
// (a fresh NewStore against the same DB) must still be in effect after.
func TestStoreSurvivesRestartWithOverridePresent(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	base := config.Config{Immich: config.Immich{LibraryID: "${IMMICH_LIBRARY_ID}"}}
	box := testBox(t)

	first, err := NewStore(ctx, database, base, box, nil)
	if err != nil {
		t.Fatalf("NewStore (first boot): %v", err)
	}
	if err := first.Apply(ctx, map[string]any{"immich.libraryId": "my-library"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	second, err := NewStore(ctx, database, base, box, nil)
	if err != nil {
		t.Fatalf("NewStore (second boot): %v", err)
	}
	if got := second.Effective().Immich.LibraryID; got != "my-library" {
		t.Errorf("after restart: Immich.LibraryID = %q, want override to survive", got)
	}
}

func TestStoreSecretRoundTripsThroughEncryption(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	box := testBox(t)

	store, err := NewStore(ctx, database, config.Config{}, box, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Apply(ctx, map[string]any{"immich.apiKey": "top-secret-key"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := store.Effective().Immich.APIKey; got != "top-secret-key" {
		t.Errorf("Immich.APIKey = %q, want plaintext round-trip through Effective()", got)
	}

	rows, err := database.Reader.ListAppSettings(ctx)
	if err != nil {
		t.Fatalf("ListAppSettings: %v", err)
	}
	for _, row := range rows {
		if row.Key != "immich.apiKey" {
			continue
		}
		if row.IsSecret == 0 {
			t.Error("immich.apiKey row not marked is_secret")
		}
		if row.Value == `"top-secret-key"` {
			t.Error("app_settings.value stores the plaintext secret unencrypted")
		}
	}
}

func TestStoreApplyRejectsUnknownField(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, testDB(t), config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Apply(ctx, map[string]any{"not.a.real.field": "x"}, nil, "tester"); err == nil {
		t.Error("Apply with unknown field key = nil error, want an error")
	}
}

func TestStoreApplyPartialFailureWritesNothing(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store, err := NewStore(ctx, database, config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	err = store.Apply(ctx, map[string]any{
		"immich.apiUrl":    "http://immich.example:2283",
		"not.a.real.field": "x",
	}, nil, "tester")
	if err == nil {
		t.Fatal("Apply with one invalid key = nil error, want an error")
	}
	if got := store.Effective().Immich.APIURL; got != "" {
		t.Errorf("Immich.APIURL = %q, want unchanged -- a rejected batch must write nothing", got)
	}
}

// TestStoreDegradesWhenSecretKeyUnavailable pins the invalid-override
// degradation contract: a secret row that cannot be decrypted (here,
// because no key was ever configured) must not prevent the store from
// booting -- it drops back to the config/env base value for that field
// only.
func TestStoreDegradesWhenSecretKeyUnavailable(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)

	seedStore, err := NewStore(ctx, database, config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore (seed): %v", err)
	}
	if err := seedStore.Apply(ctx, map[string]any{"immich.apiKey": "top-secret-key"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	base := config.Config{Immich: config.Immich{APIKey: "from-env-key"}}
	degraded, err := NewStore(ctx, database, base, nil /* no secret box available */, nil)
	if err != nil {
		t.Fatalf("NewStore with nil box should not fail to boot: %v", err)
	}
	if got := degraded.Effective().Immich.APIKey; got != "from-env-key" {
		t.Errorf("Immich.APIKey = %q, want fallback to config/env base value when secret key is unavailable", got)
	}
}

func TestStoreDegradesOnUndecryptableSecret(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)

	seedStore, err := NewStore(ctx, database, config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore (seed): %v", err)
	}
	if err := seedStore.Apply(ctx, map[string]any{"immich.apiKey": "top-secret-key"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wrongBox, err := secrets.NewBox("ZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmY=")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	base := config.Config{Immich: config.Immich{APIKey: "from-env-key"}}
	degraded, err := NewStore(ctx, database, base, wrongBox, nil)
	if err != nil {
		t.Fatalf("NewStore with wrong key should not fail to boot: %v", err)
	}
	if got := degraded.Effective().Immich.APIKey; got != "from-env-key" {
		t.Errorf("Immich.APIKey = %q, want fallback to config/env base value on decrypt failure", got)
	}
}

func TestStorePendingRestartEmptyForLiveFields(t *testing.T) {
	// All registered fields in this PR are ApplyLive (Immich), so
	// PendingRestart must always report empty -- there is nothing yet that
	// requires a restart to take effect.
	ctx := context.Background()
	store, err := NewStore(ctx, testDB(t), config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Apply(ctx, map[string]any{"immich.apiUrl": "http://x.example:2283"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := store.PendingRestart(); len(got) != 0 {
		t.Errorf("PendingRestart() = %v, want empty (no ApplyRestart fields registered yet)", got)
	}
}

func TestStoreSubscribeNotifiedOnApply(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, testDB(t), config.Config{}, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var notified *config.Config
	store.Subscribe(func(cfg *config.Config) { notified = cfg })

	if err := store.Apply(ctx, map[string]any{"immich.apiUrl": "http://x.example:2283"}, nil, "tester"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if notified == nil {
		t.Fatal("subscriber was never called")
	}
	if notified.Immich.APIURL != "http://x.example:2283" {
		t.Errorf("subscriber saw Immich.APIURL = %q, want the new value", notified.Immich.APIURL)
	}
}
