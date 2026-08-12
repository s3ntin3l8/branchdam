package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testKey = "01234567890123456789012345678901" // 33 chars, >= minAgentKeyLength

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
	chain := AgentChain(testKey, nil)(principalCapturingHandler(t, &got, &authHeader))

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
	if got.Name != "" || got.Email != "" || len(got.Groups) != 0 {
		t.Errorf("Principal = %+v, want empty Name/Email/Groups", got)
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
			chain := AgentChain(testKey, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		"31 chars": "0123456789012345678901234567890", // one short of minAgentKeyLength
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			handlerCalled := false
			chain := AgentChain(key, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	chain := AgentChain(testKey, nil)(principalCapturingHandler(t, &got, &authHeaderSeenByHandler))

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
	if got.Name != "" {
		t.Errorf("Principal.Name = %q, want empty -- the forged X-Authentik-Username must not reach the Principal", got.Name)
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
	chain := AgentChain(testKey, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
