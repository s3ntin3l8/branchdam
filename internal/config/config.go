// Package config loads branchDAM's YAML configuration file, expanding
// ${VAR} references against the process environment.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Config is branchDAM's top-level configuration.
//
// Only ListenAddr, LogLevel, Database and HTTP are consumed in increment 1's
// early PRs (PR 0's /healthz). StorageLocations, Workers and Agent are
// defined now so the schema (PR 1), storage.Guard (PR 2), the worker pool
// (PR 4) and auth (PR 8) don't force a config shape change later — see
// docs/schema.md fix #1 for why StorageLocation.Tier's values matter.
type Config struct {
	ListenAddr string   `yaml:"listenAddr"`
	LogLevel   string   `yaml:"logLevel"`
	Database   Database `yaml:"database"`
	HTTP       HTTP     `yaml:"http"`
	Workers    Workers  `yaml:"workers"`
	Agent      Agent    `yaml:"agent"`
	Authz      Authz    `yaml:"authz"`
	Immich     Immich   `yaml:"immich"`

	StorageLocations []StorageLocation `yaml:"storageLocations"`

	// PathRewrites maps host path prefixes to container path prefixes (PR 44/45).
	PathRewrites []PathRewrite `yaml:"pathRewrites"`
}

// PathRewrite maps host path prefixes to container path prefixes.
type PathRewrite struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Authz configures group-based authorization (internal/auth, PR 2 / issue 37).
type Authz struct {
	Groups []string `yaml:"groups"`
}

// Database configures the SQLite file both pools open (internal/db, PR 1).
type Database struct {
	Path string `yaml:"path"`
}

// HTTP configures the stdlib http.Server wrapping the mux.
type HTTP struct {
	ReadTimeoutSecs  int  `yaml:"readTimeoutSecs"`
	WriteTimeoutSecs int  `yaml:"writeTimeoutSecs"`
	ExposeOpenAPI    bool `yaml:"exposeOpenAPI"`
}

// Workers configures the bounded hash worker pool (internal/workers, PR 4).
type Workers struct {
	// HashWorkers is the goroutine count for the hash pool. 0 (default)
	// means auto: min(4, runtime.NumCPU()).
	HashWorkers int `yaml:"hashWorkers"`
	// FullHashPolicy is one of "always", "tier3_and_collision" (default),
	// "never" — see docs/schema.md fix #8.
	FullHashPolicy string `yaml:"fullHashPolicy"`
	// PerceptualHash enables pHash extraction for images (default true).
	PerceptualHash bool `yaml:"perceptualHash"`
}

// Agent configures the machine-principal auth chain (internal/auth, PR 8).
// The key itself is never set directly in config.yaml -- it is meant to be
// injected via ${BRANCHDAM_AGENT_API_KEY} from a gitignored .env, per
// docs/forward-auth.md.
type Agent struct {
	APIKey string `yaml:"apiKey"`
}

// Immich configures the Immich external-library scan trigger (internal/immich,
// phase 7 #55). Leave APIURL empty to disable the sync worker entirely.
type Immich struct {
	APIURL     string `yaml:"apiUrl"`
	APIKey     string `yaml:"apiKey"`
	LibraryID  string `yaml:"libraryId"`
	ExportPath string `yaml:"exportPath"`
}

// StorageLocation is one row to seed into storage_locations at startup.
type StorageLocation struct {
	Name     string `yaml:"name"`
	RootPath string `yaml:"rootPath"`
	Tier     string `yaml:"tier"`
	ReadOnly bool   `yaml:"readOnly"`
	Prunable bool   `yaml:"prunable"`

	// Watch opts this location into a continuous fsnotify watcher at startup
	// (kind='WATCH' scan job), off by default. Never honored for Tier 3 --
	// the master archive is never watched regardless of this flag. Local NVMe
	// volumes only; fsnotify does not fire reliably over SMB/NFS (that
	// differential mtime sweep is a separate, later issue).
	Watch bool `yaml:"watch"`
}

func defaultConfig() Config {
	return Config{
		ListenAddr: ":8080",
		LogLevel:   "info",
		Database: Database{
			Path: "/data/branchdam.db",
		},
		HTTP: HTTP{
			ReadTimeoutSecs:  15,
			WriteTimeoutSecs: 15,
		},
		Workers: Workers{
			HashWorkers:    0,
			FullHashPolicy: "tier3_and_collision",
			PerceptualHash: true,
		},
	}
}

// Load reads and parses the config file at path, expanding ${VAR} references
// against the environment first. Unset variables are left as literal ${VAR}
// text (matching go-http-template's behavior) rather than silently emptied,
// so a typo'd variable name fails loudly downstream instead of producing an
// empty path or key.
func Load(path string) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	expanded := expandEnv(string(data))

	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	return cfg, nil
}

func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := strings.TrimSpace(envVarRe.FindStringSubmatch(match)[1])
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return match
	})
}
