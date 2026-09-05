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
