package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/pairing"
)

// Companion pairing API: /api/v1/companion/pairings/*.
//
// Every route is admin-only via the existing auth.RequireAdmin middleware
// (Huma passes the principal through context, and RequireAdmin refuses
// anything but a real authenticated browser principal in an admin group).
// Agent (KindMachine) principals get 403 here, same as on every other
// admin route -- a device can never enumerate or revoke other devices.

// --- request / response DTOs ---

type CreatePairingInput struct {
	Body struct {
		FriendlyLabel string `json:"friendlyLabel" minLength:"1" maxLength:"120"`
	}
}

type CreatePairingOutput struct {
	Body struct {
		PairingID     int64  `json:"pairingId"`
		AgentID       string `json:"agentId"`
		APIKey        string `json:"apiKey"`
		KeyPreview    string `json:"keyPreview"`
		QRSVG         string `json:"qrSvg"`
		CreatedAtUnix int64  `json:"createdAtUnix"`
	}
}

type ListPairingsOutput struct {
	Body struct {
		Pairings []pairingListItemDTO `json:"pairings"`
		Total    int64                `json:"total"`
	}
}

type pairingListItemDTO struct {
	ID             int64  `json:"id"`
	AgentID        string `json:"agentId"`
	FriendlyLabel  string `json:"friendlyLabel"`
	CreatedAtUnix  int64  `json:"createdAtUnix"`
	CreatedBy      string `json:"createdBy"`
	RevokedAtUnix  *int64 `json:"revokedAtUnix,omitempty"`
	ActiveKeyCount int64  `json:"activeKeyCount"`
}

type GetPairingInput struct {
	ID int64 `path:"id"`
}

type GetPairingOutput struct {
	Body struct {
		ID            int64                 `json:"id"`
		AgentID       string                `json:"agentId"`
		FriendlyLabel string                `json:"friendlyLabel"`
		CreatedAtUnix int64                 `json:"createdAtUnix"`
		CreatedBy     string                `json:"createdBy"`
		RevokedAtUnix *int64                `json:"revokedAtUnix,omitempty"`
		Keys          []pairingKeyDTO       `json:"keys"`
		AuditTail     []pairingAuditItemDTO `json:"auditTail"`
	}
}

type pairingKeyDTO struct {
	ID            int64  `json:"id"`
	KeyPreview    string `json:"keyPreview"`
	CreatedAtUnix int64  `json:"createdAtUnix"`
	ExpiresAtUnix *int64 `json:"expiresAtUnix,omitempty"`
	RevokedAtUnix *int64 `json:"revokedAtUnix,omitempty"`
}

type pairingAuditItemDTO struct {
	ID            int64  `json:"id"`
	Actor         string `json:"actor"`
	Event         string `json:"event"`
	DetailsJSON   string `json:"detailsJson"`
	CreatedAtUnix int64  `json:"createdAtUnix"`
}

type RotatePairingInput struct {
	ID   int64 `path:"id"`
	Body struct {
		GraceMinutes int `json:"graceMinutes,omitempty" minimum:"1" maximum:"10080"`
	}
}

type RotatePairingOutput struct {
	Body struct {
		KeyID                int64  `json:"keyId"`
		APIKey               string `json:"apiKey"`
		KeyPreview           string `json:"keyPreview"`
		QRSVG                string `json:"qrSvg"`
		PreviousKeyExpiresAt int64  `json:"previousKeyExpiresAtUnix"`
	}
}

type RevokePairingInput struct {
	ID int64 `path:"id"`
}

type RevokePairingOutput struct {
	Body struct {
		RevokedAtUnix int64 `json:"revokedAtUnix"`
	}
}

type PairingAuditInput struct {
	ID     int64 `path:"id"`
	Limit  int64 `query:"limit" minimum:"1" maximum:"500" default:"100"`
	Offset int64 `query:"offset" minimum:"0" default:"0"`
}

