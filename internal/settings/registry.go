// Package settings resolves UI-configurable overrides on top of the
// config.yaml/.env base config that internal/config loads at startup.
//
// The precedence rule is: a row in app_settings existing IS the override,
// regardless of its value -- an operator explicitly setting a field to ""
// in the UI must beat a populated environment variable (e.g. disabling
// Immich). This is why Registry.Set functions always overwrite when an
// override is present, with no "is it non-empty" branch anywhere in this
// package. See Resolve's doc comment for the full contract.
package settings

import "github.com/s3ntin3l8/branchdam/internal/config"

// Kind is the wire/storage type of a Field's value.
type Kind int

const (
	KindString Kind = iota
	KindInt
	KindBool
)

// ApplyMode describes when a Field's new value takes effect.
type ApplyMode int

const (
	// ApplyLive means a Store.Subscribe callback can act on the new value
	// immediately, with no process restart.
	ApplyLive ApplyMode = iota
	// ApplyRestart means the value is stored and reflected by Store.Effective,
	// but nothing in the running process re-reads it until next boot.
	ApplyRestart
	// ApplyNever means the field is never settable through this package at
	// all -- see Field.Editable.
	ApplyNever
)

// Field is one settable (or, for a non-editable field, merely
// display-only) point in config.Config. The registry is the single source
// of truth an HTTP DTO, a validation rule, and a UI form field are all
// generated from -- see docs/configuration.md's precedence section.
type Field struct {
	// Key identifies the field in app_settings and in the API, e.g.
	// "immich.apiUrl".
	Key   string
	Type  Kind
	Label string
	// Group names the section a UI would render this field under, e.g.
	// "Immich".
	Group  string
	Secret bool
	Apply  ApplyMode
	// Editable is false for fields this package refuses to ever write --
	// authz.groups (lockout risk), listenAddr/database.path (a running
	// process cannot change them). ReadOnlyReason explains why.
	Editable       bool
	ReadOnlyReason string

	// Get reads the field's current value out of cfg.
	Get func(cfg *config.Config) any
	// Set writes value into cfg. cfg is always a field-scoped shallow copy
	// (see Resolve) -- Set must assign, never mutate a slice/map in place,
	// or it would alias the original config.Config's storage.
	Set func(cfg *config.Config, value any) error
	// Validate rejects a value before it is ever persisted or applied --
	// config.Load itself performs no validation at all (see
	// internal/config's doc comment), so this is the first validation any
	// of these fields get. A nil Validate means any value of the right Kind
	// is accepted.
	Validate func(value any) error

	// EmptyMeansUnset opts this field into treating a literal, unexpanded
	// "${VAR}" base value the same as an empty one (see
	// IsUnresolvedEnvRef). This is correct for immich.apiUrl/libraryId,
	// where "not configured" and "" are the same state by design -- it
	// would be WRONG for most other fields: config.Load's whole "fails
	// loudly on a typo'd env var" contract (internal/config's doc comment)
	// depends on a literal ${VAR} staying visible downstream instead of
	// being silently emptied. Only set this on a field where "empty"
	// really is the field's own off-switch.
	EmptyMeansUnset bool

	Doc string
}

// immichFields is the only registered group in this PR -- the rest of
// config.Config's domains (workers, thumbnails, http, storageLocations,
// ...) are added to the registry alongside the settings HTTP API, which is
// what actually needs them. Immich is deliberately first: the sync worker
// already self-disables on an empty apiUrl/libraryId (main.go's
// strings.Contains(v, "${") sentinel), which is exactly the shape live
// reconfiguration needs, so it is the proof case for the whole feature.
var immichFields = []Field{
	{
		Key:   "immich.apiUrl",
		Type:  KindString,
		Label: "Immich API URL",
		Group: "Immich",
		Apply: ApplyLive,
		Get:   func(cfg *config.Config) any { return cfg.Immich.APIURL },
		Set: func(cfg *config.Config, v any) error {
			cfg.Immich.APIURL = v.(string)
			return nil
		},
		Editable:        true,
		EmptyMeansUnset: true,
		Doc:             "Base URL of the Immich instance the sync worker pushes external-library scan triggers to. Empty disables the worker.",
	},
	{
		Key:    "immich.apiKey",
		Type:   KindString,
		Label:  "Immich API Key",
		Group:  "Immich",
		Secret: true,
		Apply:  ApplyLive,
		Get:    func(cfg *config.Config) any { return cfg.Immich.APIKey },
		Set: func(cfg *config.Config, v any) error {
			cfg.Immich.APIKey = v.(string)
			return nil
		},
		Editable: true,
		Doc:      "Immich API key sent as X-Api-Key on the scan-trigger request.",
	},
	{
		Key:   "immich.libraryId",
		Type:  KindString,
		Label: "Immich Library ID",
		Group: "Immich",
		Apply: ApplyLive,
		Get:   func(cfg *config.Config) any { return cfg.Immich.LibraryID },
		Set: func(cfg *config.Config, v any) error {
			cfg.Immich.LibraryID = v.(string)
			return nil
		},
		Editable:        true,
		EmptyMeansUnset: true,
		Doc:             "External-library ID in Immich to trigger a rescan of. Empty disables the worker.",
	},
	{
		Key:   "immich.exportPath",
		Type:  KindString,
		Label: "Immich Export Path",
		Group: "Immich",
		Apply: ApplyLive,
		Get:   func(cfg *config.Config) any { return cfg.Immich.ExportPath },
		Set: func(cfg *config.Config, v any) error {
			cfg.Immich.ExportPath = v.(string)
			return nil
		},
		Editable: true,
		Doc:      "Tier-2 export directory the sync worker enqueues live nodes under, mounted natively into Immich as an external library.",
	},
}

// Fields returns every registered field, in registration order.
func Fields() []Field {
	out := make([]Field, 0, len(immichFields))
	out = append(out, immichFields...)
	return out
}

// Lookup returns the field registered under key, or false if none is.
func Lookup(key string) (Field, bool) {
	for _, f := range Fields() {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}
