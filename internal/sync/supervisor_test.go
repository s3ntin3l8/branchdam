package sync

import (
	"context"
	"log/slog"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

// These three mirror the disabled/enabled cases cmd/branchdam's
// TestStartImmichWorkerDisabledWhenNotConfigured/
// TestStartImmichWorkerDisabledWhenLibraryIDMissing/
// TestStartImmichWorkerEnabledWhenConfigured covered before that logic
// moved from main.go's startImmichWorker into Supervisor.startLocked.

func TestSupervisorStartDisabledWhenAPIURLNotConfigured(t *testing.T) {
	for name, apiURL := range map[string]string{
		"empty":                  "",
		"unresolved placeholder": "${IMMICH_API_URL}", // unset env left literal by expandEnv
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
			sv.Start(ctx, &config.Config{Immich: config.Immich{APIURL: apiURL}})
			if sv.worker != nil {
				t.Fatalf("Start with apiUrl %q started a worker, want none", apiURL)
			}
		})
	}
}

func TestSupervisorStartDisabledWhenLibraryIDMissing(t *testing.T) {
	for name, libraryID := range map[string]string{
		"empty":                  "",
		"unresolved placeholder": "${IMMICH_LIBRARY_ID}",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
			cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: libraryID, ExportPath: "/e"}}
			sv.Start(ctx, cfg)
			if sv.worker != nil {
				t.Fatalf("Start with libraryId %q started a worker, want none (an empty libraryId would 404 every push forever, see #182)", libraryID)
			}
		})
	}
}

func TestSupervisorStartEnabledWhenConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	sv.Start(ctx, cfg)
	if sv.worker == nil {
		t.Fatal("Start with APIURL set started no worker")
	}
	cancel()
	sv.Wait()
}

func TestSupervisorReloadNoopWhenImmichFieldsUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	sv.Start(ctx, cfg)
	original := sv.worker
	if original == nil {
		t.Fatal("Start with APIURL set started no worker")
	}

	// A settings write that changes something unrelated (e.g. logLevel)
	// must not tear down and restart the Immich worker.
	changed := *cfg
	changed.LogLevel = "debug"
	sv.Reload(&changed)

	if sv.worker != original {
		t.Fatal("Reload restarted the worker even though no Immich field changed")
	}

	// The no-op path leaves the original worker running -- join it before
	// openTestDB's t.Cleanup closes the database out from under it.
	cancel()
	sv.Wait()
}

// TestSupervisorReloadNoopWhenExportPathOnlyTrailingSlashDiffers pins a
// Hermes review fix on PR #280: immichParamsFrom must normalize exportPath
// the same way startLocked does (TrimRight "/"), or a purely cosmetic edit
// (adding/removing a trailing slash) reads as a real change and bounces
// the worker for no behavioral difference.
func TestSupervisorReloadNoopWhenExportPathOnlyTrailingSlashDiffers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	sv.Start(ctx, cfg)
	original := sv.worker
	if original == nil {
		t.Fatal("Start with APIURL set started no worker")
	}

	changed := *cfg
	changed.Immich.ExportPath = "/e/"
	sv.Reload(&changed)

	if sv.worker != original {
		t.Fatal("Reload restarted the worker over a trailing-slash-only exportPath edit, want a no-op")
	}

	cancel()
	sv.Wait()
}

func TestSupervisorReloadRestartsWorkerWhenImmichFieldsChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	sv.Start(ctx, cfg)
	original := sv.worker
	if original == nil {
		t.Fatal("Start with APIURL set started no worker")
	}

	changed := *cfg
	changed.Immich.APIURL = "http://immich.example:2283"
	sv.Reload(&changed)

	if sv.worker == nil {
		t.Fatal("Reload with a changed apiUrl left no worker running")
	}
	if sv.worker == original {
		t.Fatal("Reload with a changed apiUrl kept the same worker instance, want a fresh one built from the new client config")
	}

	// stopLocked already joined `original` synchronously inside Reload, but
	// the replacement worker it started is still running -- join it too
	// before openTestDB's t.Cleanup closes the database.
	cancel()
	sv.Wait()
}

func TestSupervisorReloadStopsWorkerWhenImmichBecomesUnconfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	sv.Start(ctx, cfg)
	if sv.worker == nil {
		t.Fatal("Start with APIURL set started no worker")
	}

	cleared := *cfg
	cleared.Immich.APIURL = ""
	sv.Reload(&cleared)

	// Reload's stopLocked already joined the old worker synchronously
	// before returning, and startLocked never started a replacement for an
	// empty apiUrl -- nothing left running, so sv.Wait() here is only for
	// symmetry with the other cases, not a fix for a leak.
	if sv.worker != nil {
		t.Fatal("Reload clearing apiUrl left a worker running, want it stopped")
	}
	sv.Wait()
}

// TestSupervisorReloadRefusesToStartAfterShutdown pins the fix for a real
// hazard: Reload is driven by an in-flight PUT /api/v1/settings request,
// and cmd/branchdam's run() shutdown sequence can have httpServer.Shutdown
// return (its own deadline firing) while such a request is still being
// handled. If Reload started a fresh worker at that point, it could still
// be running (and writing to the database via RecoverStalePushing or a
// push) after run()'s join sequence already called Wait() and moved on to
// pool.Drain()/db.Close() -- exactly the writer-after-close hazard
// dbUnsafeToClose exists to prevent.
func TestSupervisorReloadRefusesToStartAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}
	sv.Start(ctx, cfg)
	if sv.worker == nil {
		t.Fatal("Start with APIURL set started no worker")
	}

	// Simulate process shutdown: cancel the root context (as main.go's
	// signal.NotifyContext does) and join whatever Start spawned, exactly
	// as run()'s join sequence would before moving on to db.Close().
	cancel()
	sv.Wait()

	// A settings write racing the tail end of shutdown must not spin up a
	// new worker now that the root context is done.
	changed := *cfg
	changed.Immich.APIURL = "http://immich.example:2283"
	sv.Reload(&changed)

	if sv.worker != nil {
		t.Fatal("Reload started a new worker after the root context was cancelled, want none (shutdown in progress)")
	}
}

func TestSupervisorReloadNoopBeforeStart(t *testing.T) {
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich:2283", APIKey: "k", LibraryID: "lib", ExportPath: "/e"}}

	sv.Reload(cfg) // must not panic despite Start never having been called

	if sv.worker != nil {
		t.Fatal("Reload before Start started a worker")
	}
}

func TestSupervisorWaitSafeWhenNoWorkerRunning(t *testing.T) {
	sv := NewSupervisor(openTestDB(t), slog.New(slog.DiscardHandler))
	sv.Wait() // must return immediately, not block
}
