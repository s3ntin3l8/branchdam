package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/clock"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

const oauthProtectedResourcePath = "/.well-known/oauth-protected-resource"

// oauthMetadataPaths are the exact documents this process serves.
// The path-inserted form (RFC 9728) for the /mcp resource is the
// third and fourth. Nothing else under the well-known prefix is public.
func oauthMetadataPaths() []string {
	return []string{
		oauthProtectedResourcePath,
		oauthProtectedResourcePath + "/",
		oauthProtectedResourcePath + "/mcp",
		oauthProtectedResourcePath + "/mcp/",
	}
}

func (s *Server) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	if s.mcpOAuth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(s.mcpOAuth.Metadata()); err != nil {
		s.log.Warn("mcp oauth: write metadata", "error", err)
	}
}

// authenticateMCPOAuth is the /mcp path for a bearer that is not a
// personal access token. It calls next with an OAuth principal, or
// writes 401/403 itself. It does not create users.
func (s *Server) authenticateMCPOAuth(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if s.mcpOAuth == nil || s.db == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	meta := s.mcpOAuth.MetadataURL()
	raw := oauthBearer(r)
	if raw == "" {
		auth.OAuthUnauthorized(w, meta, false)
		return
	}
	ident, err := s.mcpOAuth.Validate(r.Context(), raw, clock.Real())
	if err != nil {
		s.log.Warn("mcp oauth: rejected token", "error", err)
		auth.OAuthUnauthorized(w, meta, true)
		return
	}
	row, err := s.db.Reader.GetAttributionUserByExternalUID(r.Context(), sqlcgen.GetAttributionUserByExternalUIDParams{
		AuthProvider: attributionusers.AuthProviderAuthentik,
		ExternalUid:  ident.Subject,
	})
	if errors.Is(err, sql.ErrNoRows) {
		auth.OAuthForbidden(w, meta, false)
		return
	}
	if err != nil {
		s.log.Warn("mcp oauth: user lookup", "error", err)
		http.Error(w, "authentication required", http.StatusInternalServerError)
		return
	}
	scopes := auth.MapOAuthScopes(ident.Claims, s.mcpOAuth.ScopeClaims())
	if len(scopes) == 0 {
		auth.OAuthForbidden(w, meta, true)
		return
	}
	email := ""
	if row.Email.Valid {
		email = row.Email.String
	}
	p := auth.Principal{
		Kind:          auth.KindUser,
		Name:          row.Username,
		Email:         email,
		ExternalUID:   row.ExternalUid,
		Authenticated: true,
		AuthProvider:  auth.AuthProviderOAuth,
		UserID:        row.ID,
	}
	ctx := auth.ContextWithOAuth(r.Context(), p, scopes, row.IsAdmin != 0)
	next.ServeHTTP(w, r.WithContext(ctx))
}

func oauthBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
