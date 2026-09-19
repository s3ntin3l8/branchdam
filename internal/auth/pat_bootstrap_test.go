package auth

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/db"
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
