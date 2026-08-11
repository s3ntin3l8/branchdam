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
  - name: scratch
    rootPath: /storage/scratch
    tier: TIER1_LOCAL_SCRATCH
    prunable: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.StorageLocations) != 2 {
		t.Fatalf("len(StorageLocations) = %d, want 2", len(cfg.StorageLocations))
	}
	if !cfg.StorageLocations[0].ReadOnly || cfg.StorageLocations[0].Tier != "TIER3_MASTER_ARCHIVE" {
		t.Errorf("StorageLocations[0] = %+v, want read-only TIER3_MASTER_ARCHIVE", cfg.StorageLocations[0])
	}
	if !cfg.StorageLocations[1].Prunable || cfg.StorageLocations[1].Tier != "TIER1_LOCAL_SCRATCH" {
		t.Errorf("StorageLocations[1] = %+v, want prunable TIER1_LOCAL_SCRATCH", cfg.StorageLocations[1])
	}
}
