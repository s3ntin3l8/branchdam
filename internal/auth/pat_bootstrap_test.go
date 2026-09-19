package auth

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// newBootstrapTestDB opens a migrated SQLite database in a temp dir,
// for RunBootstrapPAT tests that need the real user_pats schema.
func newBootstrapTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(context.Background(), filepath.Join(root, "bootstrap.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, root
}

func TestRunBootstrapPAT_MintThenRefuseSecondRun(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// First run mints: plaintext returned, file carries it.
	plaintext, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err != nil {
		t.Fatalf("RunBootstrapPAT: %v", err)
	}
	if !strings.HasPrefix(plaintext, PATPrefix) {
		t.Fatalf("plaintext %q missing prefix %q", plaintext, PATPrefix)
	}
	path := filepath.Join(root, BootstrapPATFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bootstrap file: %v", err)
	}
	if strings.TrimSpace(string(content)) != plaintext {
		t.Fatalf("file content = %q, want the minted plaintext", strings.TrimSpace(string(content)))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat bootstrap file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}

	// Second run refuses with the consume-once sentinel error.
	if _, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log); !errors.Is(err, ErrBootstrapAlreadyMinted) {
		t.Fatalf("second run err = %v, want ErrBootstrapAlreadyMinted", err)
	}
}

func TestRunBootstrapPAT_EmptyEnvIsNoop(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	plaintext, err := RunBootstrapPAT(context.Background(), database, patTestPepper, "", root, log)
	if err != nil {
		t.Fatalf("RunBootstrapPAT with empty env: %v", err)
	}
	if plaintext != "" {
		t.Errorf("plaintext = %q, want empty", plaintext)
	}
	if _, err := os.Stat(filepath.Join(root, BootstrapPATFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("bootstrap file exists despite empty env (err=%v)", err)
	}
}

// TestRunBootstrapPAT_SymlinkRefused pins the TOCTOU finding: a
// symlink planted at the sentinel path must not be followed -- neither
// for the existence check nor for the plaintext write -- and must not
// be reported as "already minted".
func TestRunBootstrapPAT_SymlinkRefused(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	path := filepath.Join(root, BootstrapPATFileName)
	target := filepath.Join(root, "elsewhere.txt")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := RunBootstrapPAT(context.Background(), database, patTestPepper, "1", root, log)
	if err == nil {
		t.Fatal("RunBootstrapPAT must refuse a symlink at the sentinel path")
	}
	if errors.Is(err, ErrBootstrapAlreadyMinted) {
		t.Fatalf("symlink must not surface as ErrBootstrapAlreadyMinted: %v", err)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Error("symlink target was created -- the plaintext write followed the link")
	}
}

// TestRunBootstrapPAT_ClaimedFileRemovedOnDBFailure: when the DB half
// of the bootstrap fails, the claimed (empty) sentinel file must be
// removed -- otherwise the failure would block every future
// re-bootstrap while holding no token.
func TestRunBootstrapPAT_ClaimedFileRemovedOnDBFailure(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// A cancelled context makes the InTx begin fail.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err == nil {
		t.Fatal("RunBootstrapPAT with cancelled ctx must fail")
	}
	if _, statErr := os.Stat(filepath.Join(root, BootstrapPATFileName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("claim file must be removed after a failed boot (stat err=%v)", statErr)
	}
}

// TestRunBootstrapPAT_ZeroByteSentinelRecovered: a crash (SIGKILL,
// reboot) between the exclusive create and the DB commit skips the
// cleanup defer and leaves a zero-byte sentinel. The next boot must
// treat it as a crashed claim -- remove and re-mint -- not as
// "already minted", or one crash would permanently block bootstrap.
func TestRunBootstrapPAT_ZeroByteSentinelRecovered(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	path := filepath.Join(root, BootstrapPATFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed zero-byte sentinel: %v", err)
	}

	plaintext, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err != nil {
		t.Fatalf("zero-byte sentinel must be recovered, got: %v", err)
	}
	if !strings.HasPrefix(plaintext, PATPrefix) {
		t.Fatalf("plaintext %q missing prefix %q", plaintext, PATPrefix)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bootstrap file: %v", err)
	}
	if strings.TrimSpace(string(content)) != plaintext {
		t.Fatalf("file content = %q, want the minted plaintext", strings.TrimSpace(string(content)))
	}
}

// TestRunBootstrapPAT_AfterSystemSentinel is the round-3 regression:
// main.go runs EnsureSystemUser (email=”, source='forward-link')
// BEFORE the bootstrap block, and 00018's partial unique index
// users_email_source_uniq covers ” (only NULL is excluded). The
// bootstrap user must not collide with the sentinel pair or the boot
// dies with UNIQUE constraint violated.
func TestRunBootstrapPAT_AfterSystemSentinel(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.EnsureSystemUser(ctx)
		return err
	}); err != nil {
		t.Fatalf("ensure system user: %v", err)
	}

	plaintext, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err != nil {
		t.Fatalf("bootstrap after system sentinel must succeed, got: %v", err)
	}
	if !strings.HasPrefix(plaintext, PATPrefix) {
		t.Fatalf("plaintext %q missing prefix %q", plaintext, PATPrefix)
	}
}

