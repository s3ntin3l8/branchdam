package auth

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// minAgentKeyLength matches config.example.yaml's documented requirement.
// A shorter (or unset) key fails every agent request closed rather than
// accepting a weak or empty secret.
const minAgentKeyLength = 32

const apiKeyHeader = "X-API-Key"

// AgentChain authenticates /api/v1/agent/* requests against a static
// shared secret (spec §5) and attaches a machine Principal. Two properties
// hold regardless of what happens downstream:
//
//  1. Every X-Authentik-* header is deleted from the request BEFORE the key
//     is even checked -- unconditionally, not just on success. The agent
//     router bypasses Traefik's ForwardAuth middleware by design (spec
//     §5), so nothing upstream strips a client-supplied
//     X-Authentik-Username; if this chain didn't, a request straight to
//     this router could forge one. Stripping first, regardless of outcome,
//     means the observable behavior of this middleware never depends on
//     whether those headers were present -- there's no timing or
//     error-branch difference to distinguish "stripped because valid" from
//     "stripped because rejected."
//  2. The resulting Principal (on success) has Kind: KindMachine and
//     empty Name/Email/Groups -- see principal.go's doc comment for why.
//
// keyConfigured reports whether apiKey met minAgentKeyLength at
// construction time; log records that fact ONCE here, at chain-build time
// (server startup), rather than on every rejected request.
func AgentChain(apiKey string, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	keyConfigured := len(apiKey) >= minAgentKeyLength
	if !keyConfigured {
		log.Warn("auth: BRANCHDAM_AGENT_API_KEY is unset or shorter than the minimum length -- agent routes will fail closed with 503 until it is fixed", "minLength", minAgentKeyLength)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripAuthentikHeaders(r)

			if !keyConfigured {
				http.Error(w, "agent authentication is not configured", http.StatusServiceUnavailable)
				return
			}

			provided := r.Header.Get(apiKeyHeader)
			if provided == "" || !constantTimeEqual(provided, apiKey) {
				http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
				return
			}

			principal := Principal{Kind: KindMachine}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
		})
	}
}

// stripAuthentikHeaders deletes every header whose canonical name starts
// with "X-Authentik-", regardless of how many were sent or what they
// contained. Canonical form (http.CanonicalHeaderKey, which r.Header.Get/
// Del already apply) means this catches "x-authentik-username",
// "X-AUTHENTIK-USERNAME", etc. -- HTTP header names are case-insensitive on
// the wire, and Go's http.Header always stores them canonicalized.
func stripAuthentikHeaders(r *http.Request) {
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-authentik-") {
			r.Header.Del(name)
		}
	}
}

// constantTimeEqual reports whether a and b are equal without leaking
// timing information about *where* they first differ. subtle.
// ConstantTimeCompare requires equal-length inputs to make that guarantee;
// the length check itself is not constant-time, but leaking "the provided
// key's length doesn't match" is not a meaningful side channel for a fixed
// shared secret an attacker cannot otherwise probe the length of.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
