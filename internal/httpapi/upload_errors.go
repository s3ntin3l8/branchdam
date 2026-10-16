package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/s3ntin3l8/branchdam/internal/apierr"
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

// writeUploadError maps domain and upload errors to HTTP status codes and an
// apierr problem+json body with a stable code (plus the legacy "error"
// field). Shared by the agent upload and the browser upload; statuses and
// messages are unchanged.
func (s *Server) writeUploadError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := apierr.CodeInternal
	userMsg := "upload processing failed"

	var readOnlyTierErr *storage.ErrReadOnlyTier
	switch {
	case errors.Is(err, ErrInvalidFilename):
		status, code = http.StatusBadRequest, apierr.CodeValidationFailed
		userMsg = "invalid filename"
	case errors.Is(err, ErrInvalidCameraModel):
		status, code = http.StatusBadRequest, apierr.CodeValidationFailed
		userMsg = "invalid camera model"
	case errors.Is(err, ErrInvalidPath):
		status, code = http.StatusBadRequest, apierr.CodeValidationFailed
		userMsg = "invalid path"
	case errors.Is(err, ErrPathEscapes):
		status, code = http.StatusBadRequest, apierr.CodePathOutsideStorage
		userMsg = "path escapes storage location"
	case errors.Is(err, ErrChecksumMismatch):
		status, code = http.StatusBadRequest, apierr.CodeChecksumMismatch
		userMsg = "checksum mismatch"
	case errors.Is(err, ErrInvalidStorageLocation), errors.Is(err, ErrStorageLocationNotFound), errors.Is(err, ErrStorageLocationInactive):
		status, code = http.StatusBadRequest, apierr.CodeValidationFailed
		userMsg = "invalid storage location"
	case errors.Is(err, ErrFileTooLarge):
		status, code = http.StatusRequestEntityTooLarge, apierr.CodePayloadTooLarge
		userMsg = "upload exceeds maximum allowed file size (50 GB)"
	case errors.Is(err, ErrStorageLocationReadOnly), errors.As(err, &readOnlyTierErr):
		status, code = http.StatusForbidden, apierr.CodeReadOnlyTarget
		userMsg = "storage location is read-only"
	case errors.Is(err, ErrNoWritableLocation):
		status, code = http.StatusServiceUnavailable, apierr.CodeStorageNotConfigured
		userMsg = "no writable storage location configured"
	default:
		if s.log != nil {
			errMessage := "<nil>"
			if err != nil {
				errMessage = sanitizeForLog(err.Error())
			}
			s.log.Error("upload internal processing failure", "err", errMessage)
		}
	}

	writeUploadProblem(w, apierr.New(status, code, userMsg))
}
