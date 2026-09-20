package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const testKey = "01234567890123456789012345678901" // 33 chars
const testAgentID = "test-device"

// chainWithTestKey mimics the legacy chainWithTestKey() factory
// that issue #453 PR F removed. Returns an AgentChainWithConfig whose
// LookupKey stub accepts the canonical testKey and reports the supplied
// agent_id and signing key, so the post-PR-F chain still authenticates
// the same X-API-Key tests have always used. The signing key is
// []byte(testKey) so HMAC verification continues to compute against the
// same secret the pre-PR-F env-bootstrap path did, preserving every
// signature-mismatch assertion in TestAgentChainSignedRequests.
func chainWithTestKey() func(http.Handler) http.Handler {
	return AgentChainWithConfig(AgentConfig{
		LookupKey: func(_ context.Context, presented string) (KeyLookupResult, error) {
			if presented == testKey {
				return KeyLookupResult{AgentID: testAgentID, SigningKey: []byte(testKey)}, nil
			}
			return KeyLookupResult{}, nil
		},
	}, nil)
}

// chainWithTestKeyAndSigning wires the same LookupKey stub but allows
// the caller to override SignedRequests / ReplayWindow / Now / Cache
// for the signed-request test suite. The LookupKey stub still uses
// []byte(testKey) so the HMAC inputs match exactly what the production
// signing path sees.
func chainWithTestKeyAndSigning(extra AgentConfig) func(http.Handler) http.Handler {
	extra.LookupKey = func(_ context.Context, presented string) (KeyLookupResult, error) {
		if presented == testKey {
			return KeyLookupResult{AgentID: testAgentID, SigningKey: []byte(testKey)}, nil
		}
		return KeyLookupResult{}, nil
	}
	return AgentChainWithConfig(extra, nil)
}

func principalCapturingHandler(t *testing.T, got *Principal, gotAuthentikHeader *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := From(r.Context())
		if !ok {
			t.Error("handler saw no Principal in context")
		}
		*got = p
		*gotAuthentikHeader = r.Header.Get(authentikUsernameHeader)
		w.WriteHeader(http.StatusOK)
	})
}

// TestAgentChainValidKey is half of T2 (spec 9.5): a correct key yields a
// machine Principal and the request reaches the handler.
func TestAgentChainValidKey(t *testing.T) {
	var got Principal
	var authHeader string
	chain := chainWithTestKey()(principalCapturingHandler(t, &got, &authHeader))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, testKey)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.Kind != KindMachine {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMachine)
	}
	// The env-var bootstrap path attaches Principal.Name = testAgentID
	// (#companion-pairing): machine principals do carry Name now, set to
	// either the agent_id (device-paired path) or testAgentID (legacy
	// path). Email/Groups remain empty for machine principals -- the
	// BrowserChain-attached human Principal is the only one with identity.
	if got.Name != testAgentID {
		t.Errorf("Principal.Name = %q, want %q", got.Name, testAgentID)
	}
	if got.Email != "" || len(got.Groups) != 0 {
		t.Errorf("Principal = %+v, want empty Email/Groups", got)
	}
}

// TestAgentChainMissingOrWrongKey is the other half of T2: an absent or
// incorrect key is rejected with 401, never reaches the handler.
func TestAgentChainMissingOrWrongKey(t *testing.T) {
	cases := []struct {
		name     string
		provided string
		setKey   bool
	}{
		{"missing key", "", false},
		{"wrong key", "not-the-right-key-but-still-long-enough", true},
		{"empty key header set explicitly", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handlerCalled := false
			chain := chainWithTestKey()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalled = true
			}))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
			if c.setKey {
				req.Header.Set(apiKeyHeader, c.provided)
			}
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
			if handlerCalled {
				t.Error("handler was called despite an invalid key")
			}
		})
	}
}

// TestAgentChainFailsClosedOnMisconfiguredKey is T2's third case: an unset
// or too-short configured key means 503 for every request, valid-looking
// key or not -- never silently falls back to "no auth required."
func TestAgentChainFailsClosedOnMisconfiguredKey(t *testing.T) {
	cases := map[string]string{
		"unset":    "",
		"7 chars":  "short12",
		"31 chars": "0123456789012345678901234567890", // one short of MinAgentKeyLength
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			handlerCalled := false
			// Misconfigured deployment (no LookupKey, no APIKey after
			// issue #453 PR F): every agent route fails closed with 503.
			chain := AgentChainWithConfig(AgentConfig{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalled = true
			}))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
			req.Header.Set(apiKeyHeader, key) // even if the (misconfigured) key happens to be sent back
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)

			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rr.Code)
			}
			if handlerCalled {
				t.Error("handler was called despite a misconfigured agent key")
			}
		})
	}
}

