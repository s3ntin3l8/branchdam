package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// PAT endpoints (issue #453 PR E). Per-user admin PATs for unattended
// operator tooling. Routes:
//   POST   /api/v1/users/me/pats      -- mint (returns plaintext once)
//   GET    /api/v1/users/me/pats      -- list (no plaintext)
//   POST   /api/v1/users/me/pats/{id}/revoke -- soft-delete (404 when
//                                         the id matches no live PAT
//                                         owned by the caller)
//
// All three require RequireAdmin (only admins mint/revoke/list admin
// PATs -- regular users don't have PATs) plus the "pats:write" scope
// when the caller is a PAT (see patScopeFor in server.go). The PAT
// middleware (auth.PATMiddleware.RequirePAT) is wired into these
// routes so an admin's PAT can hit them too; a non-admin PAT gets
// 403 from the RequireAdmin check that runs after RequirePAT
// attaches its Principal.

// --- registration ---

// registerPats wires the admin PAT endpoints (issue #453 PR E).
// Per-user admin PATs for unattended operator tooling -- the
// kubeadm-init bootstrap pattern lives in cmd/branchdam's startup
// hook; these endpoints let admins mint/revoke/list scoped tokens
// via the API itself. Lives in its own registrar (not
// registerCompanionPairings) so each route family's registration
// matches its file, mirroring the Pairing wiring shape. Nil-safe:
// the routes only exist when the PAT service is configured.
func (s *Server) registerPats(api huma.API) {
	if s.patService == nil {
		return
	}
	huma.Post(api, "/api/v1/users/me/pats", s.handleCreatePAT)
	huma.Get(api, "/api/v1/users/me/pats", s.handleListPATs)
	huma.Post(api, "/api/v1/users/me/pats/{id}/revoke", s.handleRevokePAT)
}

type CreatePATInput struct {
	Body struct {
		Name string `json:"name" required:"true"`
		// Scopes is the list of capabilities the PAT carries; an admin
		// PAT for the kubeadm-init bootstrap path carries ["*"]. For a
		// workstation-provisioning PAT, ["pairings:write"] is typical.
		// The middleware compares against this list on every request.
		Scopes []string `json:"scopes" required:"true"`
		// ExpiresAt is unix-seconds; omit (or zero) for a non-expiring
		// PAT. Useful for "service-account" tokens operators want to
		// rotate on a schedule rather than rely on revocation.
		ExpiresAt int64 `json:"expiresAt,omitempty"`
	}
}

type CreatePATOutput struct {
	Body struct {
		// Plaintext is shown to the operator exactly once and never
		// re-served. Subsequent GETs return only metadata (no plaintext
		// field). Mirror the kubeadm-init bootstrap-token UX: "this is
		// your only chance to copy it".
		Plaintext string   `json:"plaintext"`
		ID        int64    `json:"id"`
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		CreatedAt int64    `json:"createdAt"`
		// HashedKeyPrefix is the first 8 hex chars of the stored hash
		// -- enough to make this token distinguishable in audit logs
		// and actor_audit details_json without leaking the full hash.
		HashedKeyPrefix string `json:"hashedKeyPrefix"`
	}
}

func (s *Server) handleCreatePAT(ctx context.Context, in *CreatePATInput) (*CreatePATOutput, error) {
	p, ok := auth.From(ctx)
	if !ok || p.Kind != auth.KindUser {
		return nil, huma.Error403Forbidden("admin user principal required", nil)
	}
	uid, err := s.resolveCallerUserID(ctx)
	if err != nil {
		return nil, huma.Error403Forbidden("could not resolve user id", err)
	}

	plaintext, row, err := s.patService.Mint(ctx, uid, in.Body.Name, in.Body.Scopes, in.Body.ExpiresAt)
	if err != nil {
		return nil, huma.Error500InternalServerError("mint PAT", err)
	}

	// Audit: every PAT mint is an admin action worth recording.
	if s.audit != nil {
		details := map[string]any{
			"name":              row.Name,
			"scopes":            in.Body.Scopes,
			"hashed_key_prefix": row.HashedKey[:8],
		}
		if err := s.audit.WriteActorAudit(ctx, p, audit.EventPATMinted, "user_pat", fmt.Sprintf("%d", row.ID), details); err != nil {
			s.log.Warn("failed to write actor audit for PAT mint", "error", err)
		}
	}

	out := &CreatePATOutput{}
	out.Body.Plaintext = plaintext
	out.Body.ID = row.ID
	out.Body.Name = row.Name
	// Re-decode scopes from the row's JSON for the response (the
	// mint accepts []string; the row stores JSON).
	scopes, _ := decodeScopesFromJSON(row.ScopesJson)
	out.Body.Scopes = scopes
	out.Body.CreatedAt = row.CreatedAt
	out.Body.HashedKeyPrefix = row.HashedKey[:8]
	return out, nil
}

type ListPATsInput struct {
	Limit  int64 `query:"limit" default:"50" minimum:"1" maximum:"200"`
	Offset int64 `query:"offset" default:"0" minimum:"0"`
}

type ListPATsOutput struct {
	Body struct {
		PATs  []patDTO `json:"pats"`
		Total int64    `json:"total"`
	}
}

type patDTO struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Scopes          []string `json:"scopes"`
	CreatedAt       int64    `json:"createdAt"`
	LastUsedAt      *int64   `json:"lastUsedAt,omitempty"`
	ExpiresAt       *int64   `json:"expiresAt,omitempty"`
	RevokedAt       *int64   `json:"revokedAt,omitempty"`
	HashedKeyPrefix string   `json:"hashedKeyPrefix"`
}