// TestRunBootstrapPAT_ReactivatesDisabledOwner: the bootstrap block
// must leave its service account LIVE, not merely is_admin=1 -- the
// PAT lookup requires disabled_at IS NULL. Re-bootstrap after an
// operator disabled the account must produce a working token.
func TestRunBootstrapPAT_ReactivatesDisabledOwner(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Pre-existing, disabled bootstrap user (operator disabled the
	// service account; the sentinel file was deleted to re-bootstrap).
	var uid int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.EnsureBootstrapUser(ctx)
		if err != nil {
			return err
		}
		uid = id
		return q.PromoteUserToAdmin(ctx, id)
	}); err != nil {
		t.Fatalf("seed bootstrap user: %v", err)
	}
	// Simulate the round-3 probe: disable the account directly.
	now := sql.NullInt64{Int64: time.Now().Unix(), Valid: true}
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.DisableUser(ctx, sqlcgen.DisableUserParams{ID: uid, DisabledAt: now})
	}); err != nil {
		t.Fatalf("disable bootstrap user: %v", err)
	}

	plaintext, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err != nil {
		t.Fatalf("bootstrap with disabled owner must reactivate, got: %v", err)
	}
	// The minted token must actually authenticate: Lookup joins the
	// owner row and requires disabled_at IS NULL.
	if _, err := NewPATService(database, patTestPepper).Lookup(ctx, plaintext); err != nil {
		t.Fatalf("bootstrap token must authenticate after reactivation, Lookup: %v", err)
	}
}

// TestPATService_MintRequiresLiveAdminOwner pins the mint-path half of
// the live-authority model: the lookup query authenticates only
// live-admin-owned tokens, so minting for a non-admin, disabled, or
// row-less owner would return a token that 401s on first use. Mint
// must refuse with ErrPATOwnerNotAdmin instead.
func TestPATService_MintRequiresLiveAdminOwner(t *testing.T) {
	database, _ := newBootstrapTestDB(t)
	ctx := context.Background()
	svc := NewPATService(database, patTestPepper)

	// A user row that was never promoted.
	var plainUserID int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		uid, err := q.EnsureBootstrapUser(ctx)
		if err != nil {
			return err
		}
		plainUserID = uid
		return nil
	}); err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	if _, _, err := svc.Mint(ctx, plainUserID, "dead", []string{"*"}, 0); !errors.Is(err, ErrPATOwnerNotAdmin) {
		t.Fatalf("mint for non-admin owner err = %v, want ErrPATOwnerNotAdmin", err)
	}

	// Promote -> mint succeeds.
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.PromoteUserToAdmin(ctx, plainUserID)
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, _, err := svc.Mint(ctx, plainUserID, "live", []string{"*"}, 0); err != nil {
		t.Fatalf("mint for admin owner: %v", err)
	}

	// Demote again -> mint refuses again (the check is live, not
	// mint-history).
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.DemoteUserFromAdmin(ctx, plainUserID)
	}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if _, _, err := svc.Mint(ctx, plainUserID, "dead-again", []string{"*"}, 0); !errors.Is(err, ErrPATOwnerNotAdmin) {
		t.Fatalf("mint for demoted owner err = %v, want ErrPATOwnerNotAdmin", err)
	}

	// No users row at all -> same refusal.
	if _, _, err := svc.Mint(ctx, 999999, "no-row", []string{"*"}, 0); !errors.Is(err, ErrPATOwnerNotAdmin) {
		t.Fatalf("mint for row-less owner err = %v, want ErrPATOwnerNotAdmin", err)
	}
}

