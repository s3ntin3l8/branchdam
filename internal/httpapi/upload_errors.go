package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/s3ntin3l8/branchdam/internal/storage"
)

var (
	ErrInvalidFilename         = errors.New("invalid filename")
	ErrInvalidCameraModel      = errors.New("invalid camera model")
	ErrInvalidPath             = errors.New("invalid path")
	ErrPathEscapes             = errors.New("path escapes storage location")
	ErrFileTooLarge            = errors.New("upload exceeds maximum allowed file size (50 GB)")
	ErrStorageLocationNotFound = errors.New("storage location not found")
	ErrInvalidStorageLocation  = errors.New("invalid storage location")
	ErrStorageLocationInactive = errors.New("storage location is inactive")
	ErrStorageLocationReadOnly = errors.New("storage location is read-only")
	ErrNoWritableLocation      = errors.New("no writable storage location configured")
	ErrChecksumMismatch        = errors.New("checksum mismatch")
	ErrDatabaseUnavailable     = errors.New("database not available")
)

type ChecksumMismatchError struct {
	Expected string
	Got      string
}

func (e *ChecksumMismatchError) Error() string {
	return fmt.Sprintf("checksum mismatch: expected %s, got %s", e.Expected, e.Got)
}

func (e *ChecksumMismatchError) Is(target error) bool {
	return target == ErrChecksumMismatch
}

type StorageLocationReadOnlyError struct {
	Location string
	Tier     string
}

func (e *StorageLocationReadOnlyError) Error() string {
	return fmt.Sprintf("storage location %q is read-only (tier %s)", e.Location, e.Tier)
}

func (e *StorageLocationReadOnlyError) Is(target error) bool {
	return target == ErrStorageLocationReadOnly
}

// writeUploadError maps domain and upload errors to appropriate HTTP status codes and JSON messages.
func (s *Server) writeUploadError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	userMsg := "upload processing failed"

	var readOnlyTierErr *storage.ErrReadOnlyTier
	switch {
	case errors.Is(err, ErrInvalidFilename):
		status = http.StatusBadRequest
		userMsg = "invalid filename"
	case errors.Is(err, ErrInvalidCameraModel):
		status = http.StatusBadRequest
		userMsg = "invalid camera model"
	case errors.Is(err, ErrInvalidPath):
		status = http.StatusBadRequest
		userMsg = "invalid path"
	case errors.Is(err, ErrPathEscapes):
		status = http.StatusBadRequest
		userMsg = "path escapes storage location"
	case errors.Is(err, ErrChecksumMismatch):
		status = http.StatusBadRequest
		userMsg = "checksum mismatch"
	case errors.Is(err, ErrInvalidStorageLocation), errors.Is(err, ErrStorageLocationNotFound), errors.Is(err, ErrStorageLocationInactive):
		status = http.StatusBadRequest
		userMsg = "invalid storage location"
	case errors.Is(err, ErrFileTooLarge):
		status = http.StatusRequestEntityTooLarge
		userMsg = "upload exceeds maximum allowed file size (50 GB)"
	case errors.Is(err, ErrStorageLocationReadOnly), errors.As(err, &readOnlyTierErr):
		status = http.StatusForbidden
		userMsg = "storage location is read-only"
	case errors.Is(err, ErrNoWritableLocation):
		status = http.StatusServiceUnavailable
		userMsg = "no writable storage location configured"
	default:
		if s.log != nil {
			s.log.Error("upload internal processing failure", "err", err)
		}
	}

	s.writeJSONError(w, status, userMsg)
}