func (s *Server) handleListPATs(ctx context.Context, in *ListPATsInput) (*ListPATsOutput, error) {
	p, ok := auth.From(ctx)
	if !ok || p.Kind != auth.KindUser {
		return nil, huma.Error403Forbidden("admin user principal required", nil)
	}
	uid, err := s.resolveCallerUserID(ctx)
	if err != nil {
		return nil, huma.Error403Forbidden("could not resolve user id", err)
	}

	rows, err := s.db.Reader.ListUserPATs(ctx, sqlcgen.ListUserPATsParams{
		UserID: uid,
		Limit:  in.Limit,
		Offset: in.Offset,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("list PATs", err)
	}
	total, err := s.db.Reader.CountUserPATs(ctx, uid)
	if err != nil {
		return nil, huma.Error500InternalServerError("count PATs", err)
	}

	out := &ListPATsOutput{}
	out.Body.PATs = make([]patDTO, 0, len(rows))
	for _, row := range rows {
		scopes, _ := decodeScopesFromJSON(row.ScopesJson)
		dto := patDTO{
			ID:              row.ID,
			Name:            row.Name,
			Scopes:          scopes,
			CreatedAt:       row.CreatedAt,
			HashedKeyPrefix: row.HashedKey[:8],
		}
		if row.LastUsedAt.Valid {
			v := row.LastUsedAt.Int64
			dto.LastUsedAt = &v
		}
		if row.ExpiresAt.Valid {
			v := row.ExpiresAt.Int64
			dto.ExpiresAt = &v
		}
		if row.RevokedAt.Valid {
			v := row.RevokedAt.Int64
			dto.RevokedAt = &v
		}
		out.Body.PATs = append(out.Body.PATs, dto)
	}
	out.Body.Total = total
	return out, nil
}

type RevokePATInput struct {
	ID int64 `path:"id"`
}

type RevokePATOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func (s *Server) handleRevokePAT(ctx context.Context, in *RevokePATInput) (*RevokePATOutput, error) {
	p, ok := auth.From(ctx)
	if !ok || p.Kind != auth.KindUser {
		return nil, huma.Error403Forbidden("admin user principal required", nil)
	}
	uid, err := s.resolveCallerUserID(ctx)
	if err != nil {
		return nil, huma.Error403Forbidden("could not resolve user id", err)
	}

	revoked, err := s.patService.Revoke(ctx, in.ID, uid)
	if err != nil {
		return nil, huma.Error500InternalServerError("revoke PAT", err)
	}
	if !revoked {
		// No live row matched (id, user_id): the id doesn't exist,
		// was already revoked, or belongs to another admin. Report
		// 404 rather than a silent false success -- an operator
		// pointing at the wrong id must not walk away believing the
		// token is dead.
		return nil, huma.Error404NotFound("no live PAT with that id", nil)
	}

	if s.audit != nil {
		details := map[string]any{"id": in.ID}
		if err := s.audit.WriteActorAudit(ctx, p, audit.EventPATRevoked, "user_pat", fmt.Sprintf("%d", in.ID), details); err != nil {
			s.log.Warn("failed to write actor audit for PAT revoke", "error", err)
		}
	}

	out := &RevokePATOutput{}
	out.Body.OK = true
	return out, nil
}

// resolveCallerUserID returns the users.id of the calling admin.
// Three identity paths reach these handlers:
//
//   - PAT middleware: LocalUserView.UserID is the token owner's id
//     (RequirePAT attaches it) -- the fast path, no extra query.
//   - Local session: the session middleware attached the same view.
//   - Forward-auth only (no local rows): fall back to resolving the
//     Principal's ExternalUID via the attribution lookup.
//
// The forward-only fallback keeps the endpoints usable in deployments
// that haven't enabled local auth yet; the view paths cover the modes
// where a users row is guaranteed to exist.
func (s *Server) resolveCallerUserID(ctx context.Context) (int64, error) {
	if view, ok := auth.FromUser(ctx); ok && view.UserID != 0 {
		return view.UserID, nil
	}
	p, ok := auth.From(ctx)
	if !ok || p.Kind != auth.KindUser || !p.Authenticated {
		return 0, errors.New("no authenticated user principal")
	}
	return s.resolveUserIDFromPrincipal(ctx, p)
}

// resolveUserIDFromPrincipal looks up the users row for the calling
// Principal's ExternalUID. Only the forward-auth-only path needs this
// -- session and PAT requests carry a LocalUserView with UserID set.
func (s *Server) resolveUserIDFromPrincipal(ctx context.Context, p auth.Principal) (int64, error) {
	if p.ExternalUID == "" {
		return 0, errors.New("principal has no ExternalUID")
	}
	authProvider := p.AuthProvider
	if authProvider == "" {
		authProvider = "forward-link" // local session default
	}
	row, err := s.db.Reader.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
		AuthProvider: authProvider,
		ExternalUid:  p.ExternalUID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("no users row for %s/%s", authProvider, p.ExternalUID)
		}
		return 0, err
	}
	return row.ID, nil
}

// decodeScopesFromJSON unmarshals the scopes_json column. Tolerates
// the empty-array case (a freshly-minted PAT has empty scopes only
// if scopes was an empty slice, which the mint caller can guard
// against -- but be defensive anyway).
func decodeScopesFromJSON(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(s), &scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}
