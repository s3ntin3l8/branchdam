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

import (
	"fmt"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

// Kind is the wire/storage type of a Field's value.
type Kind int

const (
	KindString Kind = iota
	KindInt
	KindBool
	KindStringList
	KindPathRewriteList
)

func (k Kind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindInt:
		return "int"
	case KindBool:
		return "bool"
	case KindStringList:
		return "stringList"
	case KindPathRewriteList:
		return "pathRewriteList"
	default:
		return "unknown"
	}
}

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

func (a ApplyMode) String() string {
	switch a {
	case ApplyLive:
		return "live"
	case ApplyRestart:
		return "restart"
	case ApplyNever:
		return "never"
	default:
		return "unknown"
	}
}

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

// serverFields covers the two top-level fields that describe the running
// process itself, plus logLevel. listenAddr and database.path are
// deliberately never editable: nothing this package does can rebind a
// listener or move an open SQLite file out from under the process that
// opened it -- see docs/configuration.md's precedence section.
var serverFields = []Field{
	{
		Key:            "listenAddr",
		Type:           KindString,
		Label:          "Listen Address",
		Group:          "Server",
		Apply:          ApplyNever,
		Get:            func(cfg *config.Config) any { return cfg.ListenAddr },
		Set:            func(cfg *config.Config, v any) error { cfg.ListenAddr = v.(string); return nil },
		Editable:       false,
		ReadOnlyReason: "a running process cannot rebind its own listen address",
		Doc:            "The stdlib http.Server bind address.",
	},
	{
		Key:            "database.path",
		Type:           KindString,
		Label:          "Database Path",
		Group:          "Server",
		Apply:          ApplyNever,
		Get:            func(cfg *config.Config) any { return cfg.Database.Path },
		Set:            func(cfg *config.Config, v any) error { cfg.Database.Path = v.(string); return nil },
		Editable:       false,
		ReadOnlyReason: "a running process cannot move its own open database file",
		Doc:            "Path to the SQLite database file.",
	},
	{
		Key:      "logLevel",
		Type:     KindString,
		Label:    "Log Level",
		Group:    "Server",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.LogLevel },
		Set:      func(cfg *config.Config, v any) error { cfg.LogLevel = v.(string); return nil },
		Validate: oneOf("debug", "info", "warn", "error"),
		Editable: true,
		Doc:      "One of debug/info/warn/error.",
	},
}

var workersFields = []Field{
	{
		Key:      "workers.hashWorkers",
		Type:     KindInt,
		Label:    "Hash Workers",
		Group:    "Workers",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Workers.HashWorkers },
		Set:      func(cfg *config.Config, v any) error { cfg.Workers.HashWorkers = v.(int); return nil },
		Validate: nonNegativeInt,
		Editable: true,
		Doc:      "Goroutine count for the hash pool. 0 = auto (min(4, NumCPU)).",
	},
	{
		Key:      "workers.fullHashPolicy",
		Type:     KindString,
		Label:    "Full Hash Policy",
		Group:    "Workers",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Workers.FullHashPolicy },
		Set:      func(cfg *config.Config, v any) error { cfg.Workers.FullHashPolicy = v.(string); return nil },
		Validate: oneOf("always", "tier3_and_collision", "never"),
		Editable: true,
		Doc:      "When the expensive BLAKE3-256 full_hash runs.",
	},
	{
		Key:      "workers.perceptualHash",
		Type:     KindBool,
		Label:    "Perceptual Hash",
		Group:    "Workers",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Workers.PerceptualHash },
		Set:      func(cfg *config.Config, v any) error { cfg.Workers.PerceptualHash = v.(bool); return nil },
		Editable: true,
		Doc:      "Enables pHash extraction; Tier-3 heuristic matching depends on this.",
	},
}

var thumbnailsFields = []Field{
	{
		Key:      "thumbnails.enabled",
		Type:     KindBool,
		Label:    "Thumbnails Enabled",
		Group:    "Thumbnails",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Thumbnails.Enabled },
		Set:      func(cfg *config.Config, v any) error { cfg.Thumbnails.Enabled = v.(bool); return nil },
		Editable: true,
	},
	{
		Key:      "thumbnails.cacheDir",
		Type:     KindString,
		Label:    "Thumbnail Cache Directory",
		Group:    "Thumbnails",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Thumbnails.CacheDir },
		Set:      func(cfg *config.Config, v any) error { cfg.Thumbnails.CacheDir = v.(string); return nil },
		Validate: absolutePath,
		Editable: true,
		Doc:      "Must be absolute. Changing this orphans any thumbnails already cached under the old path.",
	},
	{
		Key:      "thumbnails.maxEdgePx",
		Type:     KindInt,
		Label:    "Thumbnail Max Edge (px)",
		Group:    "Thumbnails",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Thumbnails.MaxEdgePx },
		Set:      func(cfg *config.Config, v any) error { cfg.Thumbnails.MaxEdgePx = v.(int); return nil },
		Validate: nonNegativeInt,
		Editable: true,
	},
	{
		Key:      "thumbnails.workers",
		Type:     KindInt,
		Label:    "Thumbnail Workers",
		Group:    "Thumbnails",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Thumbnails.Workers },
		Set:      func(cfg *config.Config, v any) error { cfg.Thumbnails.Workers = v.(int); return nil },
		Validate: nonNegativeInt,
		Editable: true,
	},
	{
		Key:      "thumbnails.intervalSecs",
		Type:     KindInt,
		Label:    "Thumbnail Poll Interval (s)",
		Group:    "Thumbnails",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Thumbnails.IntervalSecs },
		Set:      func(cfg *config.Config, v any) error { cfg.Thumbnails.IntervalSecs = v.(int); return nil },
		Validate: nonNegativeInt,
		Editable: true,
	},
}

