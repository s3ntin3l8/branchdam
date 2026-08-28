package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
)

func TestPostRestartRequiresAdmin(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	srv.requestRestart = func() {}

	t.Run("unauthenticated forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/restart", nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
		}
	})

	t.Run("non-admin forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/restart", nil)
		req.Header.Set("X-Authentik-Username", "alice")
		req.Header.Set("X-Authentik-Groups", "dam-users")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
		}
	})
}

// TestPostRestartRejectsMachinePrincipal pins requireSettingsAdmin's
// KindMachine == forbidden branch through this route specifically -- an
// agent holding only its API key must never be able to bounce the process,
// the same guarantee requireSettingsAdmin already gives GET/PUT
// /api/v1/settings and PUT /api/v1/storage-locations/{id}.
func TestPostRestartRejectsMachinePrincipal(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "restart-machine.db"))
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
	fired := false
	srv := New(Deps{
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		RequestRestart: func() { fired = true },
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/restart", nil)
	req.Header.Set("X-API-Key", "01234567890123456789012345678901")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
	if fired {
		t.Error("RequestRestart fired for a machine principal")
	}
}

// TestPostRestartReturns503WithoutHook pins the "this deployment didn't
// wire a restart mechanism" case: every existing test-constructed Server,
// and any Server built without Deps.RequestRestart, has a nil
// requestRestart -- the route must say so explicitly (503) rather than
// silently reporting success and doing nothing.
func TestPostRestartReturns503WithoutHook(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPost, "/api/v1/restart", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
}

// TestPostRestartRespondsBeforeFiringHook is the regression test for the
// self-wait hazard restart.go's doc comment describes: httpServer.Shutdown
// (which a real RequestRestart hook triggers, indirectly, via the same
// stop() SIGTERM uses) blocks on in-flight requests, so firing the hook
// synchronously from inside the handler would make Shutdown wait on the
// very request that asked for it. Asserts the handler returns success
// while the hook is still unfired, and that it fires shortly after.
func TestPostRestartRespondsBeforeFiringHook(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	fired := make(chan struct{})
	srv.requestRestart = func() { close(fired) }

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPost, "/api/v1/restart", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if !decodePutStorageLocationBody(t, rr).OK {
		t.Fatal("response ok = false")
	}

	select {
	case <-fired:
		t.Fatal("RequestRestart fired before the handler's response was written")
	default:
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestRestart never fired")
	}
}
