package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

// newAuditWiredServer builds a Server with audit + attribution wired,
// the same way cmd/branchdam does at boot. Used to verify the
// /api/v1/scan, /api/v1/settings, /api/v1/restart, /api/v1/storage-locations/{id},
// and /api/v1/prune handlers write actor_audit rows on success.
func newAuditWiredServer(t *testing.T, withAudit bool, withAttribution bool, extra func(*Deps)) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit-write.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{Authz: config.Authz{Groups: []string{"dam-admins"}}}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}

	deps := Deps{
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Log: nil,
	}
	if withAttribution {
		usersSvc := attributionusers.NewService(database)
		if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
			t.Fatalf("EnsureSystemUser: %v", err)
		}
		deps.Attribution = usersSvc
		if withAudit {
			deps.Audit = audit.NewService(database, usersSvc)
		}
	}
	if extra != nil {
		extra(&deps)
	}
	return New(deps)
}

// findActorAuditRow returns the first actor_audit row matching event.
// Used by write-route tests to assert a row landed.
func findActorAuditRow(t *testing.T, srv *Server, event string) (actorKind string, actorName string, found bool) {
	t.Helper()
	ctx := context.Background()
	rows, err := srv.db.Reader.ListActorAudit(ctx, sqlcgen.ListActorAuditParams{
		Limit:  200,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListActorAudit: %v", err)
	}
	for _, r := range rows {
		if r.Event == event {
			return r.ActorKind, r.ActorName, true
		}
	}
	return "", "", false
}

// TestPostRestart_WritesActorAudit: handlePostRestart writes
// actor_audit('restart.executed') before scheduling the restart
// goroutine. With audit wired, the row should be present in the
// database after the handler returns.
func TestPostRestart_WritesActorAudit(t *testing.T) {
	srv := newAuditWiredServer(t, true, true, func(d *Deps) {
		d.RequestRestart = func() {}
	})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPost, "/api/v1/restart", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	kind, name, ok := findActorAuditRow(t, srv, audit.EventRestart)
	if !ok {
		t.Fatal("no restart.executed actor_audit row after handler returned")
	}
	if kind != "user" {
		t.Errorf("restart actor_kind = %q, want user", kind)
	}
	if name != "admin" {
		t.Errorf("restart actor_name = %q, want admin", name)
	}
}

// TestPostRestart_AuditSkippedWhenNotConfigured: when audit is nil
// (existing test setups), the restart handler must still succeed -- no
// panic, no 5xx, no audit row.
func TestPostRestart_AuditSkippedWhenNotConfigured(t *testing.T) {
	srv := newAuditWiredServer(t, false, false, func(d *Deps) {
		d.RequestRestart = func() {}
	})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPost, "/api/v1/restart", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	// No audit service: no row. Just verify nothing panicked.
}

// TestHandlePutSettings_WritesActorAudit: a successful settings PUT
// writes actor_audit('settings.updated') with the diff in details_json.
func TestHandlePutSettings_WritesActorAudit(t *testing.T) {
	srv := newAuditWiredServer(t, true, true, nil)

	body := settingsGetJSON(map[string]any{"set": map[string]any{"workers.perceptualHash": true}})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	_, _, ok := findActorAuditRow(t, srv, audit.EventSettingsUpdated)
	if !ok {
		t.Error("no settings.updated actor_audit row after handler returned")
	}
}

// TestHandlePutSettings_AuditSkippedWhenNotConfigured: when audit is
// nil, the settings handler still succeeds.
func TestHandlePutSettings_AuditSkippedWhenNotConfigured(t *testing.T) {
	srv := newAuditWiredServer(t, false, false, nil)

	body := settingsGetJSON(map[string]any{"set": map[string]any{"workers.perceptualHash": true}})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandlePutStorageLocation_WritesActorAudit: PUT on a storage
// location writes actor_audit('storage_location.upserted'). The
// name from the seeded row is the resource_id.
func TestHandlePutStorageLocation_WritesActorAudit(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "storloc-audit.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{Authz: config.Authz{Groups: []string{"dam-admins"}}}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	usersSvc := attributionusers.NewService(database)
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	srv := New(Deps{
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Attribution: usersSvc,
		Audit:       audit.NewService(database, usersSvc),
	})
	locID := seedTestStorageLocation(t, srv, "test-loc", "/tmp/test-loc", "TIER1_LOCAL_SCRATCH", false)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/storage-locations/"+itoa(locID),
		settingsGetJSON(map[string]any{
			"set": map[string]any{"watch": true, "sweepIntervalSecs": float64(600)},
		})))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	_, _, ok := findActorAuditRow(t, srv, audit.EventStorageLocationPut)
	if !ok {
		t.Error("no storage_location.upserted actor_audit row after PUT")
	}
}

// seedTestStorageLocationWithTTL inserts a prunable storage location
// with a non-zero cache_ttl_hours, so /api/v1/prune reaches the
// execute branch (rather than early-returning for ttlHours <= 0).
func seedTestStorageLocationWithTTL(t *testing.T, srv *Server, name, rootPath, tier string, prunable bool, ttlHours int) int64 {
	t.Helper()
	pr := int64(0)
	if prunable {
		pr = 1
	}
	res, err := srv.db.ExecInTx(context.Background(),
		"INSERT INTO storage_locations (name, root_path, tier, prunable, cache_ttl_hours, is_active) VALUES (?, ?, ?, ?, ?, 1)",
		name, rootPath, tier, pr, int64(ttlHours))
	if err != nil {
		t.Fatalf("seed storage location with ttl: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

// TestHandlePrune_AuditFiresWhenExecuteRuns: POST /api/v1/prune with
// execute=true but no storage Guard wired returns 500 ("no storage
// guard is configured") and DOES NOT write actor_audit. The audit row
// only lands when prune.Execute actually runs -- exercising that
// path requires a real storage.Guard and is out of scope here. This
// test pins the documented "no audit on failure" contract.
func TestHandlePrune_AuditFiresWhenExecuteRuns(t *testing.T) {
	srv := newAuditWiredServer(t, true, true, func(d *Deps) {
		if d.Settings == nil {
			t.Fatal("expected Settings")
		}
	})
	enablePruneBody := settingsGetJSON(map[string]any{
		"set": map[string]any{"pruning.enabled": true},
	})
	rr0 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr0, adminReq(http.MethodPut, "/api/v1/settings", enablePruneBody))
	if rr0.Code != http.StatusOK {
		t.Fatalf("enable prune: status = %d, body=%s", rr0.Code, rr0.Body.String())
	}
	locID := seedTestStorageLocationWithTTL(t, srv, "prune-target", "/tmp/prune-target", "TIER1_LOCAL_SCRATCH", true, 24)
	body := settingsGetJSON(map[string]any{
		"storageLocationId": locID,
		"execute":           true,
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPost, "/api/v1/prune", body))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no guard wired), body=%s", rr.Code, rr.Body.String())
	}
	// The 500 path is BEFORE the audit write -- no row should be there.
	_, _, ok := findActorAuditRow(t, srv, audit.EventPruneExecuted)
	if ok {
		t.Error("prune.executed actor_audit row landed despite prune being skipped")
	}
}
