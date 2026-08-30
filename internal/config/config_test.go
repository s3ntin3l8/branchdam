package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// An unset ${VAR} in a non-sensitive field must be left as literal text,
	// not silently emptied -- a typo'd variable name fails loudly downstream.
	path := writeConfig(t, "listenAddr: \"${BRANCHDAM_DEFINITELY_UNSET_VAR}\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "${BRANCHDAM_DEFINITELY_UNSET_VAR}" {
		t.Errorf("ListenAddr = %q, want literal ${BRANCHDAM_DEFINITELY_UNSET_VAR}", cfg.ListenAddr)
	}
}

func TestExpandEnvUnsetInSensitiveFieldRejected(t *testing.T) {
	// An unset ${VAR} in a sensitive field must be rejected by
	// validateSecretExpansion -- leaving an unresolved env var in an API key
	// would silently authenticate with a broken credential.
	t.Setenv("BRANCHDAM_TEST_UNSET_VAR_XYZ", "")
	os.Unsetenv("BRANCHDAM_TEST_UNSET_VAR_XYZ") //nolint:errcheck // intentional: test needs var unset
	path := writeConfig(t, "agent:\n  apiKey: \"${BRANCHDAM_TEST_UNSET_VAR_XYZ}\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with unresolved secret: want error, got nil")
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
	t.Setenv("BRANCHDAM_AGENT_API_KEY", "example-agent-key")
	t.Setenv("IMMICH_API_KEY", "example-immich-key")
	t.Setenv("IMMICH_API_URL", "http://immich.example.com")
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

func writeConfigWithPerm(t *testing.T, contents string, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), perm); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	return path
}

func TestLoadWarnsOnWorldReadableConfig(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)

	path := writeConfigWithPerm(t, "listenAddr: \":9090\"\n", 0o644)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(buf.String(), "world-readable") {
		t.Errorf("expected world-readable warning in log output, got: %s", buf.String())
	}
}

func TestLoadNoWarningOnRestrictedConfig(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)

	path := writeConfigWithPerm(t, "listenAddr: \":9090\"\n", 0o600)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(buf.String(), "world-readable") {
		t.Errorf("unexpected world-readable warning for 0600 config: %s", buf.String())
	}
}

func TestLoadRejectsUnresolvedSecretEnvVars(t *testing.T) {
	t.Setenv("BRANCHDAM_UNDEFINED_TEST_VAR", "")
	os.Unsetenv("BRANCHDAM_UNDEFINED_TEST_VAR") //nolint:errcheck // intentional: test needs var unset
	path := writeConfig(t, "agent:\n  apiKey: \"${BRANCHDAM_UNDEFINED_TEST_VAR}\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with unresolved secret: want error, got nil")
	}
	if !strings.Contains(err.Error(), "agent.apiKey") {
		t.Errorf("error = %q, want it to mention agent.apiKey", err.Error())
	}
}

func TestLoadRejectsMultipleUnresolvedSecrets(t *testing.T) {
	os.Unsetenv("BRANCHDAM_UNDEFINED_VAR_A") //nolint:errcheck // intentional: test needs var unset
	os.Unsetenv("BRANCHDAM_UNDEFINED_VAR_B") //nolint:errcheck // intentional: test needs var unset
	path := writeConfig(t, "agent:\n  apiKey: \"${BRANCHDAM_UNDEFINED_VAR_A}\"\nimmich:\n  apiKey: \"${BRANCHDAM_UNDEFINED_VAR_B}\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with multiple unresolved secrets: want error, got nil")
	}
	if !strings.Contains(err.Error(), "agent.apiKey") {
		t.Errorf("error = %q, want it to mention agent.apiKey", err.Error())
	}
	if !strings.Contains(err.Error(), "immich.apiKey") {
		t.Errorf("error = %q, want it to mention immich.apiKey", err.Error())
	}
}

func TestLoadAcceptsResolvedSecretEnvVars(t *testing.T) {
	t.Setenv("BRANCHDAM_TEST_SECRET", "my-api-key")
	path := writeConfig(t, "agent:\n  apiKey: \"${BRANCHDAM_TEST_SECRET}\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.APIKey != "my-api-key" {
		t.Errorf("Agent.APIKey = %q, want my-api-key", cfg.Agent.APIKey)
	}
}

func TestLoadAllowsEmptySecretFields(t *testing.T) {
	path := writeConfig(t, "agent:\n  apiKey: \"\"\nimmich:\n  apiUrl: \"\"\n  apiKey: \"\"\n")
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load with empty secrets: %v, want nil", err)
	}
}
