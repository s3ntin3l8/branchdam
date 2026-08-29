package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
		errMsg := err.Error()
		if strings.Contains(errMsg, "invalid filename") ||
			strings.Contains(errMsg, "invalid camera model") ||
			strings.Contains(errMsg, "invalid rendered path") ||
			strings.Contains(errMsg, "path escapes storage location") ||
			strings.Contains(errMsg, "checksum mismatch") {
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
	_ = json.NewEncoder(w).Encode(AgentUploadResponse{
		NodeUUID:     result.NodeUUID,
		Status:       "UPLOADED",
		BytesWritten: result.SizeBytes,
		Blake3Hash:   result.Blake3Hash,
		RelativePath: result.RelativePath,
	})
}

func isStandaloneDisplayable(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".heic", ".png", ".webp", ".mp4", ".mov":
		return true
	default:
		return false
	}
}

func linkOrCopyFile(src, dst string) error {
	linkErr := os.Link(src, dst)
	if linkErr == nil || errors.Is(linkErr, os.ErrExist) {
		return nil
	}

	// Fallback to safe non-truncating copy on cross-device link failure
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("link error: %w, open src error: %v", linkErr, err)
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("link error: %w, create dst error: %v", linkErr, err)
	}
	defer func() { _ = dstFile.Close() }()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("link error: %w, copy error: %v", linkErr, err)
	}
	return nil
}
