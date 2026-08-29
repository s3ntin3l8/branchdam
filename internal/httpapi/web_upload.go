package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

const maxFormMemory = 32 << 20 // 32 MB in-memory buffer for small parts; large parts stream to disk

// WebUploadResponse is the JSON structure returned upon successful web upload.
type WebUploadResponse struct {
	Asset        assetDTO `json:"asset"`
	NodeUUID     string   `json:"nodeUuid"`
	Status       string   `json:"status"`
	BytesWritten int64    `json:"bytesWritten"`
	Blake3Hash   string   `json:"blake3Hash"`
	RelativePath string   `json:"relativePath"`
}

// handleWebUpload processes browser multipart/form-data uploads.
func (s *Server) handleWebUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok || !p.Authenticated {
		s.writeJSONError(w, http.StatusForbidden, "authentication required")
		return
	}

	// Verify admin permissions if allowedGroups are configured
	var allowedGroups []string
	if cfg := s.cfg(); cfg != nil {
		allowedGroups = cfg.Authz.Groups
	}
	if !auth.IsAdmin(p, allowedGroups) {
		s.writeJSONError(w, http.StatusForbidden, "admin authorization required")
		return
	}

	if err := r.ParseMultipartForm(maxFormMemory); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("failed to parse multipart form: %v", err))
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer func() { _ = file.Close() }()

	var storageLocationID int64
	if locIDStr := r.FormValue("storageLocationId"); locIDStr != "" {
		storageLocationID, _ = strconv.ParseInt(locIDStr, 10, 64)
	}

	relativePath := r.FormValue("relativePath")
	cameraModel := r.FormValue("overrideCameraModel")

	var capturedAtUnix int64
	if capStr := r.FormValue("overrideCapturedAt"); capStr != "" {
		capturedAtUnix, _ = strconv.ParseInt(capStr, 10, 64)
	}

	applyNamingTemplate := true
	if applyStr := r.FormValue("applyNamingTemplate"); applyStr != "" {
		applyNamingTemplate = strings.ToLower(applyStr) == "true" || applyStr == "1"
	}

	filename := header.Filename
	if filename == "" {
		filename = "uploaded_media.bin"
	}

	result, err := s.processUploadedStream(r.Context(), UploadParams{
		Filename:            filename,
		Body:                file,
		StorageLocationID:   storageLocationID,
		RelativePath:        relativePath,
		ApplyNamingTemplate: applyNamingTemplate,
		CameraModel:         cameraModel,
		CapturedAtUnix:      capturedAtUnix,
	})
	if err != nil {
		status := http.StatusInternalServerError
		errMsg := err.Error()
		if strings.Contains(errMsg, "invalid filename") ||
			strings.Contains(errMsg, "invalid camera model") ||
			strings.Contains(errMsg, "invalid rendered path") ||
			strings.Contains(errMsg, "path escapes storage location") ||
			strings.Contains(errMsg, "invalid relative path") {
			status = http.StatusBadRequest
		} else if strings.Contains(errMsg, "read-only") {
			status = http.StatusConflict
		} else if strings.Contains(errMsg, "no writable storage location configured") || strings.Contains(errMsg, "not configured") || strings.Contains(errMsg, "not found") {
			status = http.StatusServiceUnavailable
		}
		s.writeJSONError(w, status, errMsg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(WebUploadResponse{
		Asset:        result.Asset,
		NodeUUID:     result.NodeUUID,
		Status:       "UPLOADED",
		BytesWritten: result.SizeBytes,
		Blake3Hash:   result.Blake3Hash,
		RelativePath: result.RelativePath,
	})
}
