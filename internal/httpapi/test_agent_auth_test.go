package httpapi

import (
	"context"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

// NewStubAgentKeyLookup returns a LookupKey that recognises every
// plaintext in the acceptedKeys slice, returning the matching agentID
// with the production signing key for the presented plaintext
// (auth.SigningKeyForAPIKey -- the same function
// internal/pairing.Service.KeyLookup uses).
//
// Test-only helper used after issue #453 PR F retired the shared-
// secret apiKey fallback: pre-PR-F, tests authenticated agent routes
// by setting `cfg.Agent.APIKey = routeTestAgentKey`; post-PR-F the
// agent auth chain authenticates strictly via LookupKey, so tests
// inject this stub via Deps.agentKeyLookup to keep the same plaintext
// working without setting up a full pairing service + paired-device
// row.
//
// The signing key deliberately comes from the production derivation
// rather than a test-local one: an earlier stub padded the plaintext to
// 32 bytes, which for the 32-character routeTestAgentKey happened to
// equal the raw key the tests signed with, and so hid that production
// derived a pepper-keyed value no client could compute. See
// agent_signing_e2e_test.go for the end-to-end check against the real
// pairing service.
//
// Production code never sees this helper.
func NewStubAgentKeyLookup(acceptedKeys map[string]string) func(ctx context.Context, presented string) (auth.KeyLookupResult, error) {
	return func(_ context.Context, presented string) (auth.KeyLookupResult, error) {
		agentID, ok := acceptedKeys[presented]
		if !ok {
			return auth.KeyLookupResult{}, nil
		}
		return auth.KeyLookupResult{AgentID: agentID, SigningKey: auth.SigningKeyForAPIKey(presented)}, nil
	}
}

// DefaultTestAgentKeyLookup is the common one-liner used by tests that
// only need to authenticate against the canonical routeTestAgentKey
// constant. Returns agent_id "test-device".
func DefaultTestAgentKeyLookup(routeTestAgentKey string) func(ctx context.Context, presented string) (auth.KeyLookupResult, error) {
	return NewStubAgentKeyLookup(map[string]string{routeTestAgentKey: "test-device"})
}
