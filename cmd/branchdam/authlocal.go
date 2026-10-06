package main

import (
	"fmt"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth/ratelimit"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/config"
)

// parseOptionalDuration parses a config duration string; blank means "use
// the package default" (zero). A malformed value is an error so a typo
// fails the boot instead of silently reverting to a default.
func parseOptionalDuration(field, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("auth.local.%s: %w", field, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("auth.local.%s: must not be negative", field)
	}
	return d, nil
}

// rateLimitConfig converts auth.local.rateLimit into a ratelimit.Config.
// Zero fields fall through to the limiter's own defaults.
func rateLimitConfig(rl config.RateLimit) (ratelimit.Config, error) {
	cfg := ratelimit.Config{
		MaxFailuresFast: rl.MaxFailuresFast,
		MaxFailuresSlow: rl.MaxFailuresSlow,
	}
	var err error
	if cfg.FastWindow, err = parseOptionalDuration("rateLimit.fastWindow", rl.FastWindow); err != nil {
		return cfg, err
	}
	if cfg.CoolOffFast, err = parseOptionalDuration("rateLimit.coolOffFast", rl.CoolOffFast); err != nil {
		return cfg, err
	}
	if cfg.SlowWindow, err = parseOptionalDuration("rateLimit.slowWindow", rl.SlowWindow); err != nil {
		return cfg, err
	}
	if cfg.CoolOffSlow, err = parseOptionalDuration("rateLimit.coolOffSlow", rl.CoolOffSlow); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// argonParams overlays auth.local.argon2 onto the OWASP defaults field by
// field, so setting only memoryKB keeps the other parameters sane.
func argonParams(a config.Argon2) users.Argon2idParameters {
	p := users.DefaultArgon2idParameters()
	if a.MemoryKB > 0 {
		p.MemoryKB = uint32(a.MemoryKB)
	}
	if a.Iterations > 0 {
		p.Iterations = uint32(a.Iterations)
	}
	if a.Parallelism > 0 {
		p.Parallelism = uint8(min(a.Parallelism, 255))
	}
	if a.SaltLength > 0 {
		p.SaltLength = uint32(a.SaltLength)
	}
	if a.KeyLength > 0 {
		p.KeyLength = uint32(a.KeyLength)
	}
	return p
}
