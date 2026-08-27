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