type PairingAuditOutput struct {
	Body struct {
		Events []pairingAuditItemDTO `json:"events"`
		Total  int64                 `json:"total"`
	}
}

// --- registration ---

func (s *Server) registerCompanionPairings(api huma.API) {
	huma.Post(api, "/api/v1/companion/pairings", s.handleCreatePairing)
	huma.Get(api, "/api/v1/companion/pairings", s.handleListPairings)
	huma.Get(api, "/api/v1/companion/pairings/{id}", s.handleGetPairing)
	huma.Post(api, "/api/v1/companion/pairings/{id}/rotate", s.handleRotatePairing)
	huma.Post(api, "/api/v1/companion/pairings/{id}/revoke", s.handleRevokePairing)
	huma.Get(api, "/api/v1/companion/pairings/{id}/audit", s.handlePairingAudit)
}

func (s *Server) pairingSvc() (*pairing.Service, error) {
	if s.pairingService == nil {
		return nil, huma.Error503ServiceUnavailable("companion pairing is not configured")
	}
	return s.pairingService, nil
}

func actorFromCtx(ctx context.Context) string {
	p, ok := auth.From(ctx)
	if !ok {
		return "system"
	}
	if p.Kind == auth.KindMachine {
		return p.Name
	}
	if p.Name == "" {
		return "user:anonymous"
	}
	return "user:" + p.Name
}

// --- handlers ---

// handleCreatePairing mints a new pairing + initial key + QR SVG. The
// plaintext key is returned exactly once.
func (s *Server) handleCreatePairing(ctx context.Context, in *CreatePairingInput) (*CreatePairingOutput, error) {
	svc, err := s.pairingSvc()
	if err != nil {
		return nil, err
	}
	pairing, key, err := svc.CreatePairing(ctx, in.Body.FriendlyLabel, actorFromCtx(ctx), s.qrPayloadFor(ctx))
	if err != nil {
		return nil, huma.Error500InternalServerError("create pairing", err)
	}
	out := &CreatePairingOutput{}
	out.Body.PairingID = pairing.ID
	out.Body.AgentID = pairing.AgentID
	out.Body.APIKey = key.Plaintext
	out.Body.KeyPreview = key.Preview
	out.Body.QRSVG = string(key.QRSVG)
	out.Body.CreatedAtUnix = pairing.CreatedAt
	return out, nil
}

func (s *Server) handleListPairings(ctx context.Context, _ *struct{}) (*ListPairingsOutput, error) {
	svc, err := s.pairingSvc()
	if err != nil {
		return nil, err
	}
	// Soft cap for the SPA's unpaginated list. Homelab deployments rarely
	// exceed a dozen devices; this avoids unbounded reads without needing
	// a full pagination UX for an admin-only diagnostic page.
	const limit int64 = 200
	rows, err := svc.ListPairings(ctx, limit, 0)
	if err != nil {
		return nil, huma.Error500InternalServerError("list pairings", err)
	}
	total, err := svc.CountPairings(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("count pairings", err)
	}
	out := &ListPairingsOutput{}
	out.Body.Pairings = make([]pairingListItemDTO, len(rows))
	for i, r := range rows {
		dto := pairingListItemDTO{
			ID:             r.ID,
			AgentID:        r.AgentID,
			FriendlyLabel:  r.FriendlyLabel,
			CreatedAtUnix:  r.CreatedAt,
			CreatedBy:      r.CreatedBy,
			ActiveKeyCount: r.ActiveKeyCount,
		}
		if r.RevokedAt.Valid {
			v := r.RevokedAt.Int64
			dto.RevokedAtUnix = &v
		}
		out.Body.Pairings[i] = dto
	}
	out.Body.Total = total
	return out, nil
}

