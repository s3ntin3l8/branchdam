package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

type AgentUploadResponse struct {
	NodeUUID     string `json:"nodeUuid"`
	Status       string `json:"status"`
	BytesWritten int64  `json:"bytesWritten"`
	Blake3Hash   string `json:"blake3Hash"`
	RelativePath string `json:"relativePath,omitempty"`
}

func (s *Server) writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// resolveAgentUploadUserID resolves the uploaded_by_user_id attribution for
// an agent upload from agentID's device_pairings row. Returns 0 (NULL
// attribution) for every case other than "an active pairing with a set
// owner" -- no pairing row, a revoked pairing, or a pairing with no owner --
// logging a Warn for lookup failures.
//
// The RevokedAt.Valid case is normally unreachable through the HTTP auth
// path: GetDevicePairingKeyByHash (the query behind AgentConfig.LookupKey)
// already filters out keys belonging to a revoked pairing, so a revoked
// pairing's key fails authentication before this ever runs. It exists to
// cover the narrow TOCTOU window where a pairing is revoked between that
// auth check and this lookup, for an already-in-flight request.
func (s *Server) resolveAgentUploadUserID(ctx context.Context, agentID string) int64 {
	pairing, err := s.db.Reader.GetDevicePairingByAgentID(ctx, agentID)
	switch {
	case err != nil:
		s.log.Warn("agent upload: device pairing lookup failed, attribution NULL", "agentId", agentID, "err", err.Error())
		return 0
	case pairing.RevokedAt.Valid:
		s.log.Warn("agent upload: pairing revoked, attribution NULL", "agentId", agentID)
		return 0
	case !pairing.UserID.Valid:
		s.log.Warn("agent upload: pairing has no owner, attribution NULL", "agentId", agentID)
		return 0
	default:
		return pairing.UserID.Int64
	}
}

func (s *Server) handleAgentUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok || (p.Kind != auth.KindMachine && !p.Authenticated) {
		s.writeJSONError(w, http.StatusForbidden, "authentication required")
		return
	}

	// Resolve the paired user's ID for uploaded_by_user_id attribution.
	// Agent principals are KindMachine with no ExternalUID, so the
	// user_id comes from the device_pairings row keyed on agent_id.
	var agentUserID int64
	if p.Kind == auth.KindMachine {
		agentUserID = s.resolveAgentUploadUserID(r.Context(), p.Name)
	}

	filename := r.Header.Get("X-Filename")
	cameraModel := r.Header.Get("X-Camera-Model")
	expectedBlake3 := r.Header.Get("X-Blake3-Hash")
	sourcePathHash := r.Header.Get("X-Source-Path-Hash")
	capturedAtHeader := r.Header.Get("X-Capture-Timestamp")
	var capturedAtUnix int64
	if capturedAtHeader != "" {
		capturedAtUnix, _ = strconv.ParseInt(capturedAtHeader, 10, 64)
	}

	result, err := s.processUploadedStream(r.Context(), UploadParams{
		Filename:            filename,
		Body:                r.Body,
		ApplyNamingTemplate: true,
		CameraModel:         cameraModel,
		CapturedAtUnix:      capturedAtUnix,
		ExpectedBlake3:      expectedBlake3,
		SourcePathHash:      sourcePathHash,
		UserID:              agentUserID,
	})
	if err != nil {
		s.writeUploadError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if result.IsDedup {
		w.Header().Set("X-Dedup", "true")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(AgentUploadResponse{
			NodeUUID:     result.NodeUUID,
			Status:       "DEDUPLICATED",
			BytesWritten: result.SizeBytes,
			Blake3Hash:   result.Blake3Hash,
			RelativePath: result.RelativePath,
		})
		return
	}

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(AgentUploadResponse{
		NodeUUID:     result.NodeUUID,
		Status:       "UPLOADED",
		BytesWritten: result.SizeBytes,
		Blake3Hash:   result.Blake3Hash,
		RelativePath: result.RelativePath,
	})
}