var httpFields = []Field{
	{
		Key:      "http.readTimeoutSecs",
		Type:     KindInt,
		Label:    "HTTP Read Timeout (s)",
		Group:    "HTTP",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.HTTP.ReadTimeoutSecs },
		Set:      func(cfg *config.Config, v any) error { cfg.HTTP.ReadTimeoutSecs = v.(int); return nil },
		Validate: positiveInt,
		Editable: true,
	},
	{
		Key:      "http.writeTimeoutSecs",
		Type:     KindInt,
		Label:    "HTTP Write Timeout (s)",
		Group:    "HTTP",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.HTTP.WriteTimeoutSecs },
		Set:      func(cfg *config.Config, v any) error { cfg.HTTP.WriteTimeoutSecs = v.(int); return nil },
		Validate: positiveInt,
		Editable: true,
	},
	{
		Key:      "http.exposeOpenAPI",
		Type:     KindBool,
		Label:    "Expose OpenAPI",
		Group:    "HTTP",
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.HTTP.ExposeOpenAPI },
		Set:      func(cfg *config.Config, v any) error { cfg.HTTP.ExposeOpenAPI = v.(bool); return nil },
		Editable: true,
		Doc:      "Serves /openapi.json, /openapi.yaml, and /docs.",
	},
}

var agentFields = []Field{
	{
		Key:      "agent.apiKey",
		Type:     KindString,
		Label:    "Agent API Key",
		Group:    "Agent",
		Secret:   true,
		Apply:    ApplyRestart,
		Get:      func(cfg *config.Config) any { return cfg.Agent.APIKey },
		Set:      func(cfg *config.Config, v any) error { cfg.Agent.APIKey = v.(string); return nil },
		Editable: true,
		Doc:      "Machine-principal key for /api/v1/agent/* routes. Changing this breaks every workstation agent until re-keyed with the new value.",
	},
}

// authzFields is display-only, by decision: authz.groups gates the very
// settings routes that would edit it, and an operator locking themselves
// out of every admin group has no recovery path short of an on-disk config
// edit. See docs/configuration.md's precedence section.
var authzFields = []Field{
	{
		Key:   "authz.groups",
		Type:  KindStringList,
		Label: "Admin Groups",
		Group: "Authorization",
		Apply: ApplyNever,
		Get:   func(cfg *config.Config) any { return cfg.Authz.Groups },
		Set: func(cfg *config.Config, v any) error {
			groups, ok := v.([]string)
			if !ok {
				return fmt.Errorf("must be a string list")
			}
			cfg.Authz.Groups = groups
			return nil
		},
		Editable:       false,
		ReadOnlyReason: "editable only via config.yaml/.env -- a UI edit that locks the operator out of every admin group would have no recovery path",
		Doc:            "Groups permitted admin (write) access. Empty means every authenticated user is admin.",
	},
}

var pathRewriteFields = []Field{
	{
		Key:   "pathRewrites",
		Type:  KindPathRewriteList,
		Label: "Operator Path Rewrites",
		Group: "Path Resolution",
		Apply: ApplyLive,
		Get: func(cfg *config.Config) any {
			if cfg.PathRewrites == nil {
				return []config.PathRewrite{}
			}
			return cfg.PathRewrites
		},
		Set: func(cfg *config.Config, v any) error {
			list, ok := v.([]config.PathRewrite)
			if !ok {
				return fmt.Errorf("must be a list of path rewrites")
			}
			cfg.PathRewrites = list
			return nil
		},
		Validate: func(v any) error {
			list, ok := v.([]config.PathRewrite)
			if !ok {
				return fmt.Errorf("must be a list of path rewrites")
			}
			for i, rw := range list {
				if strings.TrimSpace(rw.From) == "" {
					return fmt.Errorf("item %d: 'from' prefix cannot be empty", i)
				}
				if strings.TrimSpace(rw.To) == "" {
					return fmt.Errorf("item %d: 'to' prefix cannot be empty", i)
				}
			}
			return nil
		},
		Editable: true,
		Doc:      "Host-to-container path transformation rules used when resolving project-file references (Tier-1).",
	},
}

// Fields returns every registered field, in registration order.
func Fields() []Field {
	groups := [][]Field{immichFields, serverFields, workersFields, thumbnailsFields, httpFields, agentFields, authzFields, pathRewriteFields}
	n := 0
	for _, g := range groups {
		n += len(g)
	}
	out := make([]Field, 0, n)
	for _, g := range groups {
		out = append(out, g...)
	}
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
