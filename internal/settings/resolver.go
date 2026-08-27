package settings

import (
	"log/slog"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

// IsUnresolvedEnvRef reports whether s is (or contains) a literal,
// never-expanded ${VAR} reference -- config.expandEnv (internal/config)
// deliberately leaves an unset variable as literal text rather than
// emptying it, so a typo'd env var name fails loudly instead of silently
// producing an empty value. main.go's Immich startup check already used
// this exact substring test ad hoc in two places; this is that rule made
// shared, so the resolver and any future caller agree on what "not
// configured" means.
func IsUnresolvedEnvRef(s string) bool {
	return strings.Contains(s, "${")
}

// normalizeBase treats a literal unresolved ${VAR} the same as an empty
// base value for fields that opt into it via EmptyMeansUnset -- see that
// field's doc comment for why this must NOT be the default for every
// field.
func normalizeBase(f Field, v any) any {
	if !f.EmptyMeansUnset {
		return v
	}
	s, ok := v.(string)
	if !ok {
		return v
	}
	if IsUnresolvedEnvRef(s) {
		return ""
	}
	return v
}

// Resolve computes the effective config: base with every registered
// field's value replaced by its override when one is present in
// overrides. overrides is keyed by Field.Key; a present-but-empty-string
// entry is a deliberate override (see the package doc) and is applied
// exactly like any other value -- there is no "non-empty wins" branch.
//
// base is never mutated: Resolve returns a new *config.Config built from a
// shallow copy of *base. This is safe only because every registered
// Field.Set assigns rather than mutates in place (documented on Field.Set);
// an unregistered field is left aliasing base's own storage, which is fine
// since nothing here ever writes to it.
func Resolve(base *config.Config, overrides map[string]any, log *slog.Logger) *config.Config {
	if log == nil {
		log = slog.Default()
	}
	effective := *base

	for _, f := range Fields() {
		val, overridden := overrides[f.Key]
		if !overridden {
			val = normalizeBase(f, f.Get(base))
		}
		if err := f.Set(&effective, val); err != nil {
			// Set only fails on a type assertion mismatch, which means an
			// invalid value slipped past Store's own reload-time validation
			// (or normalizeBase produced an unexpected type) -- fall back to
			// the field's own base value rather than leaving a half-applied
			// config in place.
			log.Error("settings: failed to apply field, keeping config/env base value", "key", f.Key, "err", err)
			_ = f.Set(&effective, normalizeBase(f, f.Get(base)))
		}
	}

	return &effective
}
