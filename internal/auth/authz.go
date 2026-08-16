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

			// Empty allowedGroups permits all authenticated users.
			if len(allowedGroups) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Check if user has at least one of the allowed admin groups.
			for _, g := range p.Groups {
				if slices.Contains(allowedGroups, g) {
					next.ServeHTTP(w, r)
					return
				}
			}

			writeForbidden(w, "admin authorization required")
		})
	}
}