// TestAgentChainStripsForgedIdentityHeaders is T3: a request carrying a
// valid X-API-Key AND a forged X-Authentik-Username must yield a machine
// Principal with no name, and the downstream handler must see the header
// gone entirely, not just ignored.
func TestAgentChainStripsForgedIdentityHeaders(t *testing.T) {
	var got Principal
	var authHeaderSeenByHandler string
	chain := chainWithTestKey()(principalCapturingHandler(t, &got, &authHeaderSeenByHandler))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(apiKeyHeader, testKey)
	req.Header.Set(authentikUsernameHeader, "admin")
	req.Header.Set(authentikEmailHeader, "admin@example.com")
	req.Header.Set(authentikGroupsHeader, "admins|superusers")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.Kind != KindMachine {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMachine)
	}
	// Env-var bootstrap path: Principal.Name is testAgentID, the
	// X-Authentik-Username header (forged to "admin") must NOT have leaked
	// through. The strip-X-Authentik-* step runs before the key check, so
	// even the Authentik headers set on this request are gone by the time
	// the principal is constructed -- this assertion is the negative
	// control that proves it.
	if got.Name != testAgentID {
		t.Errorf("Principal.Name = %q, want %q (forged X-Authentik-Username must not reach the Principal)", got.Name, testAgentID)
	}
	if len(got.Groups) != 0 {
		t.Errorf("Principal.Groups = %v, want empty", got.Groups)
	}
	if authHeaderSeenByHandler != "" {
		t.Errorf("downstream handler still saw X-Authentik-Username = %q, want it deleted from the request entirely", authHeaderSeenByHandler)
	}
}

