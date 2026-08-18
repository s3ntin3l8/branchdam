package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestGuard builds a Guard over two real directories under t.TempDir():
// tier2 (read-write) and tier3 (read-only), mirroring the production shape
// where the master archive is mounted :ro alongside a writable exports tier.
func newTestGuard(t *testing.T) (guard *Guard, tier2, tier3 string) {
	t.Helper()
	root := t.TempDir()
	tier2 = filepath.Join(root, "exports")
	tier3 = filepath.Join(root, "archive")
	for _, d := range []string{tier2, tier3} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Resolve /tmp itself in case it's a symlink (macOS: /tmp -> /private/tmp)
	// so RootPath comparisons in the test match what canonicalize() produces.
	resolvedTier2, err := filepath.EvalSymlinks(tier2)
	if err != nil {
		t.Fatalf("resolve tier2: %v", err)
	}
	resolvedTier3, err := filepath.EvalSymlinks(tier3)
	if err != nil {
		t.Fatalf("resolve tier3: %v", err)
	}
	guard = NewGuard([]Location{
		{ID: 1, Name: "exports", RootPath: resolvedTier2, Tier: "TIER2_EXPORTS", ReadOnly: false},
		{ID: 2, Name: "archive", RootPath: resolvedTier3, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})
	return guard, resolvedTier2, resolvedTier3
}

// TestCreateOnReadOnlyTierRefused is T4: the write is refused with a typed
// error before any syscall, and no file is created.
func TestCreateOnReadOnlyTierRefused(t *testing.T) {
	guard, _, tier3 := newTestGuard(t)
	target := filepath.Join(tier3, "should-not-exist.txt")

	f, err := guard.Create(target)
	if err == nil {
		_ = f.Close()
		t.Fatal("Create on read-only tier succeeded, want *ErrReadOnlyTier")
	}
	var roErr *ErrReadOnlyTier
	if !errors.As(err, &roErr) {
		t.Fatalf("error = %v (%T), want *ErrReadOnlyTier", err, err)
	}
	if roErr.Tier != "TIER3_MASTER_ARCHIVE" {
		t.Errorf("ErrReadOnlyTier.Tier = %q, want TIER3_MASTER_ARCHIVE", roErr.Tier)
	}

	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("file exists after refused Create (stat err = %v), want it absent", statErr)
	}
}

