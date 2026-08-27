package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
)

type errorResponse struct {
	Schema string `json:"$schema"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func writeForbidden(w http.ResponseWriter, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Schema: "https://huma.rocks/schema/error.json",
		Title:  "Forbidden",
		Status: http.StatusForbidden,
		Detail: detail,
	})
}

// RequireAdmin returns a middleware gating mutating (write) actions to users belonging
// to at least one group in allowedGroups.
//
// Read methods (GET, HEAD, OPTIONS) and machine principals (agent path) are permitted unconditionally
// for any authenticated principal. If allowedGroups is empty, all authenticated users have write access,
// and a WARN log naming authz.groups is emitted upon middleware creation.
func RequireAdmin(allowedGroups []string, log *slog.Logger) func(http.Handler) http.Handler {
	allowedGroups = slices.Clone(allowedGroups)
	if len(allowedGroups) == 0 && log != nil {
		log.Warn("authz.groups is empty: all authenticated users have admin access", "key", "authz.groups")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := From(r.Context())
			if !ok {
				writeForbidden(w, "authentication required")
				return
			}

			// Machine principals (agent path) skip group checks.
			if p.Kind == KindMachine {
				next.ServeHTTP(w, r)
				return
			}

			// Read methods (GET, HEAD, OPTIONS) are open to any authenticated principal.
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			// #164: a browser principal with no X-Authentik-Username at all
			// (misconfigured ForwardAuth, or a direct hit on this port) must
			// not reach the group check below as if it were a real logged-in
			// user with no matching group -- that would grant full write
			// access whenever allowedGroups is empty (the solo-homelab
			// default). Reads are unaffected: this check runs after the
			// GET/HEAD/OPTIONS bypass above, not before it.
			if !p.Authenticated {
				writeForbidden(w, "authentication required")
				return
			}

			if IsAdmin(p, allowedGroups) {
				next.ServeHTTP(w, r)
				return
			}

			writeForbidden(w, "admin authorization required")
		})
	}
}

// IsAdmin reports whether p satisfies the same admin policy RequireAdmin
// enforces on a mutating request: a user principal, authenticated, and
// either allowedGroups is empty (the solo-homelab default: every
// authenticated user is admin) or p is a member of at least one of them.
//
// This does NOT reproduce RequireAdmin's GET/HEAD/OPTIONS bypass or its
// unconditional pass for KindMachine -- those are properties of *which
// requests* RequireAdmin's middleware gates at all, not of what "admin"
// means once a request is gated. A caller wanting a stricter policy (e.g.
// internal/httpapi's settings routes, which must reject KindMachine and gate
// GET too, unlike every other browser-routed read) calls this directly with
// its own Kind/method checks around it, rather than reusing the middleware.
func IsAdmin(p Principal, allowedGroups []string) bool {
	if p.Kind != KindUser || !p.Authenticated {
		return false
	}
	if len(allowedGroups) == 0 {
		return true
	}
	return slices.ContainsFunc(allowedGroups, func(g string) bool {
		return slices.Contains(p.Groups, g)
	})
}
