package httpapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

type PutStorageLocationInput struct {
	ID   int64 `path:"id"`
	Body struct {
		Set   map[string]any `json:"set,omitempty"`
		Unset []string       `json:"unset,omitempty"`
	}
}

type PutStorageLocationOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

// normalizeStorageLocationValue converts a JSON-decoded value (numbers
// always arrive as float64, regardless of the field's declared shape) into
// the Go type settings.ApplyStorageLocationOverride expects to encode --
// the same wire-format boundary normalizeSettingValue (settings.go) keeps
// out of internal/settings itself.
func normalizeStorageLocationValue(field string, v any) (any, error) {
	switch field {
	case "name":
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("must be a non-empty string")
		}
		return s, nil
	case "watch", "sweep", "enabled":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be a boolean")
		}
		return b, nil
	case "sweepIntervalSecs", "cacheTtlHours":
		n, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		if n < 0 {
			return nil, fmt.Errorf("must be >= 0")
		}
		return int(n), nil
	default:
		return nil, fmt.Errorf("unknown field %q", field)
	}
}

// errLastEnabledStorageLocation is handlePutStorageLocation's sentinel for
// the last-enabled guard tripping inside its transaction -- distinguished
// from a genuine write failure so the caller can map it to a 422 instead of
// a 500.
var errLastEnabledStorageLocation = errors.New("cannot disable the last enabled storage location")

// countEnabledStorageLocationsTx reports how many rootPaths currently in
// config.yaml's effective storage location list (excluding excludeRootPath,
// the location being edited) have no "enabled": false override -- the
// last-enabled guard's data source. Deliberately walks s.cfg().StorageLocations,
// not the storage_locations table: a DB row can outlive its rootPath being
// removed from config.yaml (deactivated via DeactivateStorageLocationsNotIn,
// but never deleted -- see the schema's "rows are never deleted" invariant),
// and such an orphaned row has no override either, so counting DB rows would
// let it silently stand in as "still enabled" and permit disabling the only
// location config.yaml actually still lists.
//
// Takes q, not s.db.Reader: handlePutStorageLocation runs this count and its
// own write inside the same db.InTx block specifically so two concurrent
// "disable a different location" requests can't both observe n>=1 against
// the read pool and both proceed, leaving zero enabled locations -- the
// writer's single connection (db.DB's SetMaxOpenConns(1), see AGENTS.md)
// serializes InTx bodies, so reading via the transaction's own q closes that
// TOCTOU window instead of merely narrowing it.
func (s *Server) countEnabledStorageLocationsTx(ctx context.Context, q *sqlcgen.Queries, excludeRootPath string) (int, error) {
	overrides, err := settings.LoadStorageLocationOverrides(ctx, q)
	if err != nil {
		return 0, err
	}
	cfg := s.cfg()
	if cfg == nil {
		return 0, nil
	}
	n := 0
	for _, loc := range cfg.StorageLocations {
		if loc.RootPath == excludeRootPath {
			continue
		}
		if ov, ok := overrides[loc.RootPath]; ok && ov.Enabled != nil && !*ov.Enabled {
			continue
		}
		n++
	}
	return n, nil
}

