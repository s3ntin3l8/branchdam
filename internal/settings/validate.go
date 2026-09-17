package settings

import (
	"fmt"
	"path/filepath"
	"slices"
)

// oneOf rejects any string value not in allowed -- used for enum-shaped
// config fields (workers.fullHashPolicy, logLevel) that internal/config
// itself never validates (see internal/config's doc comment: config.Load
// performs no validation at all).
func oneOf(allowed ...string) func(any) error {
	return func(v any) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if !slices.Contains(allowed, s) {
			return fmt.Errorf("must be one of %v", allowed)
		}
		return nil
	}
}

func nonNegativeInt(v any) error {
	i, ok := v.(int)
	if !ok {
		return fmt.Errorf("must be an integer")
	}
	if i < 0 {
		return fmt.Errorf("must be >= 0")
	}
	return nil
}

func positiveInt(v any) error {
	i, ok := v.(int)
	if !ok {
		return fmt.Errorf("must be an integer")
	}
	if i <= 0 {
		return fmt.Errorf("must be > 0")
	}
	return nil
}

// minLenString rejects a string shorter than n. Used for agent.apiKey, whose
// underlying auth.MinAgentKeyLength requirement was previously enforced only
// at request time (internal/auth.AgentChainWithConfig's keyConfigured gate) --
// a too-short value saved through the settings UI would 503 every agent
// route, paired devices included, only after the next restart. Validate
// runs before Store.Apply's own value.(string) assertion for secret fields
// (see store.go), so this also owns the "not a string" case.
func minLenString(n int) func(any) error {
	return func(v any) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if len(s) < n {
			return fmt.Errorf("must be at least %d characters", n)
		}
		return nil
	}
}

// absolutePath mirrors the constraint internal/config's doc comments state
// for database.path/thumbnails.cacheDir but config.Load never enforces.
func absolutePath(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("must be a string")
	}
	if !filepath.IsAbs(s) {
		return fmt.Errorf("must be an absolute path")
	}
	return nil
}
