package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// appSettingsLister is the subset of sqlcgen.Querier LoadStorageLocationOverrides
// needs -- satisfied by both *db.DB.Reader (the multi-conn read pool, used
// standalone e.g. by handleStorageHealth) and a *sqlcgen.Queries bound to an
// in-progress writer transaction (used by internal/httpapi's
// handlePutStorageLocation so the last-enabled-location count and the
// override write it guards happen atomically -- see ApplyStorageLocationOverrideTx).
type appSettingsLister interface {
	ListAppSettings(ctx context.Context) ([]sqlcgen.AppSetting, error)
}

// StorageLocationKeyPrefix namespaces app_settings rows holding per-location
// safe-field overrides: storageLocation.<rootPath>.<field>, e.g.
// "storageLocation./data/tier1.watch". These rows are deliberately outside
// the Field registry (Fields()/Lookup()) -- a storage location is a
// dynamic list keyed by rootPath, not the fixed, enumerable set of keys
// the registry model assumes -- and never secret. Store.reload skips this
// namespace silently rather than warning "unregistered key" for every row
// in it (see that call site).
const StorageLocationKeyPrefix = "storageLocation."

// storageLocationFields is the exact set of safe, UI-editable fields for a
// storage location. rootPath, tier, readOnly, and prunable stay
// config-only by design -- they gate storage.Guard's Tier-3 write refusal
// and the Tier-1 prune authorization (see docs/configuration.md's
// precedence section) -- so they are not, and must never become, keys in
// this namespace.
var storageLocationFields = map[string]bool{
	"name": true, "watch": true, "sweep": true,
	"sweepIntervalSecs": true, "cacheTtlHours": true, "enabled": true,
}

// IsStorageLocationField reports whether field is one of the six safe,
// overridable storage-location fields -- internal/httpapi uses this to
// reject an unrecognized key in a PUT body's set/unset instead of silently
// ignoring it.
func IsStorageLocationField(field string) bool {
	return storageLocationFields[field]
}

// StorageLocationOverride holds the safe, UI-editable fields for one
// storage location. A nil pointer means no override exists for that field
// -- the same row-exists-is-the-override rule the rest of this package
// follows (see resolver.go).
type StorageLocationOverride struct {
	Name              *string
	Watch             *bool
	Sweep             *bool
	SweepIntervalSecs *int
	CacheTTLHours     *int
	Enabled           *bool
}

func storageLocationOverrideKey(rootPath, field string) string {
	return StorageLocationKeyPrefix + rootPath + "." + field
}

// LoadStorageLocationOverrides reads every storageLocation.* row from
// app_settings and groups it by rootPath. A row whose value fails to
// decode is skipped (logged by the caller's context, not here -- this is a
// low-level read used both at boot and on every storage-health request, so
// it stays quiet by design and lets ResolveStorageLocations/callers decide
// what "no override" versus "a bad one" should look like to an operator).
func LoadStorageLocationOverrides(ctx context.Context, lister appSettingsLister) (map[string]StorageLocationOverride, error) {
	rows, err := lister.ListAppSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("list app_settings: %w", err)
	}
	out := make(map[string]StorageLocationOverride)
	for _, row := range rows {
		if !strings.HasPrefix(row.Key, StorageLocationKeyPrefix) {
			continue
		}
		rest := strings.TrimPrefix(row.Key, StorageLocationKeyPrefix)
		idx := strings.LastIndex(rest, ".")
		if idx < 0 {
			continue // malformed key, ignore
		}
		rootPath, field := rest[:idx], rest[idx+1:]
		ov := out[rootPath]
		switch field {
		case "name":
			var v string
			if json.Unmarshal([]byte(row.Value), &v) == nil {
				ov.Name = &v
			}
		case "watch":
			var v bool
			if json.Unmarshal([]byte(row.Value), &v) == nil {
				ov.Watch = &v
			}
		case "sweep":
			var v bool
			if json.Unmarshal([]byte(row.Value), &v) == nil {
				ov.Sweep = &v
			}
		case "sweepIntervalSecs":
			var v int
			if json.Unmarshal([]byte(row.Value), &v) == nil {
				ov.SweepIntervalSecs = &v
			}
		case "cacheTtlHours":
			var v int
			if json.Unmarshal([]byte(row.Value), &v) == nil {
				ov.CacheTTLHours = &v
			}
		case "enabled":
			var v bool
			if json.Unmarshal([]byte(row.Value), &v) == nil {
				ov.Enabled = &v
			}
		default:
			continue // unrecognized field name, ignore
		}
		out[rootPath] = ov
	}
	return out, nil
}

