package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/settings"
)

// startupWarnings builds a Server over the given auth mode and returns
// everything the startup path logged.
//
// This is the switch-level test: it hands `New` a configuration directly,
// so it fails if the case arm is gone. The store round-trip -- how the
// empty mode actually reaches the switch in the running process -- is
// covered by startupWarningsThroughStore below.
func startupWarnings(t *testing.T, authMode string) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{}
	cfg.Auth.Mode = authMode
	New(Deps{Config: cfg, Log: logger})
	return buf.String()
}

// startupWarningsThroughStore builds the Server the way the process does:
// over a settings store that resolved the base configuration.
//
// The forward-mode warning is a startup-time operator signal, so it has to
// be asserted where it is emitted: New reads the EFFECTIVE config (the one
// the settings store resolves), not the file on disk.
//
// The regression this pins is not only "the switch has no empty case" --
// it is that the empty mode REACHES the switch. cmd/branchdam normalises
// the mode on its own copy of the configuration (cmd/branchdam/main.go),
// but the settings store snapshot the base configuration when it is
// constructed, so Effective() still carries the empty string and the
// server is handed a configuration that says "forward" in every way but
// in writing. A test that builds the config by hand assumes all of that
// away; this one goes through the store and reads Effective() back.
func startupWarningsThroughStore(t *testing.T, baseMode string) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "startup.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Closed before the test returns: the temporary directory's cleanup
	// removes the database file, and an open handle makes that fail on
	// Windows.
	defer func() { _ = database.Close() }()
	base := config.Config{}
	base.Auth.Mode = baseMode
	store, err := settings.NewStore(context.Background(), database, base, nil, logger)
	if err != nil {
		t.Fatalf("new settings store: %v", err)
	}
	// The store resolved the base configuration; the empty mode is what
	// the server will see.
	if got := store.Effective().Auth.Mode; got != baseMode {
		t.Fatalf("store effective auth.mode = %q, want %q -- the test would not exercise the empty mode", got, baseMode)
	}
	New(Deps{Settings: store, Log: logger})
	return buf.String()
}

// TestStartupWarnsForForwardModeAndEmptyMode pins the forward-mode
// startup warning.
//
// Mode "forward" does not check the peer against http.trustedProxies, so
// the forwarded identity headers are honoured from whoever can reach the
// port. That is the documented contract, but it is a full bypass the
// moment the port is reachable directly, and nothing said so.
//
// The EMPTY mode is the same case: docs/configuration.md documents it as
// "treated as forward", and cmd/branchdam normalises the mode on its own
// copy of the config, but the settings store snapshot the base config
// before that -- so the empty string reaches the startup switch unchanged,
// and the configuration that most needed the warning (one that never set
// auth.mode at all) got none.
func TestStartupWarnsForForwardModeAndEmptyMode(t *testing.T) {
	const forwardMarker = "forward-auth identity headers are honoured from ANY peer"

	for _, mode := range []string{"forward", ""} {
		out := startupWarnings(t, mode)
		if !strings.Contains(out, forwardMarker) {
			t.Errorf("auth.mode %q: no forward-mode startup warning; log was:\n%s", mode, out)
		}
	}
}

// TestStartupWarnsForTheEmptyModeTheStoreResolves is the same warning,
// reached the way the process reaches it: through the settings store,
// which still carries the empty mode after cmd/branchdam normalised its
// own copy.
func TestStartupWarnsForTheEmptyModeTheStoreResolves(t *testing.T) {
	const forwardMarker = "forward-auth identity headers are honoured from ANY peer"
	out := startupWarningsThroughStore(t, "")
	if !strings.Contains(out, forwardMarker) {
		t.Errorf("auth.mode \"\" (through the settings store): no forward-mode startup warning; log was:\n%s", out)
	}
}

// TestStartupDoesNotWarnForLocalMode is the counterpart: mode "local"
// runs the session-cookie chain only, so the forward-mode warning would
// be wrong there.
func TestStartupDoesNotWarnForLocalMode(t *testing.T) {
	const forwardMarker = "forward-auth identity headers are honoured from ANY peer"
	out := startupWarnings(t, "local")
	if strings.Contains(out, forwardMarker) {
		t.Errorf("auth.mode \"local\": forward-mode warning fired; log was:\n%s", out)
	}
}
