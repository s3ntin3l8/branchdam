// Tests for passwordResetBaseURL (Hermes re-review on PR #460).
//
// The Host header on an inbound HTTP request is attacker-controlled.
// Embedding it into a password-reset link that gets emailed to a
// victim would let an attacker (Host: attacker.com) send a victim
// an email whose reset link points at attacker.com -- the victim
// resets their password on the attacker's page, surrendering the
// one-time token. passwordResetBaseURL must prefer the
// operator-declared auth.email.baseURL and only fall back to the
// inbound Host header when no baseURL is configured, and even then
// it logs a WARN so the misconfiguration is visible.
package httpapi

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

func TestPasswordResetBaseURL_PrefersConfigOverHost(t *testing.T) {
	srv := &Server{
		cfgProvider: staticConfigProvider{cfg: &config.Config{
			Auth: config.Auth{Email: config.AuthEmail{BaseURL: "https://branchdam.example.com"}},
		}},
		log: slog.New(slog.DiscardHandler),
	}
	req := httptest.NewRequest("POST", "/api/v1/password-reset/request", nil)
	req.Host = "attacker.example"

	assert.Equal(t, "https://branchdam.example.com", passwordResetBaseURL(srv, req))
}

func TestPasswordResetBaseURL_TrimsTrailingSlash(t *testing.T) {
	srv := &Server{
		cfgProvider: staticConfigProvider{cfg: &config.Config{
			Auth: config.Auth{Email: config.AuthEmail{BaseURL: "https://branchdam.example.com/"}},
		}},
		log: slog.New(slog.DiscardHandler),
	}
	req := httptest.NewRequest("POST", "/api/v1/password-reset/request", nil)

	got := passwordResetBaseURL(srv, req)
	assert.Equal(t, "https://branchdam.example.com", got)
}

func TestPasswordResetBaseURL_FallsBackToHostWhenUnset(t *testing.T) {
	srv := &Server{
		cfgProvider: staticConfigProvider{cfg: &config.Config{
			Auth: config.Auth{Email: config.AuthEmail{BaseURL: ""}},
		}},
		log: slog.New(slog.DiscardHandler),
	}
	req := httptest.NewRequest("POST", "/api/v1/password-reset/request", nil)
	req.Host = "branchdam.example"

	// baseURL unset -> fall back to Host. WARN is logged but doesn't
	// affect the returned value; this test pins only the value.
	// httptest.NewRequest uses scheme=http by default.
	assert.Equal(t, "http://branchdam.example", passwordResetBaseURL(srv, req))
}

func TestPasswordResetBaseURL_NilCfgReturnsRequestHost(t *testing.T) {
	srv := &Server{
		log: slog.New(slog.DiscardHandler),
	}
	req := httptest.NewRequest("POST", "/api/v1/password-reset/request", nil)
	req.Host = "branchdam.example"

	assert.Equal(t, "http://branchdam.example", passwordResetBaseURL(srv, req))
}
