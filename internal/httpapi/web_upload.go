package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

// WebUploadResponse is the JSON structure returned upon successful web upload.
type WebUploadResponse struct {
	Asset        assetDTO `json:"asset"`
	NodeUUID     string   `json:"nodeUuid"`
	Status       string   `json:"status"`
	BytesWritten int64    `json:"bytesWritten"`
	Blake3Hash   string   `json:"blake3Hash"`
	RelativePath string   `json:"relativePath"`
}

// handleWebUpload processes browser multipart/form-data uploads via direct streaming.
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

	mr, err := r.MultipartReader()
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("failed to parse multipart form: %v", err))
		return
	}

	var (
		storageLocationID   int64
		relativePath        = r.URL.Query().Get("relativePath")
		cameraModel         = r.URL.Query().Get("overrideCameraModel")
		capturedAtUnix      int64
		applyNamingTemplate = true
		expectedBlake3      = r.Header.Get("X-Blake3-Hash")
		sourcePathHash      = r.Header.Get("X-Source-Path-Hash")
		result              *UploadResult
		fileProcessed       bool
	)

	if locIDStr := r.URL.Query().Get("storageLocationId"); locIDStr != "" {
		if parsed, parseErr := strconv.ParseInt(locIDStr, 10, 64); parseErr == nil {
			storageLocationID = parsed
		} else {
			s.writeUploadError(w, ErrInvalidStorageLocation)
			return
		}
	}
	if capStr := r.URL.Query().Get("overrideCapturedAt"); capStr != "" {
		capturedAtUnix, _ = strconv.ParseInt(capStr, 10, 64)
	}
	if applyStr := r.URL.Query().Get("applyNamingTemplate"); applyStr != "" {
		applyNamingTemplate = strings.ToLower(applyStr) == "true" || applyStr == "1"
	}
	if expectedBlake3 == "" {
		expectedBlake3 = r.URL.Query().Get("expectedBlake3")
	}
	if sourcePathHash == "" {
		sourcePathHash = r.URL.Query().Get("sourcePathHash")
	}

	for {
		part, partErr := mr.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("failed to read multipart part: %v", partErr))
			return
		}

		formName := part.FormName()
		if formName == "file" || (formName == "" && part.FileName() != "") {
			filename := part.FileName()
			if filename == "" {
				filename = "uploaded_media.bin"
			}

			fileProcessed = true
			res, procErr := s.processUploadedStream(r.Context(), UploadParams{
				Filename:            filename,
				Body:                part,
				StorageLocationID:   storageLocationID,
				RelativePath:        relativePath,
				ApplyNamingTemplate: applyNamingTemplate,
				CameraModel:         cameraModel,
				CapturedAtUnix:      capturedAtUnix,
				ExpectedBlake3:      expectedBlake3,
				SourcePathHash:      sourcePathHash,
			})
			_ = part.Close()
			if procErr != nil {
				s.writeUploadError(w, procErr)
				return
			}
			result = res
			break
		}

		// Non-file form fields: read their value (limit to 64KB per field)
		buf, readErr := io.ReadAll(io.LimitReader(part, 64*1024))
		_ = part.Close()
		if readErr != nil {
			s.writeJSONError(w, http.StatusBadRequest, "failed to read form field")
			return
		}
		val := string(buf)
		switch formName {
		case "storageLocationId":
			if val != "" {
				if locID, parseErr := strconv.ParseInt(val, 10, 64); parseErr == nil {
					storageLocationID = locID
				} else {
					s.writeUploadError(w, ErrInvalidStorageLocation)
					return
				}
			}
		case "relativePath":
			relativePath = val
		case "overrideCameraModel":
			cameraModel = val
		case "overrideCapturedAt":
			if val != "" {
				if capUnix, parseErr := strconv.ParseInt(val, 10, 64); parseErr == nil {
					capturedAtUnix = capUnix
				}
			}
		case "applyNamingTemplate":
			if val != "" {
				applyNamingTemplate = strings.ToLower(val) == "true" || val == "1"
			}
		case "expectedBlake3":
			if expectedBlake3 == "" {
				expectedBlake3 = val
			}
		case "sourcePathHash":
			if sourcePathHash == "" {
				sourcePathHash = val
			}
		}
	}

	if !fileProcessed || result == nil {
		s.writeJSONError(w, http.StatusBadRequest, "file field is required")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if result.IsDedup {
		w.Header().Set("X-Dedup", "true")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(WebUploadResponse{
			Asset:        result.Asset,
			NodeUUID:     result.NodeUUID,
			Status:       "DEDUPLICATED",
			BytesWritten: result.SizeBytes,
			Blake3Hash:   result.Blake3Hash,
			RelativePath: result.RelativePath,
		})
		return
	}

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