func (s *Server) handleGetPairing(ctx context.Context, in *GetPairingInput) (*GetPairingOutput, error) {
	svc, err := s.pairingSvc()
	if err != nil {
		return nil, err
	}
	p, err := svc.GetPairing(ctx, in.ID)
	if err != nil {
		if errors.Is(err, pairing.ErrPairingNotFound) {
			return nil, huma.Error404NotFound("pairing not found")
		}
		return nil, huma.Error500InternalServerError("get pairing", err)
	}
	keys, err := svc.ListKeys(ctx, p.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list keys", err)
	}
	audit, err := svc.ListAudit(ctx, p.ID, 5, 0)
	if err != nil {
		return nil, huma.Error500InternalServerError("list audit", err)
	}
	out := &GetPairingOutput{}
	out.Body.ID = p.ID
	out.Body.AgentID = p.AgentID
	out.Body.FriendlyLabel = p.FriendlyLabel
	out.Body.CreatedAtUnix = p.CreatedAt
	out.Body.CreatedBy = p.CreatedBy
	if p.RevokedAt.Valid {
		v := p.RevokedAt.Int64
		out.Body.RevokedAtUnix = &v
	}
	out.Body.Keys = make([]pairingKeyDTO, len(keys))
	for i, k := range keys {
		dto := pairingKeyDTO{
			ID:            k.ID,
			KeyPreview:    k.KeyPreview,
			CreatedAtUnix: k.CreatedAt,
		}
		if k.ExpiresAt.Valid {
			v := k.ExpiresAt.Int64
			dto.ExpiresAtUnix = &v
		}
		if k.RevokedAt.Valid {
			v := k.RevokedAt.Int64
			dto.RevokedAtUnix = &v
		}
		out.Body.Keys[i] = dto
	}
	out.Body.AuditTail = make([]pairingAuditItemDTO, len(audit))
	for i, a := range audit {
		out.Body.AuditTail[i] = pairingAuditItemDTO{
			ID:            a.ID,
			Actor:         a.Actor,
			Event:         a.Event,
			DetailsJSON:   a.Details,
			CreatedAtUnix: a.CreatedAt,
		}
	}
	return out, nil
}

func (s *Server) handleRotatePairing(ctx context.Context, in *RotatePairingInput) (*RotatePairingOutput, error) {
	svc, err := s.pairingSvc()
	if err != nil {
		return nil, err
	}
	grace := in.Body.GraceMinutes
	if grace <= 0 {
		grace = 24 * 60
	}
	key, expiresAt, err := svc.RotateKey(ctx, in.ID, actorFromCtx(ctx), grace, s.qrPayloadFor(ctx))
	if err != nil {
		if errors.Is(err, pairing.ErrPairingNotFound) {
			return nil, huma.Error404NotFound("pairing not found")
		}
		return nil, huma.Error500InternalServerError("rotate pairing", err)
	}
	out := &RotatePairingOutput{}
	out.Body.KeyID = key.ID
	out.Body.APIKey = key.Plaintext
	out.Body.KeyPreview = key.Preview
	out.Body.QRSVG = string(key.QRSVG)
	out.Body.PreviousKeyExpiresAt = expiresAt
	return out, nil
}

func (s *Server) handleRevokePairing(ctx context.Context, in *RevokePairingInput) (*RevokePairingOutput, error) {
	svc, err := s.pairingSvc()
	if err != nil {
		return nil, err
	}
	revokedAt, err := svc.RevokePairing(ctx, in.ID, actorFromCtx(ctx))
	if err != nil {
		if errors.Is(err, pairing.ErrPairingNotFound) {
			return nil, huma.Error404NotFound("pairing not found")
		}
		return nil, huma.Error500InternalServerError("revoke pairing", err)
	}
	out := &RevokePairingOutput{}
	out.Body.RevokedAtUnix = revokedAt
	return out, nil
}