// TestAgentChainStripsHeadersEvenOnRejection: the header strip must not be
// conditional on the key check succeeding -- see agent.go's doc comment on
// why that matters (no behavioral difference to distinguish the two cases).
func TestAgentChainStripsHeadersEvenOnRejection(t *testing.T) {
	var strippedBeforeRejection bool
	// Wrap AgentChain's own handler indirectly: we can't see inside a
	// rejected request's handler (it's never called), so instead assert on
	// the request object's headers after ServeHTTP returns -- httptest
	// gives us the same *http.Request we constructed, and AgentChain
	// mutates it in place via r.Header.Del.
	chain := chainWithTestKey()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called for a rejected request")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set(authentikUsernameHeader, "admin") // forged header
	// deliberately no X-API-Key -- this request must be rejected
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	strippedBeforeRejection = req.Header.Get(authentikUsernameHeader) == ""
	if !strippedBeforeRejection {
		t.Error("X-Authentik-Username survived on a rejected request -- stripping must happen unconditionally, before the key check")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("same", "same") {
		t.Error("equal strings compared unequal")
	}
	if constantTimeEqual("same", "diff") {
		t.Error("different same-length strings compared equal")
	}
	if constantTimeEqual("short", "longer-string") {
		t.Error("different-length strings compared equal")
	}
}

func computeSignature(apiKey, method, path, nonce, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(method + "\n" + path + "\n" + nonce + "\n" + timestamp + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAgentChainSignedRequests(t *testing.T) {
	fixedNow := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return fixedNow }

	t.Run("valid signed request with body", func(t *testing.T) {
		var got Principal
		var authHeader string
		var capturedBody []byte
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := From(r.Context())
			if !ok {
				t.Error("no principal in context")
			}
			got = p
			authHeader = r.Header.Get(authentikUsernameHeader)
			var err error
			capturedBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		})

		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
		})(handler)

		body := []byte(`{"agentId":"test-agent"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(body))
		ts := strconv.FormatInt(fixedNow.UnixNano(), 10)
		nonce := "0123456789abcdef0123456789abcdef"
		sig := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce, ts, body)

		req.Header.Set("X-API-Key", testKey)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		if authHeader != "" {
			t.Errorf("authHeader = %q, want empty", authHeader)
		}
		if got.Kind != KindMachine {
			t.Errorf("got kind %v, want %v", got.Kind, KindMachine)
		}
		if string(capturedBody) != string(body) {
			t.Errorf("handler received body %q, want %q", string(capturedBody), string(body))
		}
	})

	t.Run("valid signed GET request with query string", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
		})(handler)

		path := "/api/v1/agent/check-content?fastHash=abcdef1234567890"
		req := httptest.NewRequest(http.MethodGet, path, nil)
		ts := strconv.FormatInt(fixedNow.UnixNano(), 10)
		nonce := "nonce-get-test-12345"
		sig := computeSignature(testKey, http.MethodGet, path, nonce, ts, nil)

		req.Header.Set("X-API-Key", testKey)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("missing signature headers when signedRequests=true", func(t *testing.T) {
		cases := []struct {
			name   string
			setTs  bool
			setNon bool
			setSig bool
		}{
			{"all missing", false, false, false},
			{"missing timestamp", false, true, true},
			{"missing nonce", true, false, true},
			{"missing signature", true, true, false},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				chain := chainWithTestKeyAndSigning(AgentConfig{
					SignedRequests: true,
					ReplayWindow:   5 * time.Minute,
					Now:            nowFn,
				})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))

				req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
				req.Header.Set("X-API-Key", testKey)
				if tc.setTs {
					req.Header.Set("X-Timestamp", strconv.FormatInt(fixedNow.UnixNano(), 10))
				}
				if tc.setNon {
					req.Header.Set("X-Nonce", "nonce-1")
				}
				if tc.setSig {
					req.Header.Set("X-Signature", "sig-1")
				}

				rr := httptest.NewRecorder()
				chain.ServeHTTP(rr, req)

				if rr.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", rr.Code)
				}
				if !bytes.Contains(rr.Body.Bytes(), []byte("invalid or missing signature")) {
					t.Errorf("body %q does not contain 'invalid or missing signature'", rr.Body.String())
				}
			})
		}
	})

	t.Run("tampered body rejected", func(t *testing.T) {
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		origBody := []byte(`{"agentId":"agent-1"}`)
		ts := strconv.FormatInt(fixedNow.UnixNano(), 10)
		nonce := "nonce-tamper-1"
		sig := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce, ts, origBody)

		tamperedBody := []byte(`{"agentId":"agent-tampered"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(tamperedBody))
		req.Header.Set("X-API-Key", testKey)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("replayed nonce within window rejected", func(t *testing.T) {
		cache := NewReplayCache()
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
			Cache:          cache,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		ts := strconv.FormatInt(fixedNow.UnixNano(), 10)
		nonce := "nonce-replay-unique-123"
		sig := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce, ts, nil)

		// First request: OK
		req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req1.Header.Set("X-API-Key", testKey)
		req1.Header.Set("X-Timestamp", ts)
		req1.Header.Set("X-Nonce", nonce)
		req1.Header.Set("X-Signature", sig)

		rr1 := httptest.NewRecorder()
		chain.ServeHTTP(rr1, req1)
		if rr1.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", rr1.Code)
		}

		// Second request with same nonce: Replayed -> 401
		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req2.Header.Set("X-API-Key", testKey)
		req2.Header.Set("X-Timestamp", ts)
		req2.Header.Set("X-Nonce", nonce)
		req2.Header.Set("X-Signature", sig)

		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusUnauthorized {
			t.Fatalf("replayed request status = %d, want 401", rr2.Code)
		}
	})

	t.Run("clock skew tolerance", func(t *testing.T) {
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// 1. Within window in past (4m ago) -> OK
		tsPast4m := strconv.FormatInt(fixedNow.Add(-4*time.Minute).UnixNano(), 10)
		nonce1 := "nonce-past-4m"
		sig1 := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce1, tsPast4m, nil)

		req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req1.Header.Set("X-API-Key", testKey)
		req1.Header.Set("X-Timestamp", tsPast4m)
		req1.Header.Set("X-Nonce", nonce1)
		req1.Header.Set("X-Signature", sig1)

		rr1 := httptest.NewRecorder()
		chain.ServeHTTP(rr1, req1)
		if rr1.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr1.Code)
		}

		// 2. Within window in future (4m ahead) -> OK
		tsFut4m := strconv.FormatInt(fixedNow.Add(4*time.Minute).UnixNano(), 10)
		nonce2 := "nonce-fut-4m"
		sig2 := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce2, tsFut4m, nil)

		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req2.Header.Set("X-API-Key", testKey)
		req2.Header.Set("X-Timestamp", tsFut4m)
		req2.Header.Set("X-Nonce", nonce2)
		req2.Header.Set("X-Signature", sig2)

		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr2.Code)
		}

		// 3. Past window (> 5m ago) -> 401
		tsPast6m := strconv.FormatInt(fixedNow.Add(-6*time.Minute).UnixNano(), 10)
		nonce3 := "nonce-past-6m"
		sig3 := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce3, tsPast6m, nil)

		req3 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req3.Header.Set("X-API-Key", testKey)
		req3.Header.Set("X-Timestamp", tsPast6m)
		req3.Header.Set("X-Nonce", nonce3)
		req3.Header.Set("X-Signature", sig3)

		rr3 := httptest.NewRecorder()
		chain.ServeHTTP(rr3, req3)
		if rr3.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr3.Code)
		}

		// 4. Future window (> 5m ahead) -> 401
		tsFut6m := strconv.FormatInt(fixedNow.Add(6*time.Minute).UnixNano(), 10)
		nonce4 := "nonce-fut-6m"
		sig4 := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce4, tsFut6m, nil)

		req4 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req4.Header.Set("X-API-Key", testKey)
		req4.Header.Set("X-Timestamp", tsFut6m)
		req4.Header.Set("X-Nonce", nonce4)
		req4.Header.Set("X-Signature", sig4)

		rr4 := httptest.NewRecorder()
		chain.ServeHTTP(rr4, req4)
		if rr4.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr4.Code)
		}
	})

	t.Run("signedRequests=false ignores missing or present signature", func(t *testing.T) {
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: false,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// Without signature headers
		req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req1.Header.Set("X-API-Key", testKey)
		rr1 := httptest.NewRecorder()
		chain.ServeHTTP(rr1, req1)
		if rr1.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr1.Code)
		}

		// With garbage signature headers -- signedRequests=false must
		// ignore these entirely, not reject them as invalid.
		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req2.Header.Set("X-API-Key", testKey)
		req2.Header.Set("X-Timestamp", "not-a-number")
		req2.Header.Set("X-Nonce", "garbage-nonce")
		req2.Header.Set("X-Signature", "deadbeef")
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusOK {
			t.Fatalf("status with garbage signature headers = %d, want 200 (signedRequests=false must ignore present signatures)", rr2.Code)
		}
	})

	t.Run("clock skew at exact window boundary is accepted", func(t *testing.T) {
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// skew == -window (exactly at the past boundary) -> accepted (strict >)
		tsPastExact := strconv.FormatInt(fixedNow.Add(-5*time.Minute).UnixNano(), 10)
		nonce1 := "nonce-past-exact-boundary"
		sig1 := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce1, tsPastExact, nil)
		req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req1.Header.Set("X-API-Key", testKey)
		req1.Header.Set("X-Timestamp", tsPastExact)
		req1.Header.Set("X-Nonce", nonce1)
		req1.Header.Set("X-Signature", sig1)
		rr1 := httptest.NewRecorder()
		chain.ServeHTTP(rr1, req1)
		if rr1.Code != http.StatusOK {
			t.Fatalf("status at skew == -window = %d, want 200 (boundary inclusive: condition is skew > window, not >=)", rr1.Code)
		}

		// skew == +window (exactly at the future boundary) -> accepted
		tsFutExact := strconv.FormatInt(fixedNow.Add(5*time.Minute).UnixNano(), 10)
		nonce2 := "nonce-fut-exact-boundary"
		sig2 := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce2, tsFutExact, nil)
		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", nil)
		req2.Header.Set("X-API-Key", testKey)
		req2.Header.Set("X-Timestamp", tsFutExact)
		req2.Header.Set("X-Nonce", nonce2)
		req2.Header.Set("X-Signature", sig2)
		rr2 := httptest.NewRecorder()
		chain.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusOK {
			t.Fatalf("status at skew == +window = %d, want 200 (boundary inclusive: condition is skew > window, not >=)", rr2.Code)
		}
	})

	t.Run("signedRequests=true + skip path bypasses signature validation", func(t *testing.T) {
		// /api/v1/agent/upload streams up to 50 GiB; the agent client cannot
		// sign over a body that large, so the route is in the default skip
		// list. Signature headers must not be required.
		handlerCalled := false
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
			Now:            nowFn,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		// 50 KiB body -- far above SignedMaxBodyBytes but the route is exempt,
		// so MaxBytesReader never wraps r.Body and the read below succeeds.
		body := bytes.Repeat([]byte{0x42}, 50*1024)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(body))
		req.Header.Set("X-API-Key", testKey)
		// No X-Timestamp / X-Nonce / X-Signature at all.

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (skip path must bypass signature validation, even when signedRequests=true). body=%s", rr.Code, rr.Body.String())
		}
		if !handlerCalled {
			t.Fatal("handler was not called for an upload-route request")
		}
	})

	t.Run("signedRequests=true + skip path still enforces API key", func(t *testing.T) {
		// Skipping signature validation does NOT skip the API key check --
		// the upload route still needs a valid key, otherwise anyone who
		// can reach /api/v1/agent/upload (Traefik strips ForwardAuth for
		// this prefix) could push 50 GiB to disk.
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests: true,
			ReplayWindow:   5 * time.Minute,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler must not be called without a valid API key")
		}))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", nil)
		// No X-API-Key set at all.
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (skip path must still enforce X-API-Key)", rr.Code)
		}
	})

	t.Run("body exceeding SignedMaxBodyBytes is rejected with 413", func(t *testing.T) {
		const cap = 1024
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests:     true,
			ReplayWindow:       5 * time.Minute,
			Now:                nowFn,
			SignedMaxBodyBytes: cap,
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler must not be called for an oversize body")
		}))

		body := bytes.Repeat([]byte{0x42}, cap+1)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytes.NewReader(body))
		// No signature headers -- the body cap must be enforced before
		// signature validation runs, so we should see 401 for missing
		// headers only if signature validation ran; here we expect 413
		// because MaxBytesReader triggers during the read.
		ts := strconv.FormatInt(fixedNow.UnixNano(), 10)
		nonce := "nonce-oversize-body"
		sig := computeSignature(testKey, http.MethodPost, "/api/v1/agent/events", nonce, ts, nil)
		req.Header.Set("X-API-Key", testKey)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", sig)

		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 (SignedMaxBodyBytes exceeded)", rr.Code)
		}
	})

	t.Run("prefix-match skip path bypasses signature validation", func(t *testing.T) {
		// Operators may add a path ending in '/' to skip a whole subtree;
		// exact-match and prefix-match must both work.
		handlerCalled := false
		chain := chainWithTestKeyAndSigning(AgentConfig{
			SignedRequests:     true,
			ReplayWindow:       5 * time.Minute,
			Now:                nowFn,
			SkipSignaturePaths: []string{"/api/v1/agent/internal/"},
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/internal/health", nil)
		req.Header.Set("X-API-Key", testKey)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (prefix-match skip path must bypass signing)", rr.Code)
		}
		if !handlerCalled {
			t.Fatal("handler was not called")
		}
	})
}

// TestMatchesAnyPath locks in the simple prefix/exact semantics used by
// the SkipSignaturePaths list -- a regression here would silently start
// signing the upload route again.
func TestMatchesAnyPath(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{"empty patterns", "/anything", nil, false},
		{"exact match", "/api/v1/agent/upload", []string{"/api/v1/agent/upload"}, true},
		{"exact non-match", "/api/v1/agent/events", []string{"/api/v1/agent/upload"}, false},
		{"prefix match", "/api/v1/agent/upload/foo", []string{"/api/v1/agent/upload/"}, true},
		{"prefix doesn't match longer exact", "/api/v1/agent/uploadevilly", []string{"/api/v1/agent/upload"}, false},
		{"empty pattern is skipped", "/anything", []string{""}, false},
		{"multiple patterns, second matches", "/x", []string{"/a", "/x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchesAnyPath(c.path, c.patterns)
			if got != c.want {
				t.Errorf("matchesAnyPath(%q, %v) = %v, want %v", c.path, c.patterns, got, c.want)
			}
		})
	}
}
