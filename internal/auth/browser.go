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
	// authentikUidHeader is the stable per-user opaque id Authentik
	// exposes; the multi-user attribution layer uses it as the
	// users.external_uid key so an Authentik username rename doesn't
	// fragment attribution rows (the display username stays in the
	// denormalized `users.username` column, refreshed on every login).
	authentikUidHeader = "X-Authentik-Uid"
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
		name := r.Header.Get(authentikUsernameHeader)
		principal := Principal{
			Kind:   KindUser,
			Name:   name,
			Email:  r.Header.Get(authentikEmailHeader),
			Groups: splitGroups(r.Header.Get(authentikGroupsHeader)),
			// ExternalUID is the stable per-user key used by
			// internal/users.ResolveOrCreate. Falls back to Name when
			// ForwardAuth doesn't expose X-Authentik-Uid (older Authentik
			// deployments, dev setups that skip Traefik). That fallback
			// degrades attribution under username renames but keeps the
			// system working.
			ExternalUID: pickExternalUID(r.Header.Get(authentikUidHeader), name),
			// #164: a request with none of the expected Authentik headers
			// (misconfigured ForwardAuth, or a direct hit on this port) must
			// not read as "authenticated with an empty username" on a write
			// route -- see RequireAdmin.
			Authenticated: name != "",
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
}

// pickExternalUID returns the Authentik-issued stable id, or falls back
// to the username when the deployment doesn't forward X-Authentik-Uid.
func pickExternalUID(uid, name string) string {
	if uid != "" {
		return uid
	}
	return name
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