// TestPATService_MintRejectsUnknownScope pins the round-4 allowlist:
// patScopeFor only ever consults PATGrantableScopes, so a token
// carrying anything else would authenticate but pass no scope gate --
// a silent dead credential. Mint must refuse at the boundary.
func TestPATService_MintRejectsUnknownScope(t *testing.T) {
	database, _ := newBootstrapTestDB(t)
	ctx := context.Background()
	svc := NewPATService(database, patTestPepper)

	var uid int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.EnsureBootstrapUser(ctx)
		if err != nil {
			return err
		}
		uid = id
		return q.PromoteUserToAdmin(ctx, id)
	}); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}

	if _, _, err := svc.Mint(ctx, uid, "dead", []string{"settings:write"}, 0); !errors.Is(err, ErrPATInvalidScope) {
		t.Fatalf("mint with unknown scope err = %v, want ErrPATInvalidScope", err)
	}
	// An empty scope list mints a token that 403s on every route
	// group -- the silent-dead-credential shape. Reject it too.
	if _, _, err := svc.Mint(ctx, uid, "empty", nil, 0); !errors.Is(err, ErrPATInvalidScope) {
		t.Fatalf("mint with empty scopes err = %v, want ErrPATInvalidScope", err)
	}
	// Every allowlisted scope mints fine.
	for _, scope := range PATGrantableScopes {
		if _, _, err := svc.Mint(ctx, uid, "ok-"+scope, []string{scope}, 0); err != nil {
			t.Fatalf("mint with allowlisted scope %q: %v", scope, err)
		}
	}
}

// TestRunBootstrapPAT_BlocksWhileAnotherBootHoldsLock pins the flock
// serialization: the round-4 probe showed a live zero-byte claim being
// stolen by a concurrent boot's crashed-claim recovery. A second boot
// must block until the first releases the lock, then see the completed
// sentinel.
func TestRunBootstrapPAT_BlocksWhileAnotherBootHoldsLock(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	lockPath := filepath.Join(root, ".bootstrap-pat.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
		done <- err
	}()

	// While the lock is held, the bootstrap must not proceed.
	select {
	case err := <-done:
		t.Fatalf("RunBootstrapPAT completed while another boot holds the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunBootstrapPAT after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunBootstrapPAT did not proceed after the lock was released")
	}
}

// TestRunBootstrapPAT_LockTimeoutBoundsStartup: a peer boot wedged
// mid-sequence must fail this boot with a clear error after the
// bounded retry instead of parking startup forever.
func TestRunBootstrapPAT_LockTimeoutBoundsStartup(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	lockPath := filepath.Join(root, ".bootstrap-pat.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	prev := bootstrapLockTimeout
	bootstrapLockTimeout = 300 * time.Millisecond
	defer func() { bootstrapLockTimeout = prev }()

	start := time.Now()
	_, err = RunBootstrapPAT(context.Background(), database, patTestPepper, "1", root, log)
	if err == nil {
		t.Fatal("RunBootstrapPAT must fail when the lock cannot be acquired in time")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("lock wait was not bounded: %v", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a lock-timeout message", err)
	}
}

// TestRunBootstrapPAT_CrashRecoveryRevokesPriorRow: a boot killed
// between the commit and the file write leaves a live 'bootstrap'
// wildcard row whose plaintext never reached disk. The recovery boot's
// re-mint must revoke that prior row so live bootstrap tokens never
// accumulate.
func TestRunBootstrapPAT_CrashRecoveryRevokesPriorRow(t *testing.T) {
	database, root := newBootstrapTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewPATService(database, patTestPepper)

	// First boot mints token A.
	tokenA, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	// Simulate the crash-loss scenario: the sentinel disappears
	// (disk loss, operator deletion) while token A's row is still
	// live. (The revoked-vs-live state is what matters; deleting the
	// file here just lets the second boot run the mint path.)
	if err := os.Remove(filepath.Join(root, BootstrapPATFileName)); err != nil {
		t.Fatalf("remove sentinel: %v", err)
	}

	// Second boot re-mints (token B).
	tokenB, err := RunBootstrapPAT(ctx, database, patTestPepper, "1", root, log)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if tokenA == tokenB {
		t.Fatal("tokens must differ across re-mints")
	}
	// Token A (crashed boot) must have been revoked by the recovery
	// path; token B must authenticate.
	if _, err := svc.Lookup(ctx, tokenA); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Lookup(tokenA) err = %v, want sql.ErrNoRows (revoked by recovery)", err)
	}
	if _, err := svc.Lookup(ctx, tokenB); err != nil {
		t.Fatalf("Lookup(tokenB): %v", err)
	}
}
