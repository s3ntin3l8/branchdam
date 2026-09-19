package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestAgentChainLookupKeyHit verifies the device-pairing authentication
// path: when LookupKey returns a non-empty agent_id for a presented key,
// AgentChain attaches a Principal with Name=<agent_id> and lets the
// request through. Companion to TestAgentChainValidKey (env-var path).
func TestAgentChainLookupKeyHit(t *testing.T) {
	var got Principal
	var ignoredAuthHeader string
	chain := AgentChainWithConfig(AgentConfig{
		APIKey: testKey,
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			return KeyLookupResult{AgentID: "iphone-a3f9c2e1"}, nil
		},
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
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) { return KeyLookupResult{}, nil },
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
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			return KeyLookupResult{}, errors.New("simulated DB failure")
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
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			if presented == "device-key-1" {
				return KeyLookupResult{AgentID: "iphone-a3f9c2e1"}, nil
			}
			return KeyLookupResult{}, nil
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
				LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
					if presented == "device-key-1" {
						return KeyLookupResult{AgentID: "iphone-a3f9c2e1"}, nil
					}
					return KeyLookupResult{}, nil
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
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			return KeyLookupResult{AgentID: "dev-abc12345"}, nil
		},
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
		APIKey: "short12", // 7 chars, well below MinAgentKeyLength
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			return KeyLookupResult{AgentID: "dev-abc12345"}, nil
		},
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
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) { return KeyLookupResult{}, nil },
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
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) { return KeyLookupResult{}, nil },
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

// TestAgentChainWeakEnvKeyNeverAuthenticatesAsBootstrap locks in the
// invariant called out in the agent.go MinAgentKeyLength doc comment
// ("A shorter (or unset) key fails every agent request closed rather
// than accepting a weak or empty secret"). The relaxed gate lets a
// short env-var through when pairing is wired (paired traffic must
// still work, see TestAgentChainLookupKeyOnlyShortAPIKey), but the
// env-bootstrap branch must length-gate too -- otherwise a
// misconfigured-short env-var becomes a live Principal{env-bootstrap,
// KindMachine} (a principal that can act for any device) the moment
// pairing is wired. Presented with the short env-var itself, the
// request must 401, not authenticate as env-bootstrap.
func TestAgentChainWeakEnvKeyNeverAuthenticatesAsBootstrap(t *testing.T) {
	const weakKey = "short12" // 7 chars, well below MinAgentKeyLength

	var got Principal
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		APIKey: weakKey,
		// LookupKey intentionally misses for everything presented here --
		// we want the env-bootstrap branch to be the only candidate that
		// could authenticate the presented weak key. A LookupKey that
		// returned hit would mask the regression by authenticating via the
		// pairing path instead.
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) { return KeyLookupResult{}, nil },
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		p, ok := From(r.Context())
		if ok {
			got = p
		}
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, weakKey) // present the weak env-var itself
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (weak env-var must not authenticate as env-bootstrap)", rr.Code)
	}
	if handlerCalled {
		t.Errorf("handler was called; weak env-var should have been rejected before reaching it. Principal.Name = %q, Kind = %q", got.Name, got.Kind)
	}
	if got.Name == "env-bootstrap" {
		t.Errorf("Principal.Name = %q -- weak env-var leaked through as env-bootstrap (the regression this test guards)", got.Name)
	}
}

