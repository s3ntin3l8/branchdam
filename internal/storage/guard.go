// Package storage is the single chokepoint every filesystem write in
// branchDAM must pass through. Tier 3 (the master archive) is mounted
// read-only in production, which surfaces as EROFS at whatever call depth
// first happens to touch it -- Guard turns that into a typed error, returned
// before any syscall, by resolving the write's target to a storage tier and
// checking its read-only flag first.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Location mirrors one row of storage_locations (internal/db). RootPath is
// always the fully symlink-resolved, absolute, no-trailing-slash form --
// see loadLocations and canonicalize.
type Location struct {
	ID       int64
	Name     string
	RootPath string
	Tier     string
	ReadOnly bool
}

// ErrReadOnlyTier is returned by every write method when the resolved
// location is read-only. Use errors.As to detect it.
type ErrReadOnlyTier struct {
	Path     string
	Location string
	Tier     string
}

func (e *ErrReadOnlyTier) Error() string {
	return fmt.Sprintf("storage: %q resolves to read-only location %q (tier %s)", e.Path, e.Location, e.Tier)
}

// ErrUnknownLocation is returned when a path doesn't fall under any
// configured storage location. Guard refuses these rather than guessing --
// an unrecognized path is refused, not assumed writable.
type ErrUnknownLocation struct {
	Path string
}

func (e *ErrUnknownLocation) Error() string {
	return fmt.Sprintf("storage: %q does not resolve under any configured storage location", e.Path)
}

// Guard resolves a filesystem path to its storage.Location and refuses
// writes against read-only tiers before any syscall happens. Every method
// that writes -- Create, WriteFile, MkdirAll, Remove -- calls CheckWrite
// first, unconditionally. OpenRead does not check: reads are always
// permitted, including against Tier 3.
type Guard struct {
	mu   sync.RWMutex
	locs []Location // sorted by len(RootPath) descending: longest-prefix-first match
}

// buildSortedLocations returns a copy of locs sorted by RootPath length
// descending (longest-prefix-first match). Extracted so both NewGuard and
// ReloadLocations can share the sorting logic without duplicating it.
func buildSortedLocations(locs []Location) []Location {
	sorted := make([]Location, len(locs))
	copy(sorted, locs)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].RootPath) > len(sorted[j].RootPath)
	})
	return sorted
}

// NewGuard builds a Guard directly from a list of locations, already
// resolved. Used by tests and by LoadGuard below; production startup should
// prefer LoadGuard so the locations come from the same storage_locations
// table storage.Guard is meant to be the single source of truth for
// (docs/schema.md fix #1).
func NewGuard(locs []Location) *Guard {
	return &Guard{locs: buildSortedLocations(locs)}
}

// ReloadLocations atomically replaces the Guard's location set. Called after
// a storage_locations row is updated in the database so the in-memory Guard
// reflects the new configuration without requiring a server restart.
func (g *Guard) ReloadLocations(locs []Location) {
	g.mu.Lock()
	g.locs = buildSortedLocations(locs)
	g.mu.Unlock()
}

// locationLister is the subset of sqlcgen.Querier LoadGuard needs. Declared
// here (not imported from internal/db/sqlcgen) so this package doesn't take
// a hard dependency on the generated code merely to load its own config --
// any caller with a compatible method satisfies it, including a fake in
// tests that never touches a real database.
type locationLister interface {
	ListStorageLocations(ctx context.Context) ([]StorageLocationRow, error)
}

// StorageLocationRow is the shape LoadGuard needs from each storage_locations
// row. internal/db's caller adapts sqlcgen.StorageLocation into this with a
// small conversion -- see cmd/branchdam (wired when a consumer needs it).
type StorageLocationRow struct {
	ID       int64
	Name     string
	RootPath string
	Tier     string
	ReadOnly bool
}

// LoadGuard reads storage_locations via lister and canonicalizes every
// RootPath with EvalSymlinks. Root paths are operator-configured mount
// points and are normally expected to exist at startup, but a single
// missing or unresolvable root (M6: an unplugged drive, a dead NFS/SMB
// mount) is NOT treated as fatal -- that would brick the entire server,
// including the UI an operator would use to diagnose it, over one bad
// mount out of potentially several. That location is instead excluded from
// the returned Guard (so no write can ever be routed to a path Guard
// cannot actually protect) and its id is returned in skippedLocationIDs
// for the caller to mark inactive (cmd/branchdam calls
// sqlcgen.SetStorageLocationActive) -- LoadGuard itself only reads via
// lister (this package has no dependency on internal/db/sqlcgen at all),
// so it cannot perform that write itself. A lister-level failure (the
// ListStorageLocations call itself erroring) is still fatal: that means
// the database is unreachable, not that one mount is unavailable.
func LoadGuard(ctx context.Context, lister locationLister, log *slog.Logger) (*Guard, []int64, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rows, err := lister.ListStorageLocations(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list storage locations: %w", err)
	}

	locs := make([]Location, 0, len(rows))
	var skipped []int64
	for _, row := range rows {
		resolved, err := filepath.EvalSymlinks(row.RootPath)
		if err != nil {
			log.Error("storage: location root_path unresolvable, excluding from Guard and marking inactive",
				"location", row.Name, "rootPath", row.RootPath, "err", err)
			skipped = append(skipped, row.ID)
			continue
		}
		locs = append(locs, Location{
			ID:       row.ID,
			Name:     row.Name,
			RootPath: filepath.Clean(resolved),
			Tier:     row.Tier,
			ReadOnly: row.ReadOnly,
		})
	}
	return NewGuard(locs), skipped, nil
}

