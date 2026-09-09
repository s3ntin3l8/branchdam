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
