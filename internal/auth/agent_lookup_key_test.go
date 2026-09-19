package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAgentChainLookupKeyHit verifies the device-pairing authentication
// path: when LookupKey returns a non-empty agent_id for a presented key,
// AgentChain attaches a Principal with Name=<agent_id> and lets the
// request through. Companion to TestAgentChainValidKey (env-var path).
func TestAgentChainLookupKeyHit(t *testing.T) {
	var got Principal
	var ignoredAuthHeader string
	chain := AgentChainWithConfig(AgentConfig{
		APIKey:    testKey,
		LookupKey: func(ctx context.Context, presented string) (string, error) { return "iphone-a3f9c2e1", nil },
	}, nil)(principalCapturingHandler(t, &got, &ignoredAuthHeader))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "the-device-pairing-key-not-the-env-var")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.Kind != KindMachine {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMachine)
	}
	if got.Name != "iphone-a3f9c2e1" {
		t.Errorf("Principal.Name = %q, want %q", got.Name, "iphone-a3f9c2e1")
	}
}

// TestAgentChainLookupKeyMiss verifies an active-key DB lookup that returns
// ("", nil) -- meaning no row in device_pairing_keys matches this hash --
// surfaces as 401, not 500. LookupKey's contract is that a non-nil error
// means DB trouble (5xx); empty result means "no match" (4xx).
func TestAgentChainLookupKeyMiss(t *testing.T) {
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		APIKey:    testKey,
		LookupKey: func(ctx context.Context, presented string) (string, error) { return "", nil },
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "never-issued-key")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called despite a LookupKey miss")
	}
}

// TestAgentChainLookupKeyDBError verifies a non-nil error from LookupKey
// surfaces as 500 (DB trouble) rather than 401 (auth fail). Operators
// monitoring their server should be able to distinguish "your key is
// wrong" from "the database is unreachable".
func TestAgentChainLookupKeyDBError(t *testing.T) {
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		APIKey: testKey,
		LookupKey: func(ctx context.Context, presented string) (string, error) {
			return "", errors.New("simulated DB failure")
		},
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "any-non-empty-key")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called despite a LookupKey DB error")
	}
}

// TestAgentChainEnvVarAndLookupKeyBothConfigured verifies the two paths
// coexist: presenting the env-var key authenticates as env-bootstrap,
// presenting a different (paired) key authenticates as that device's
// agent_id. A request that matches neither returns 401.
func TestAgentChainEnvVarAndLookupKeyBothConfigured(t *testing.T) {
	chain := AgentChainWithConfig(AgentConfig{
		APIKey: testKey,
		LookupKey: func(ctx context.Context, presented string) (string, error) {
			if presented == "device-key-1" {
				return "iphone-a3f9c2e1", nil
			}
			return "", nil
		},
	}, nil)(principalCapturingHandler(t, nil, nil))

	cases := []struct {
		name       string
		key        string
		wantStatus int
		wantName   string
	}{
		{"env-var key authenticates as env-bootstrap", testKey, http.StatusOK, "env-bootstrap"},
		{"device key authenticates as agent_id", "device-key-1", http.StatusOK, "iphone-a3f9c2e1"},
		{"unknown key is 401", "no-such-key", http.StatusUnauthorized, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got Principal
			var ignoredAuthHeader string
			subChain := AgentChainWithConfig(AgentConfig{
				APIKey: testKey,
				LookupKey: func(ctx context.Context, presented string) (string, error) {
					if presented == "device-key-1" {
						return "iphone-a3f9c2e1", nil
					}
					return "", nil
				},
			}, nil)(principalCapturingHandler(t, &got, &ignoredAuthHeader))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
			req.Header.Set(apiKeyHeader, c.key)
			rr := httptest.NewRecorder()
			subChain.ServeHTTP(rr, req)

			if rr.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, c.wantStatus)
			}
			if c.wantName != "" && got.Name != c.wantName {
				t.Errorf("Principal.Name = %q, want %q", got.Name, c.wantName)
			}
		})
	}

	// Reference chain so the outer `chain` declaration isn't flagged
	// unused -- the per-case subChain above is what actually runs.
	_ = chain
}