// Resolve canonicalizes path and returns the Location it falls under.
// Symlinks anywhere in path's existing ancestry are fully resolved before
// the tier lookup runs -- a symlink sitting in a read-write Tier 2 directory
// that points into the Tier 3 archive is resolved to its real target first,
// so it cannot be used to route a write around the tier check.
func (g *Guard) Resolve(path string) (Location, error) {
	canon, err := canonicalize(path)
	if err != nil {
		return Location{}, fmt.Errorf("storage: resolve %q: %w", path, err)
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, loc := range g.locs {
		if canon == loc.RootPath || strings.HasPrefix(canon, loc.RootPath+string(filepath.Separator)) {
			return loc, nil
		}
	}
	return Location{}, &ErrUnknownLocation{Path: path}
}

// CheckWrite returns *ErrReadOnlyTier if path resolves to a read-only
// location, or *ErrUnknownLocation if it resolves to none. Returns nil only
// when the write is permitted. Call this before any write; Create,
// WriteFile, MkdirAll and Remove already do.
func (g *Guard) CheckWrite(path string) error {
	loc, err := g.Resolve(path)
	if err != nil {
		return err
	}
	if loc.ReadOnly {
		return &ErrReadOnlyTier{Path: path, Location: loc.Name, Tier: loc.Tier}
	}
	return nil
}

// Create is os.Create, gated by CheckWrite. No file is created if the check
// fails.
func (g *Guard) Create(path string) (*os.File, error) {
	if err := g.CheckWrite(path); err != nil {
		return nil, err
	}
	return os.Create(path)
}

// WriteFile is os.WriteFile, gated by CheckWrite.
func (g *Guard) WriteFile(path string, data []byte, perm fs.FileMode) error {
	if err := g.CheckWrite(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

// MkdirAll is os.MkdirAll, gated by CheckWrite against the target directory
// itself (not each intermediate component -- if the deepest directory is
// writable, so is everything MkdirAll would create above it, since they all
// resolve under the same location).
func (g *Guard) MkdirAll(path string, perm fs.FileMode) error {
	if err := g.CheckWrite(path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

// Remove is os.Remove, gated by CheckWrite.
func (g *Guard) Remove(path string) error {
	if err := g.CheckWrite(path); err != nil {
		return err
	}
	return os.Remove(path)
}

// OpenRead is os.Open. Reads are always permitted, including against Tier 3
// -- the archive is read-only, not unreadable.
func (g *Guard) OpenRead(path string) (*os.File, error) {
	return os.Open(path)
}

// Exists reports whether path is already present on disk, using the same
// symlink-safe canonicalization Resolve uses so a not-yet-existing suffix
// can't be spoofed via a symlink planted in a writable location. A pure
// stat, never a write -- permitted against any tier, including Tier 3,
// mirroring OpenRead. This is what lets a caller distinguish "the bytes are
// already at this Tier 3 path" (safe to record in the database) from "they
// are not" (nothing else will ever place them there, since Tier 3 is
// read-only) without ever attempting a write against the archive itself.
//
// Deliberately os.Stat, not os.Lstat: canonicalize only resolves symlinks
// up to the deepest EXISTING ancestor, so for a DANGLING symlink at the
// leaf position (the link itself exists on disk, its target does not),
// EvalSymlinks fails to resolve the target and canonicalize rejoins the
// leaf name literally -- canon ends up pointing at the symlink itself, not
// through it. Lstat would then report the dangling link as "existing" even
// though zero real bytes are present at the target, defeating the entire
// point of this check. Stat follows the symlink and correctly reports
// absent when the target doesn't resolve.
func (g *Guard) Exists(path string) (bool, error) {
	canon, err := canonicalize(path)
	if err != nil {
		return false, fmt.Errorf("storage: resolve %q: %w", path, err)
	}
	if _, err := os.Stat(canon); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Locations returns a copy of all configured locations in the Guard.
func (g *Guard) Locations() []Location {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Location, len(g.locs))
	copy(out, g.locs)
	return out
}

// canonicalize resolves path to its real, symlink-free form. path need not
// exist: canonicalize walks up to the deepest existing ancestor, resolves
// that ancestor fully (EvalSymlinks), and rejoins the non-existent suffix
// components literally -- they can't be symlinks if they don't exist yet.
// A path that cannot be canonicalized at all (e.g. no existing ancestor
// short of an unreadable point) is rejected, never passed through as-is.
func canonicalize(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute: %s", path)
	}

	var suffix []string
	cur := path
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			full := resolved
			for i := len(suffix) - 1; i >= 0; i-- {
				full = filepath.Join(full, suffix[i])
			}
			return filepath.Clean(full), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no existing ancestor found while resolving %q", path)
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}
