package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, "listenAddr: \":9090\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090", cfg.ListenAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default = %q, want info (unset field should keep default)", cfg.LogLevel)
	}
	if cfg.Database.Path != "/data/branchdam.db" {
		t.Errorf("Database.Path default = %q, want /data/branchdam.db", cfg.Database.Path)
	}
	if cfg.Workers.FullHashPolicy != "tier3_and_collision" {
		t.Errorf("Workers.FullHashPolicy default = %q, want tier3_and_collision", cfg.Workers.FullHashPolicy)
	}
	if !cfg.Thumbnails.Enabled {
		t.Error("Thumbnails.Enabled default = false, want true")
	}
	if cfg.Thumbnails.CacheDir != "/data/thumbs" {
		t.Errorf("Thumbnails.CacheDir default = %q, want /data/thumbs", cfg.Thumbnails.CacheDir)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("Load with missing file: want error, got nil")
	}
}

func TestExpandEnvSet(t *testing.T) {
	t.Setenv("BRANCHDAM_TEST_KEY", "supersecret")
	path := writeConfig(t, "agent:\n  apiKey: \"${BRANCHDAM_TEST_KEY}\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.APIKey != "supersecret" {
		t.Errorf("Agent.APIKey = %q, want supersecret", cfg.Agent.APIKey)
	}
}

func TestExpandEnvUnsetLeftLiteral(t *testing.T) {
	// An unset ${VAR} must be left as literal text, not silently emptied --
	// an empty agent API key would otherwise fail closed silently instead of
	// loudly, which is the wrong direction for a security-relevant field.
	path := writeConfig(t, "agent:\n  apiKey: \"${BRANCHDAM_DEFINITELY_UNSET_VAR}\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.APIKey != "${BRANCHDAM_DEFINITELY_UNSET_VAR}" {
		t.Errorf("Agent.APIKey = %q, want literal ${BRANCHDAM_DEFINITELY_UNSET_VAR}", cfg.Agent.APIKey)
	}
}

func TestLoadStorageLocations(t *testing.T) {
	path := writeConfig(t, `
storageLocations:
  - name: archive
    rootPath: /storage/archive
    tier: TIER3_MASTER_ARCHIVE
    readOnly: true
  - name: staging
    rootPath: /storage/staging
    tier: TIER0_LOCAL_STAGING
  - name: scratch
    rootPath: /storage/scratch
    tier: TIER1_LOCAL_SCRATCH
    prunable: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.StorageLocations) != 3 {
		t.Fatalf("len(StorageLocations) = %d, want 3", len(cfg.StorageLocations))
	}
	if !cfg.StorageLocations[0].ReadOnly || cfg.StorageLocations[0].Tier != "TIER3_MASTER_ARCHIVE" {
		t.Errorf("StorageLocations[0] = %+v, want read-only TIER3_MASTER_ARCHIVE", cfg.StorageLocations[0])
	}
	if cfg.StorageLocations[1].Tier != "TIER0_LOCAL_STAGING" || cfg.StorageLocations[1].RootPath != "/storage/staging" {
		t.Errorf("StorageLocations[1] = %+v, want /storage/staging TIER0_LOCAL_STAGING", cfg.StorageLocations[1])
	}
	if !cfg.StorageLocations[2].Prunable || cfg.StorageLocations[2].Tier != "TIER1_LOCAL_SCRATCH" {
		t.Errorf("StorageLocations[2] = %+v, want prunable TIER1_LOCAL_SCRATCH", cfg.StorageLocations[2])
	}
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("Load(config.example.yaml): %v", err)
	}
	var foundStaging bool
	for _, loc := range cfg.StorageLocations {
		if loc.Tier == "TIER0_LOCAL_STAGING" {
			foundStaging = true
			if loc.Name != "staging" {
				t.Errorf("staging location name = %q, want %q", loc.Name, "staging")
			}
			if loc.RootPath != "/storage/staging" {
				t.Errorf("staging location rootPath = %q, want %q", loc.RootPath, "/storage/staging")
			}
		}
	}
	if !foundStaging {
		t.Error("config.example.yaml missing TIER0_LOCAL_STAGING storage location")
	}
}

func TestLoadAuthzGroups(t *testing.T) {
	path := writeConfig(t, `
authz:
  groups:
    - dam-admins
    - super-users
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Authz.Groups) != 2 || cfg.Authz.Groups[0] != "dam-admins" || cfg.Authz.Groups[1] != "super-users" {
		t.Errorf("Authz.Groups = %v, want [dam-admins super-users]", cfg.Authz.Groups)
	}
}

func TestExposeOpenAPIConfig(t *testing.T) {
	path := writeConfig(t, `
http:
  exposeOpenAPI: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HTTP.ExposeOpenAPI {
		t.Errorf("HTTP.ExposeOpenAPI = false, want true")
	}
}
