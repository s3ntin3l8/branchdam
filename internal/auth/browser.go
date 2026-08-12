package auth

import (
	"net/http"
	"strings"
)

// Authentik's outpost headers (spec §5). Groups are pipe-delimited -- this
// is Authentik's own ForwardAuth format, NOT the comma-separated format
// some of this project's other config values use (e.g. a future
// FORWARD_AUTH_ALLOWED_GROUPS env var); mixing the two up silently drops
// every group after the first comma.
const (
	authentikUsernameHeader = "X-Authentik-Username"
	authentikEmailHeader    = "X-Authentik-Email"
	authentikGroupsHeader   = "X-Authentik-Groups"
)

// BrowserChain reads Authentik's identity headers and attaches a user
// Principal to the request context. This is the ONLY function in the
// codebase permitted to read X-Authentik-* -- see the package doc and
// TestNoDirectAuthentikHeaderReads. It trusts these headers unconditionally
// because Traefik's ForwardAuth middleware is what should be gating this
// route in production (docs/forward-auth.md); BrowserChain itself has no
// way to verify that placement, which is exactly why AgentChain strips
// these headers on the router that bypasses ForwardAuth.
func BrowserChain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := Principal{
			Kind:   KindUser,
			Name:   r.Header.Get(authentikUsernameHeader),
			Email:  r.Header.Get(authentikEmailHeader),
			Groups: splitGroups(r.Header.Get(authentikGroupsHeader)),
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
}

func splitGroups(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, "|")
	groups := make([]string, 0, len(parts))
	for _, g := range parts {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}