// TestAgentChainLookupKeyOnlyNoAPIKey covers the post-#453 pairing-only
// deployment: LookupKey wired, APIKey unset, a valid paired key presented
// -> authenticates as the paired device. Closes the test gap noted in
// issue #453: "env key unset (or too short) + pairing wired" was never
// exercised before, and is the exact configuration the relaxed gate now
// allows.
func TestAgentChainLookupKeyOnlyNoAPIKey(t *testing.T) {
	var got Principal
	var authHeader string
	chain := AgentChainWithConfig(AgentConfig{
		LookupKey: func(ctx context.Context, presented string) (string, error) { return "dev-abc12345", nil },
	}, nil)(principalCapturingHandler(t, &got, &authHeader))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "paired-device-plaintext-key")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (pairing-only deployment must accept paired keys without an env-var set)", rr.Code)
	}
	if got.Kind != KindMachine {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMachine)
	}
	if got.Name != "dev-abc12345" {
		t.Errorf("Principal.Name = %q, want %q", got.Name, "dev-abc12345")
	}
}

// TestAgentChainLookupKeyOnlyShortAPIKey verifies the relaxed gate does
// not brick paired traffic when an operator sets a too-short env-var key
// alongside a wired LookupKey. The settings-validator catches this at
// save time (internal/settings/registry.go's minLenString on agent.apiKey,
// PR #454), but the runtime gate must also tolerate it -- a misconfigured
// env-var that nothing is actually using must not 503 paired devices.
func TestAgentChainLookupKeyOnlyShortAPIKey(t *testing.T) {
	var got Principal
	var authHeader string
	chain := AgentChainWithConfig(AgentConfig{
		APIKey:    "short12", // 7 chars, well below MinAgentKeyLength
		LookupKey: func(ctx context.Context, presented string) (string, error) { return "dev-abc12345", nil },
	}, nil)(principalCapturingHandler(t, &got, &authHeader))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "paired-device-plaintext-key")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (paired device must authenticate even when env-var is misconfigured short)", rr.Code)
	}
	if got.Name != "dev-abc12345" {
		t.Errorf("Principal.Name = %q, want %q", got.Name, "dev-abc12345")
	}
}

// TestAgentChainLookupKeyOnlyMissNoAPIKey: pairing-only deployment (env-var
// unset), presented key doesn't match any active pairing -> 401 (not 503,
// not 500). Operator monitoring should be able to distinguish "your key
// is wrong" from "your database is unreachable" and from "no auth path
// configured at all."
func TestAgentChainLookupKeyOnlyMissNoAPIKey(t *testing.T) {
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		LookupKey: func(ctx context.Context, presented string) (string, error) { return "", nil },
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "no-such-paired-key")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (pairing-only miss is auth-fail, not 503 misconfig)", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called despite a LookupKey miss with no env-var fallback")
	}
}

// TestAgentChainLookupKeyOnlyMissShortAPIKey covers the corner where both
// the env-var is misconfigured-short AND the pairing lookup misses: the
// relaxed gate lets the request through to the switch, the env-var branch
// is unreachable (constantTimeEqual fails on the too-short key), and the
// LookupKey miss returns 401 -- NOT 503. A 503 here would have meant the
// operator can't distinguish "your pairing flow isn't returning the key
// you think it is" from "your env-var is too short".
func TestAgentChainLookupKeyOnlyMissShortAPIKey(t *testing.T) {
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		APIKey:    "short12",
		LookupKey: func(ctx context.Context, presented string) (string, error) { return "", nil },
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "no-such-paired-key")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called despite a LookupKey miss")
	}
}

// TestAgentChainNothingConfiguredStillFails503 verifies the relaxed gate
// doesn't open the floodgates when neither path is configured: no env-var
// AND no LookupKey means a true misconfiguration, and the historical
// 503 fail-closed behavior must hold. Companion to
// TestAgentChainFailsClosedOnMisconfiguredKey in agent_test.go.
func TestAgentChainNothingConfiguredStillFails503(t *testing.T) {
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, "any-key-value")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (neither env-var nor LookupKey must still fail closed)", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called despite no auth path configured")
	}
}
