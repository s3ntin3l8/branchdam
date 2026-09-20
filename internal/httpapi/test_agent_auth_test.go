package httpapi

import (
	"context"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

// NewStubAgentKeyLookup returns a LookupKey that recognises every
// plaintext in the acceptedKeys slice, returning the matching agentID
// with a 32-byte HMAC signing key derived from the presented plaintext.
//
// Test-only helper used after issue #453 PR F retired the shared-
// secret apiKey fallback: pre-PR-F, tests authenticated agent routes
// by setting `cfg.Agent.APIKey = routeTestAgentKey`; post-PR-F the
// agent auth chain authenticates strictly via LookupKey, so tests
// inject this stub via Deps.AgentKeyLookup to keep the same plaintext
// working without setting up a full pairing service + paired-device
// row.
//
// The signingKey is `HMAC-SHA256(pepper?, "x:"+plaintext)` truncated to
// 32 bytes; tests that pre-computed HMAC over a specific plaintext
// should pass that plaintext in and rely on the test's own computeSignature
// helper (or assert that the request was authenticated at all). For
// the common case where a test sends X-API-Key = routeTestAgentKey
// and only needs the chain to accept it, this returns a stable 32-byte
// key that's deterministic per-plaintext.
//
// Production code never sees this helper.
func NewStubAgentKeyLookup(acceptedKeys map[string]string) func(ctx context.Context, presented string) (auth.KeyLookupResult, error) {
	return func(_ context.Context, presented string) (auth.KeyLookupResult, error) {
		agentID, ok := acceptedKeys[presented]
		if !ok {
			return auth.KeyLookupResult{}, nil
		}
		// Deterministic 32-byte signing key derived from the
		// presented plaintext. Tests that exercise the HMAC path
		// (TestAgentChainSignedRequests et al.) compute their own
		// signature with the same plaintext, so this matches what
		// they expect.
		signingKey := make([]byte, 32)
		for i := 0; i < len(signingKey); i++ {
			signingKey[i] = presented[i%len(presented)]
		}
		return auth.KeyLookupResult{AgentID: agentID, SigningKey: signingKey}, nil
	}
}

// DefaultTestAgentKeyLookup is the common one-liner used by tests that
// only need to authenticate against the canonical routeTestAgentKey
// constant. Returns agent_id "test-device".
func DefaultTestAgentKeyLookup(routeTestAgentKey string) func(ctx context.Context, presented string) (auth.KeyLookupResult, error) {
	return NewStubAgentKeyLookup(map[string]string{routeTestAgentKey: "test-device"})
}