// ResolveStorageLocations applies overrides onto base, keyed by rootPath,
// and returns the effective list main.go feeds to seedStorageLocations,
// watchedFromConfig, and sweptFromConfig. A location whose override sets
// Enabled=false is dropped entirely -- disabling a location is modelled as
// removing it from the effective list, so the existing
// DeactivateStorageLocationsNotIn self-heal (an existing DB row whose
// rootPath is absent from what's seeded) sets it inactive the same way a
// mount removed from config.yaml already does, with no seeder change
// needed.
//
// An override for a rootPath no longer present in base (removed from
// config.yaml, or renamed) is simply never matched here -- inert, not a
// leak: nothing in this codebase deletes app_settings rows automatically,
// so a stale override just sits unused until the operator's next edit
// overwrites it or the rootPath reappears in config.
//
// A CacheTTLHours override that would violate validatePruneConfig's rule
// (positive on a non-prunable location, or negative) is dropped with a
// WARN log, falling back to the base config value, rather than applied.
// Prunable is config-only and never itself overridable, so a
// once-legitimate override (set while the location was prunable) can
// become invalid later purely from a config.yaml edit (prunable flipped to
// false) with no PUT involved. Unlike the generic settings registry, the
// result of this function is fed straight into validatePruneConfig by
// main.go, which os.Exit(1)s on a violation -- silently falling back here
// is what keeps a since-invalidated override from bricking boot with the
// UI that could fix it unreachable, mirroring PR1's "WARN-and-ignore,
// never fatal" decision for the generic registry (see
// docs/configuration.md's precedence section).
func ResolveStorageLocations(base []config.StorageLocation, overrides map[string]StorageLocationOverride, log *slog.Logger) []config.StorageLocation {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	out := make([]config.StorageLocation, 0, len(base))
	for _, loc := range base {
		ov, ok := overrides[loc.RootPath]
		if !ok {
			out = append(out, loc)
			continue
		}
		if ov.Enabled != nil && !*ov.Enabled {
			continue
		}
		if ov.Name != nil {
			loc.Name = *ov.Name
		}
		if ov.Watch != nil {
			loc.Watch = *ov.Watch
		}
		if ov.Sweep != nil {
			loc.Sweep = *ov.Sweep
		}
		if ov.SweepIntervalSecs != nil {
			loc.SweepIntervalSecs = *ov.SweepIntervalSecs
		}
		if ov.CacheTTLHours != nil {
			if *ov.CacheTTLHours < 0 || (*ov.CacheTTLHours > 0 && !loc.Prunable) {
				log.Warn("settings: dropping invalid storage location cacheTtlHours override, falling back to config value",
					"rootPath", loc.RootPath, "override", *ov.CacheTTLHours, "prunable", loc.Prunable)
			} else {
				loc.CacheTTLHours = *ov.CacheTTLHours
			}
		}
		out = append(out, loc)
	}
	return out
}

// ApplyStorageLocationOverride writes a validated patch of safe-field
// overrides for one location's rootPath in a single transaction: every
// unset key is deleted first, then every set key is upserted. set's values
// must already be normalized to their Go-typed form and unset's/set's keys
// already validated against IsStorageLocationField by the caller
// (internal/httpapi) -- this function only encodes and writes, the same
// division of responsibility as Store.Apply/handlePutSettings.
func ApplyStorageLocationOverride(ctx context.Context, database *db.DB, rootPath string, set map[string]any, unset []string, actor string) error {
	return database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return ApplyStorageLocationOverrideTx(ctx, q, rootPath, set, unset, actor)
	})
}

// ApplyStorageLocationOverrideTx is ApplyStorageLocationOverride's write
// logic against an already-open transaction, exposed so a caller that must
// read-then-write atomically (internal/httpapi's handlePutStorageLocation,
// whose last-enabled-location guard would otherwise TOCTOU-race a second
// concurrent disable request -- see that function's doc comment) can run
// the guard's count and this write inside one db.InTx block instead of two.
func ApplyStorageLocationOverrideTx(ctx context.Context, q *sqlcgen.Queries, rootPath string, set map[string]any, unset []string, actor string) error {
	for _, field := range unset {
		if err := q.DeleteAppSetting(ctx, storageLocationOverrideKey(rootPath, field)); err != nil {
			return fmt.Errorf("delete %q: %w", field, err)
		}
	}
	for field, value := range set {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode %q: %w", field, err)
		}
		if _, err := q.UpsertAppSetting(ctx, sqlcgen.UpsertAppSettingParams{
			Key: storageLocationOverrideKey(rootPath, field), Value: string(encoded), IsSecret: 0, UpdatedBy: actor,
		}); err != nil {
			return fmt.Errorf("upsert %q: %w", field, err)
		}
	}
	return nil
}
