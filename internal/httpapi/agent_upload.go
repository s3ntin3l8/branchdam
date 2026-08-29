package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

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

func (s *Server) handleAgentUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok || (p.Kind != auth.KindMachine && !p.Authenticated) {
		s.writeJSONError(w, http.StatusForbidden, "authentication required")
		return
	}

	filename := r.Header.Get("X-Filename")
	cameraModel := r.Header.Get("X-Camera-Model")
	expectedBlake3 := r.Header.Get("X-Blake3-Hash")
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
	})
	if err != nil {
		status := http.StatusInternalServerError
		userMsg := "upload processing failed"
		errMsg := err.Error()

		if strings.Contains(errMsg, "invalid filename") {
			status = http.StatusBadRequest
			userMsg = "invalid filename"
		} else if strings.Contains(errMsg, "invalid camera model") {
			status = http.StatusBadRequest
			userMsg = "invalid camera model"
		} else if strings.Contains(errMsg, "invalid rendered path") || strings.Contains(errMsg, "invalid relative path") {
			status = http.StatusBadRequest
			userMsg = "invalid path"
		} else if strings.Contains(errMsg, "path escapes storage location") {
			status = http.StatusBadRequest
			userMsg = "path escapes storage location"
		} else if strings.Contains(errMsg, "checksum mismatch") {
			status = http.StatusBadRequest
			userMsg = "checksum mismatch"
		} else if strings.Contains(errMsg, "upload exceeds maximum allowed file size") {
			status = http.StatusRequestEntityTooLarge
			userMsg = "upload exceeds maximum allowed file size (50 GB)"
		} else if strings.Contains(errMsg, "read-only") {
			status = http.StatusConflict
			userMsg = "storage location is read-only"
		} else if strings.Contains(errMsg, "no writable storage location configured") || strings.Contains(errMsg, "not configured") || strings.Contains(errMsg, "not found") {
			status = http.StatusServiceUnavailable
			userMsg = "no writable storage location configured"
		}

		if s.log != nil && status == http.StatusInternalServerError {
			s.log.Error("agent upload internal processing failure")
		}
		s.writeJSONError(w, status, userMsg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(AgentUploadResponse{
		NodeUUID:     result.NodeUUID,
		Status:       "UPLOADED",
		BytesWritten: result.SizeBytes,
		Blake3Hash:   result.Blake3Hash,
		RelativePath: result.RelativePath,
	})
}
