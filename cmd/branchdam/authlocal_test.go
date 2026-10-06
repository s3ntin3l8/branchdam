package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/config"
)

func TestRateLimitConfig(t *testing.T) {
	cfg, err := rateLimitConfig(config.RateLimit{MaxFailuresFast: 7, FastWindow: "2m", CoolOffSlow: "10m", MaxFailuresSlow: 20})
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.MaxFailuresFast)
	assert.Equal(t, 2*time.Minute, cfg.FastWindow)
	assert.Equal(t, 10*time.Minute, cfg.CoolOffSlow)
	assert.Equal(t, 20, cfg.MaxFailuresSlow)
	assert.Zero(t, cfg.CoolOffFast, "unset stays zero so the limiter default applies")

	_, err = rateLimitConfig(config.RateLimit{FastWindow: "five minutes"})
	assert.ErrorContains(t, err, "auth.local.rateLimit.fastWindow")
	_, err = rateLimitConfig(config.RateLimit{CoolOffFast: "-1s"})
	assert.Error(t, err)
}

func TestArgonParamsOverlaysDefaults(t *testing.T) {
	def := users.DefaultArgon2idParameters()
	assert.Equal(t, def, argonParams(config.Argon2{}))

	p := argonParams(config.Argon2{MemoryKB: 65536, Iterations: 3})
	assert.EqualValues(t, 65536, p.MemoryKB)
	assert.EqualValues(t, 3, p.Iterations)
	assert.Equal(t, def.Parallelism, p.Parallelism)
	assert.Equal(t, def.KeyLength, p.KeyLength)
}