// TestSymlinkEscapeRefused is the case a naive strings.HasPrefix check
// would miss: a symlink sitting inside the writable Tier 2 directory whose
// target lives inside the read-only Tier 3 directory. canonicalize must
// resolve the symlink to its real Tier 3 location before the tier check
// runs, so the write is refused exactly like a direct Tier 3 write.
func TestSymlinkEscapeRefused(t *testing.T) {
	guard, tier2, tier3 := newTestGuard(t)

	linkPath := filepath.Join(tier2, "escape-hatch")
	if err := os.Symlink(tier3, linkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	target := filepath.Join(linkPath, "smuggled.txt")
	err := guard.WriteFile(target, []byte("data"), 0o644)
	if err == nil {
		t.Fatal("WriteFile through a symlink into Tier 3 succeeded, want *ErrReadOnlyTier")
	}
	var roErr *ErrReadOnlyTier
	if !errors.As(err, &roErr) {
		t.Fatalf("error = %v (%T), want *ErrReadOnlyTier", err, err)
	}

	if _, statErr := os.Stat(filepath.Join(tier3, "smuggled.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("file smuggled into tier3 (stat err = %v), want it absent", statErr)
	}
}

// TestOpenReadAlwaysAllowed proves Tier 3 is read-only, not unreadable.
func TestOpenReadAlwaysAllowed(t *testing.T) {
	guard, _, tier3 := newTestGuard(t)
	existing := filepath.Join(tier3, "master.arw")
	if err := os.WriteFile(existing, []byte("raw bytes"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	f, err := guard.OpenRead(existing)
	if err != nil {
		t.Fatalf("OpenRead on Tier 3: %v", err)
	}
	defer func() { _ = f.Close() }()

	got := make([]byte, 9)
	if _, err := f.Read(got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "raw bytes" {
		t.Errorf("read %q, want %q", got, "raw bytes")
	}
}

// TestWriteOnWritableTierSucceeds is the control case: Guard must not
// refuse legitimate writes.
func TestWriteOnWritableTierSucceeds(t *testing.T) {
	guard, tier2, _ := newTestGuard(t)
	target := filepath.Join(tier2, "render.jpg")

	if err := guard.WriteFile(target, []byte("jpeg"), 0o644); err != nil {
		t.Fatalf("WriteFile on writable tier: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "jpeg" {
		t.Errorf("content = %q, want jpeg", got)
	}
}

// TestMkdirAllAndRemoveRespectGuard covers the remaining write methods --
// same gate, same typed error, on both the permit and refuse paths.
func TestMkdirAllAndRemoveRespectGuard(t *testing.T) {
	guard, tier2, tier3 := newTestGuard(t)

	if err := guard.MkdirAll(filepath.Join(tier2, "proxies", "2026"), 0o755); err != nil {
		t.Fatalf("MkdirAll on writable tier: %v", err)
	}
	if err := guard.MkdirAll(filepath.Join(tier3, "nope"), 0o755); err == nil {
		t.Fatal("MkdirAll on read-only tier succeeded, want an error")
	}

	toRemove := filepath.Join(tier2, "scratch.txt")
	if err := os.WriteFile(toRemove, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	if err := guard.Remove(toRemove); err != nil {
		t.Fatalf("Remove on writable tier: %v", err)
	}

	protected := filepath.Join(tier3, "master.arw")
	if err := os.WriteFile(protected, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	if err := guard.Remove(protected); err == nil {
		t.Fatal("Remove on read-only tier succeeded, want an error")
	}
	if _, statErr := os.Stat(protected); statErr != nil {
		t.Errorf("protected file was removed despite refusal: %v", statErr)
	}
}

// TestUnknownLocationRefused: a path under no configured location is
// refused, never assumed writable.
func TestUnknownLocationRefused(t *testing.T) {
	guard, _, _ := newTestGuard(t)
	elsewhere := filepath.Join(t.TempDir(), "not-configured.txt")

	err := guard.CheckWrite(elsewhere)
	if err == nil {
		t.Fatal("CheckWrite on an unconfigured path succeeded, want *ErrUnknownLocation")
	}
	var unkErr *ErrUnknownLocation
	if !errors.As(err, &unkErr) {
		t.Fatalf("error = %v (%T), want *ErrUnknownLocation", err, err)
	}
}

// TestResolveRejectsRelativePaths guards against a caller accidentally
// passing a path relative to the process's CWD, which canonicalize cannot
// safely reason about relative to configured (absolute) storage roots.
func TestResolveRejectsRelativePaths(t *testing.T) {
	guard, _, _ := newTestGuard(t)
	if _, err := guard.Resolve("relative/path.txt"); err == nil {
		t.Fatal("Resolve accepted a relative path, want an error")
	}
}

type fakeLister struct {
	rows []StorageLocationRow
	err  error
}

func (f *fakeLister) ListStorageLocations(_ context.Context) ([]StorageLocationRow, error) {
	return f.rows, f.err
}

// TestLoadGuardResolvesRootPaths proves LoadGuard canonicalizes each
// configured root_path at load time (not lazily per-request), so a
// misconfigured or missing mount is a loud startup failure.
func TestLoadGuardResolvesRootPaths(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve fixture dir: %v", err)
	}

	guard, skipped, err := LoadGuard(context.Background(), &fakeLister{rows: []StorageLocationRow{
		{ID: 1, Name: "archive", RootPath: dir, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	}}, nil)
	if err != nil {
		t.Fatalf("LoadGuard: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none (root_path resolves)", skipped)
	}

	loc, err := guard.Resolve(filepath.Join(dir, "x.arw"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if loc.RootPath != resolved {
		t.Errorf("RootPath = %q, want canonicalized %q", loc.RootPath, resolved)
	}
}

// TestLoadGuardSkipsUnresolvableRootInsteadOfFailing backs M6: a single
// vanished mount (unplugged drive, dead NFS/SMB share) must not brick the
// whole server at startup -- LoadGuard excludes that location from the
// returned Guard and reports its id via skippedLocationIDs instead of
// erroring, so cmd/branchdam can still boot (and mark the location
// inactive) with every OTHER location fully functional.
func TestLoadGuardSkipsUnresolvableRootInsteadOfFailing(t *testing.T) {
	dir := t.TempDir()
	guard, skipped, err := LoadGuard(context.Background(), &fakeLister{rows: []StorageLocationRow{
		{ID: 1, Name: "vanished", RootPath: "/does/not/exist/anywhere", Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
		{ID: 2, Name: "healthy", RootPath: dir, Tier: "TIER2_EXPORTS", ReadOnly: false},
	}}, nil)
	if err != nil {
		t.Fatalf("LoadGuard with one unresolvable root_path returned an error, want nil (M6): %v", err)
	}
	if len(skipped) != 1 || skipped[0] != 1 {
		t.Errorf("skipped = %v, want [1]", skipped)
	}

	if _, err := guard.Resolve(dir); err != nil {
		t.Errorf("Resolve on the healthy location failed: %v -- one bad mount must not affect the others", err)
	}
	if _, err := guard.Resolve("/does/not/exist/anywhere/x.arw"); err == nil {
		t.Error("Resolve on the excluded location succeeded, want ErrUnknownLocation -- it must not be part of the Guard")
	}
}

// TestLoadGuardFailsLoudlyOnListerError is the one failure mode LoadGuard
// still treats as fatal: the lister call itself erroring means the
// database is unreachable, not that a single mount is unavailable -- see
// LoadGuard's doc comment.
func TestLoadGuardFailsLoudlyOnListerError(t *testing.T) {
	_, _, err := LoadGuard(context.Background(), &fakeLister{err: errors.New("db unreachable")}, nil)
	if err == nil {
		t.Fatal("LoadGuard with a failing lister succeeded, want an error")
	}
}
