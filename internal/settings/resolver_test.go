package settings

import (
	"log/slog"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

func TestIsUnresolvedEnvRef(t *testing.T) {
	cases := map[string]bool{
		"":                         false,
		"http://immich:2283":       false,
		"${IMMICH_API_URL}":        true,
		"prefix-${IMMICH_API_URL}": true,
	}
	for in, want := range cases {
		if got := IsUnresolvedEnvRef(in); got != want {
			t.Errorf("IsUnresolvedEnvRef(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestResolvePrecedenceMatrix is the correctness core the plan calls out:
// override-absent / override-empty / override-set crossed with
// base-empty / base-populated / base-literal-unresolved-ref. An
// override-set-to-"" must beat a populated base (disabling Immich from the
// UI must work), and a literal "${VAR}" base must behave identically to an
// empty one when there is no override.
func TestResolvePrecedenceMatrix(t *testing.T) {
	const key = "immich.apiUrl"
	log := slog.New(slog.DiscardHandler)

	type baseCase struct {
		name string
		base string
	}
	bases := []baseCase{
		{"empty", ""},
		{"populated", "http://immich.example:2283"},
		{"literal-unresolved-ref", "${IMMICH_API_URL}"},
	}

	type overrideCase struct {
		name    string
		present bool
		value   string
	}
	overrides := []overrideCase{
		{"absent", false, ""},
		{"empty", true, ""},
		{"set", true, "http://override.example:2283"},
	}

	for _, b := range bases {
		for _, o := range overrides {
			t.Run(b.name+"/"+o.name, func(t *testing.T) {
				cfg := &config.Config{Immich: config.Immich{APIURL: b.base}}
				ov := map[string]any{}
				if o.present {
					ov[key] = o.value
				}

				effective := Resolve(cfg, ov, log)

				var want string
				switch {
				case o.present:
					want = o.value
				case IsUnresolvedEnvRef(b.base):
					want = ""
				default:
					want = b.base
				}

				if effective.Immich.APIURL != want {
					t.Errorf("Immich.APIURL = %q, want %q (base=%q override.present=%v override.value=%q)",
						effective.Immich.APIURL, want, b.base, o.present, o.value)
				}
			})
		}
	}
}

func TestResolveOverrideEmptyBeatsPopulatedBase(t *testing.T) {
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich.example:2283"}}
	effective := Resolve(cfg, map[string]any{"immich.apiUrl": ""}, slog.New(slog.DiscardHandler))
	if effective.Immich.APIURL != "" {
		t.Errorf("Immich.APIURL = %q, want empty string (explicit UI override to disable must beat populated base)", effective.Immich.APIURL)
	}
}

func TestResolveDoesNotMutateBase(t *testing.T) {
	cfg := &config.Config{Immich: config.Immich{APIURL: "http://immich.example:2283"}}
	_ = Resolve(cfg, map[string]any{"immich.apiUrl": "http://override.example:2283"}, slog.New(slog.DiscardHandler))
	if cfg.Immich.APIURL != "http://immich.example:2283" {
		t.Errorf("base config.Immich.APIURL mutated to %q by Resolve", cfg.Immich.APIURL)
	}
}

// TestResolveLiteralRefPreservedWithoutEmptyMeansUnset pins the scope of
// the ${VAR}-normalization rule: it must NOT apply to a field that hasn't
// opted in via EmptyMeansUnset. immich.exportPath has a real config.yaml
// default ("/storage/exports/immich") and no "empty means disabled"
// semantics, so an unresolved reference in its base value must survive
// Resolve unchanged, preserving config.Load's "fails loudly on a typo'd
// env var" property for every field that isn't Immich's off-switch pair.
func TestResolveLiteralRefPreservedWithoutEmptyMeansUnset(t *testing.T) {
	cfg := &config.Config{Immich: config.Immich{ExportPath: "${IMMICH_EXPORT_PATH}"}}
	effective := Resolve(cfg, map[string]any{}, slog.New(slog.DiscardHandler))
	if effective.Immich.ExportPath != "${IMMICH_EXPORT_PATH}" {
		t.Errorf("Immich.ExportPath = %q, want the literal unresolved ref preserved (exportPath does not opt into EmptyMeansUnset)", effective.Immich.ExportPath)
	}
}

func TestResolveUnregisteredFieldUntouched(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":8080"}
	effective := Resolve(cfg, map[string]any{}, slog.New(slog.DiscardHandler))
	if effective.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want unchanged :8080 (not a registered field in this PR)", effective.ListenAddr)
	}
}