func (s *Server) handlePutStorageLocation(ctx context.Context, in *PutStorageLocationInput) (*PutStorageLocationOutput, error) {
	if err := s.requireSettingsAdmin(ctx); err != nil {
		return nil, err
	}

	loc, err := s.db.Reader.GetStorageLocationByID(ctx, in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("storage location not found")
	}

	set := make(map[string]any, len(in.Body.Set))
	for key, raw := range in.Body.Set {
		if !settings.IsStorageLocationField(key) {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("unknown field %q", key))
		}
		norm, err := normalizeStorageLocationValue(key, raw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("field %q: %v", key, err))
		}
		set[key] = norm
	}
	for _, key := range in.Body.Unset {
		if !settings.IsStorageLocationField(key) {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("unknown field %q", key))
		}
	}

	// Guard: cacheTtlHours > 0 is only meaningful on a prunable location --
	// mirrors validatePruneConfig's fatal-at-startup rule (prunable is
	// config-only, known here from the DB row), surfaced as a 422 instead
	// of bricking a future boot with an override that was valid when
	// written but never re-checked.
	if v, ok := set["cacheTtlHours"]; ok && v.(int) > 0 && loc.Prunable == 0 {
		return nil, huma.Error422UnprocessableEntity("cacheTtlHours must be 0 on a non-prunable location")
	}

	// Guard: refuse to disable the last enabled location. Not a data-safety
	// backstop -- seedStorageLocations already no-ops on an empty list
	// rather than mass-deactivating everything (its own len==0 early
	// return closes the json_each('[]') hazard upstream of this) -- but a
	// config where nothing is watched, nothing is swept, and nothing
	// self-heals is a coherence trap worth refusing outright rather than
	// letting an operator paint themselves into it from the UI. The count
	// and the write below run inside one transaction (not two separate
	// calls) so two concurrent disable requests for different locations
	// can't both pass the guard before either commits -- see
	// countEnabledStorageLocationsTx's doc comment.
	actor := principalName(ctx)
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		if v, ok := set["enabled"]; ok && !v.(bool) {
			n, err := s.countEnabledStorageLocationsTx(ctx, q, loc.RootPath)
			if err != nil {
				return err
			}
			if n == 0 {
				return errLastEnabledStorageLocation
			}
		}
		return settings.ApplyStorageLocationOverrideTx(ctx, q, loc.RootPath, set, in.Body.Unset, actor)
	})
	if errors.Is(err, errLastEnabledStorageLocation) {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("apply storage location override", err)
	}

	if err := s.reloadGuardLocations(ctx); err != nil {
		s.log.Warn("storage: failed to reload guard after location update", "err", err)
	}

	if s.hub != nil {
		s.hub.Broadcast()
	}

	out := &PutStorageLocationOutput{}
	out.Body.OK = true
	return out, nil
}

// reloadGuardLocations rebuilds the Guard from the same source-of-truth
// main.go uses: cfg.StorageLocations (the config.yaml list) overlaid with
// any storageLocation.* overrides from app_settings, then EvalSymlinks
// canonicalized. This is the only way to see a change made via PUT to an
// override field (the storage_locations table is only seeded at boot --
// ListStorageLocations returns the same rows every time after that, so a
// reload-from-DB-only path is a no-op for the change just made).
//
// Unresolvable roots (mount down, symlink target gone) are logged and
// dropped from the Guard, matching LoadGuard's startup semantics. The DB
// row stays active (the operator's config still says it should exist);
// a future restart will retry the resolution. If a mount comes back,
// POST /api/v1/restart picks it up; the next manual override change
// triggers this same path and will pick it up too.
//
// A failure here is logged but does not fail the PUT request -- the DB
// write already succeeded and the next restart will pick up the change
// regardless.
//
// s.guard is allowed to be nil (the storage_locations tests build a Server
// without a Guard to isolate the override path from filesystem coupling):
// there's nothing to hot-swap in that case, so this is a no-op rather than
// a panic. Matches the optional-Guard pattern in routes.go
// (handleResolvePath, handlePurgeTrash).
func (s *Server) reloadGuardLocations(ctx context.Context) error {
	if s.guard == nil {
		return nil
	}
	cfg := s.cfg()
	if cfg == nil {
		return fmt.Errorf("config unavailable")
	}
	overrides, err := settings.LoadStorageLocationOverrides(ctx, s.db.Reader)
	if err != nil {
		return fmt.Errorf("load storage location overrides: %w", err)
	}
	effective := settings.ResolveStorageLocations(cfg.StorageLocations, overrides, s.log)

	locs := make([]storage.Location, 0, len(effective))
	for _, c := range effective {
		resolved, err := filepath.EvalSymlinks(c.RootPath)
		if err != nil {
			s.log.Warn("storage reload: skipping unresolvable root",
				"name", c.Name, "rootPath", c.RootPath, "err", err)
			continue
		}
		locs = append(locs, storage.Location{
			ID:       0, // re-resolved from override; Guard doesn't need the DB id for Resolve/CheckWrite
			Name:     c.Name,
			RootPath: filepath.Clean(resolved),
			Tier:     c.Tier,
			ReadOnly: c.ReadOnly,
		})
	}
	s.guard.ReloadLocations(locs)
	return nil
}
