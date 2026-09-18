// Tests for passwordResetBaseURL (Hermes re-review on PR #460; hardened
// further to close CodeQL go/email-content-injection alert #86).
//
// The Host header on an inbound HTTP request is attacker-controlled.
// Embedding it into a password-reset link that gets emailed to a
// victim would let an attacker (Host: attacker.com) send a victim
// an email whose reset link points at attacker.com -- the victim
// resets their password on the attacker's page, surrendering the
// one-time token. passwordResetBaseURL therefore has NO fallback to
// the inbound Host header at all: it returns ok=false when
// auth.email.baseURL isn't configured, and the caller (handlePasswordResetRequest)
// must skip sending the reset email entirely in that case.
package httpapi

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

func TestPasswordResetBaseURL_UsesConfiguredValue(t *testing.T) {
	srv := &Server{
		cfgProvider: staticConfigProvider{cfg: &config.Config{
			Auth: config.Auth{Email: config.AuthEmail{BaseURL: "https://branchdam.example.com"}},
		}},
		log: slog.New(slog.DiscardHandler),
	}

	got, ok := passwordResetBaseURL(srv)
	assert.True(t, ok)
	assert.Equal(t, "https://branchdam.example.com", got)
}

func TestPasswordResetBaseURL_TrimsTrailingSlash(t *testing.T) {
	srv := &Server{
		cfgProvider: staticConfigProvider{cfg: &config.Config{
			Auth: config.Auth{Email: config.AuthEmail{BaseURL: "https://branchdam.example.com/"}},
		}},
		log: slog.New(slog.DiscardHandler),
	}

	got, ok := passwordResetBaseURL(srv)
	assert.True(t, ok)
	assert.Equal(t, "https://branchdam.example.com", got)
}

func TestPasswordResetBaseURL_UnsetReturnsNotOK(t *testing.T) {
	srv := &Server{
		cfgProvider: staticConfigProvider{cfg: &config.Config{
			Auth: config.Auth{Email: config.AuthEmail{BaseURL: ""}},
		}},
		log: slog.New(slog.DiscardHandler),
	}

	got, ok := passwordResetBaseURL(srv)
	assert.False(t, ok)
	assert.Empty(t, got)
}

func TestPasswordResetBaseURL_NilCfgReturnsNotOK(t *testing.T) {
	srv := &Server{
		log: slog.New(slog.DiscardHandler),
	}

	got, ok := passwordResetBaseURL(srv)
	assert.False(t, ok)
	assert.Empty(t, got)
}