// TestAgentChainLookupKeyHMACUsesPerDeviceKey verifies that a paired
// device's signed request validates against the per-device HMAC key
// (returned by LookupKey in result.SigningKey), NOT against the
// configured env-var (which the device has no access to). Without this
// wiring, signedRequests=true in a pairing-only deployment would either
// fail every paired request or -- if the env-var is also set -- leak
// signing authority to the env-var holder (a separate device or a
// forgotten legacy key).
func TestAgentChainLookupKeyHMACUsesPerDeviceKey(t *testing.T) {
	// Per-device HMAC key returned by LookupKey for this plaintext.
	// 32 bytes (SHA-256 output) -- the server doesn't care about the
	// actual value, only that the HMAC over canonical+body matches when
	// signed with this exact key.
	perDeviceKey := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	const plaintext = "paired-device-plaintext-key-A"
	handlerCalled := false

	chain := AgentChainWithConfig(AgentConfig{
		SignedRequests: true,
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			if presented == plaintext {
				return KeyLookupResult{AgentID: "dev-A", SigningKey: perDeviceKey}, nil
			}
			return KeyLookupResult{}, nil
		},
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{"agentId":"dev-A"}`)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	nonce := "0123456789abcdef0123456789abcdef" // 32 hex chars
	// Sign with the per-device key. If the server fell back to env-var
	// (which is unset here), HMAC would compute a different value and
	// the signature would not match.
	sig := computeSignature(string(perDeviceKey), http.MethodPost, "/api/v1/agent/events", nonce, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(body))
	req.Header.Set(apiKeyHeader, plaintext)
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(nonceHeader, nonce)
	req.Header.Set(signatureHeader, sig)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (paired-device signature must validate against the per-device key returned by LookupKey)", rr.Code)
	}
	if !handlerCalled {
		t.Error("handler was not called -- signature must have validated to reach it")
	}
}

// TestAgentChainLookupKeyHMACAcrossDevicesNoCrossSign locks in the
// per-device isolation invariant: device A's signing key must NOT
// validate a request presented as device B. Without this isolation,
// any compromised paired key would let an attacker sign requests that
// the server attributes to any other paired device -- bypassing the
// per-device attribution that pairing was designed to provide.
func TestAgentChainLookupKeyHMACAcrossDevicesNoCrossSign(t *testing.T) {
	keyForA := []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA0") // 32 bytes, distinct
	keyForB := []byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB0") // 32 bytes, distinct
	const plaintextA = "paired-device-plaintext-key-A"
	const plaintextB = "paired-device-plaintext-key-B"

	chain := AgentChainWithConfig(AgentConfig{
		SignedRequests: true,
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			switch presented {
			case plaintextA:
				return KeyLookupResult{AgentID: "dev-A", SigningKey: keyForA}, nil
			case plaintextB:
				return KeyLookupResult{AgentID: "dev-B", SigningKey: keyForB}, nil
			}
			return KeyLookupResult{}, nil
		},
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called -- device A's key cannot sign for device B")
	}))

	body := []byte(`{"agentId":"dev-B"}`)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	nonce := "fedcba9876543210fedcba9876543210" // 32 hex chars

	// Request presents device B's plaintext (so LookupKey returns keyForB
	// and authenticates as dev-B), but the signature was computed with
	// device A's key. The server must reject.
	sig := computeSignature(string(keyForA), http.MethodPost, "/api/v1/agent/events", nonce, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(body))
	req.Header.Set(apiKeyHeader, plaintextB)
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(nonceHeader, nonce)
	req.Header.Set(signatureHeader, sig)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (cross-device signature must be rejected)", rr.Code)
	}
}

// TestAgentChainSignedRequestsEnvBootstrapStillUsesAPIKey verifies that
// the env-bootstrap path -- which still exists for legacy
// installations that haven't migrated to pairing -- continues to HMAC
// with cfg.APIKey as it did before PR C. Without this preservation,
// rotating cfg.APIKey (to invalidate env-bootstrap for everyone) would
// stop authenticating env-bootstrap requests, but it would NOT be the
// security regression -- the regression would be the opposite: paired
// devices would suddenly need to know the env-var to sign requests.
func TestAgentChainSignedRequestsEnvBootstrapStillUsesAPIKey(t *testing.T) {
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		APIKey:         testKey,
		SignedRequests: true,
		// No LookupKey -- env-bootstrap-only deployment.
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{"agentId":"env-bootstrap-handler"}`)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	nonce := "deadbeefcafef00ddeadbeefcafef00d"
	// Sign with cfg.APIKey (the env-var path's HMAC key). If the server
	// fell through to the per-device lookup path, it would have no
	// SigningKey and would either fail to validate or fall back to the
	// empty []byte(cfg.APIKey) which wouldn't match this signature.
	sig := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(body))
	req.Header.Set(apiKeyHeader, testKey)
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(nonceHeader, nonce)
	req.Header.Set(signatureHeader, sig)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (env-bootstrap signature must validate against cfg.APIKey)", rr.Code)
	}
	if !handlerCalled {
		t.Error("handler was not called -- env-bootstrap signature must have validated")
	}
}

// TestAgentChainPairedLookupNilSigningKeyFailsClosed locks in the
// fail-closed contract called out in the LookupKey doc comment:
// a paired-path LookupKey hit that returns KeyLookupResult{AgentID: <x>,
// SigningKey: nil} must be rejected, NOT silently fall back to
// cfg.APIKey (which would re-open the env-key/paired-device cross-
// signing authority issue #453 PR C removes). The wired
// internal/pairing.Service.KeyLookup satisfies this contract, so
// this test exercises the contract by stubbing a LookupKey that
// violates it.
func TestAgentChainPairedLookupNilSigningKeyFailsClosed(t *testing.T) {
	const plaintext = "paired-device-plaintext-key-with-nil-signing"
	handlerCalled := false
	chain := AgentChainWithConfig(AgentConfig{
		APIKey:         testKey, // present (the bug would fall back to it)
		SignedRequests: true,
		LookupKey: func(ctx context.Context, presented string) (KeyLookupResult, error) {
			if presented == plaintext {
				// Contract violation: hit returned but SigningKey is nil.
				return KeyLookupResult{AgentID: "dev-broken-impl", SigningKey: nil}, nil
			}
			return KeyLookupResult{}, nil
		},
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	body := []byte(`{"agentId":"dev-broken-impl"}`)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	nonce := "0123456789abcdef0123456789abcdef"
	// Sign with cfg.APIKey -- exactly what a regression that silently
	// fell back to it would accept. The fail-closed branch must reject.
	sig := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(body))
	req.Header.Set(apiKeyHeader, plaintext)
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(nonceHeader, nonce)
	req.Header.Set(signatureHeader, sig)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (paired-path nil SigningKey must fail closed, not fall back to cfg.APIKey)", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called -- the paired-path nil-SigningKey request must have been rejected")
	}
}