func (s *Server) handlePairingAudit(ctx context.Context, in *PairingAuditInput) (*PairingAuditOutput, error) {
	svc, err := s.pairingSvc()
	if err != nil {
		return nil, err
	}
	events, err := svc.ListAudit(ctx, in.ID, in.Limit, in.Offset)
	if err != nil {
		return nil, huma.Error500InternalServerError("list audit", err)
	}
	total, err := svc.CountPairingAudit(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("count audit", err)
	}
	out := &PairingAuditOutput{}
	out.Body.Events = make([]pairingAuditItemDTO, len(events))
	for i, e := range events {
		out.Body.Events[i] = pairingAuditItemDTO{
			ID:            e.ID,
			Actor:         e.Actor,
			Event:         e.Event,
			DetailsJSON:   e.Details,
			CreatedAtUnix: e.CreatedAt,
		}
	}
	out.Body.Total = total
	return out, nil
}

// --- non-Huma endpoints (mux-direct) ---

// handlePairingQRSVG serves the cached SVG for the current active key
// of a pairing. Registered directly on the mux because Huma's response
// model expects JSON; emitting image/svg+xml via Huma requires more
// indirection than this single endpoint warrants.
func (s *Server) handlePairingQRSVG(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid pairing id", http.StatusBadRequest)
		return
	}
	svc, serr := s.pairingSvc()
	if serr != nil {
		http.Error(w, serr.Error(), http.StatusServiceUnavailable)
		return
	}
	svg, err := svc.ActiveQRSVG(r.Context(), id)
	if err != nil {
		if errors.Is(err, pairing.ErrPairingNotFound) {
			http.Error(w, "pairing not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, pairing.ErrNoActiveKey) {
			http.Error(w, "pairing has no active key (revoked or expired)", http.StatusGone)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "private, no-store, max-age=0")
	_, _ = w.Write(svg)
}

// registerPairingDirectRoutes attaches the non-Huma endpoints to the mux.
// Called from Handler() alongside the Huma api registration.
func (s *Server) registerPairingDirectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/companion/pairings/{id}/qr.svg", s.handlePairingQRSVG)
}

// qrPayloadFor returns a closure the pairing service uses to build the
// QR payload for a given (agent_id, plaintext key) pair. The service
// stays unaware of HTTP concerns (no net/http import in
// internal/pairing); this adapter bridges them. The returned closure
// reads scheme/host from values injected into ctx by the
// pairingForwarded middleware (X-Forwarded-Proto / X-Forwarded-Host),
// falling back to plain http + the listen address.
func (s *Server) qrPayloadFor(ctx context.Context) func(agentID, apiKey string) []byte {
	scheme := "http"
	if proto, ok := ctx.Value(forwardedProtoKey{}).(string); ok && proto != "" {
		scheme = proto
	}
	host := ""
	if h, ok := ctx.Value(forwardedHostKey{}).(string); ok && h != "" {
		host = h
	}
	if host == "" && s.cfg() != nil && s.cfg().ListenAddr != "" {
		host = "localhost" + s.cfg().ListenAddr
	}
	if host == "" {
		host = "localhost:8080"
	}
	return func(agentID, apiKey string) []byte {
		v := url.Values{}
		v.Set("server", scheme+"://"+host)
		v.Set("key", apiKey)
		v.Set("agent", agentID)
		return []byte("branchdam://?" + v.Encode())
	}
}

// pairingForwardedMiddleware injects X-Forwarded-Proto and
// X-Forwarded-Host into the request context so the Huma handlers
// (which only see context.Context, not *http.Request) can build
// QR payloads with the externally-reachable URL. The mobile app's
// parser expects branchdam://?server=…&key=…&agent=… where the
// server is reachable from the phone (Traefik's ForwardAuth normally
// sets these headers in front of /api/v1/*).
func pairingForwardedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			ctx = context.WithValue(ctx, forwardedProtoKey{}, proto)
		} else if r.TLS != nil {
			ctx = context.WithValue(ctx, forwardedProtoKey{}, "https")
		}
		if host := r.Header.Get("X-Forwarded-Host"); host != "" {
			ctx = context.WithValue(ctx, forwardedHostKey{}, host)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type forwardedProtoKey struct{}
type forwardedHostKey struct{}
