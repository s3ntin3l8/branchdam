// Package apierr is the single error envelope of the agent API
// (/api/v1/agent/*): an RFC 9457 problem+json body -- Huma's ErrorModel --
// plus a stable, machine-readable Code. Clients classify failures by Code
// (and HTTP status), never by parsing Detail, which is free text for humans
// and may change wording at any time.
//
// It is used by the agent auth middleware (internal/auth), the Huma agent
// handlers and the raw upload handler (internal/httpapi). The code table is
// documented in docs/agent-api.md; codes are append-only -- a published code
// never changes meaning or HTTP status.
package apierr

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Stable error codes. See docs/agent-api.md's error table for the HTTP
// status each one travels with and whether a client should retry.
const (
	// CodeAuthNotConfigured: the server has no device-key lookup wired
	// (Companion Pairing unavailable), so no agent request can
	// authenticate. 503.
	CodeAuthNotConfigured = "AUTH_NOT_CONFIGURED"
	// CodeAuthInvalidKey: X-API-Key missing, unknown or revoked. 401.
	CodeAuthInvalidKey = "AUTH_INVALID_KEY"
	// CodeAuthInvalidSignature: signed requests are enabled and the
	// X-Timestamp/X-Nonce/X-Signature set is missing, malformed, outside
	// the replay window, replayed, or does not verify. 401.
	CodeAuthInvalidSignature = "AUTH_INVALID_SIGNATURE"
	// CodeAuthForbidden: the request is authenticated but not as a paired
	// agent device. 403.
	CodeAuthForbidden = "AUTH_FORBIDDEN"
	// CodeAgentIDMismatch: the body's agentId is not the authenticated
	// device's agent id. 403.
	CodeAgentIDMismatch = "AGENT_ID_MISMATCH"
	// CodeNotNodeOwner: the node exists but was not created by the
	// calling device. 403.
	CodeNotNodeOwner = "NOT_NODE_OWNER"
	// CodeValidationFailed: the request is malformed or a field is
	// invalid (bad UUID, bad hash, missing field, schema violation). 400
	// or 422.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodePathOutsideStorage: a path does not resolve to any configured
	// storage location, or escapes one. 400.
	CodePathOutsideStorage = "PATH_OUTSIDE_STORAGE"
	// CodeReadOnlyTarget: the target storage location is read-only. 400
	// on rebase, 403 on upload.
	CodeReadOnlyTarget = "READ_ONLY_TARGET"
	// CodeArchiveFileNotPresent: a Tier-3 rebase target has no file yet;
	// retry once the copy into the archive has finished. 400.
	CodeArchiveFileNotPresent = "ARCHIVE_FILE_NOT_PRESENT"
	// CodeChecksumMismatch: the uploaded bytes do not match
	// X-Blake3-Hash. 400.
	CodeChecksumMismatch = "CHECKSUM_MISMATCH"
	// CodeNodeNotFound: no live node with that UUID (including an
	// ARCHIVED, superseded version). 404.
	CodeNodeNotFound = "NODE_NOT_FOUND"
	// CodeHashConflict: the node already has a different full hash. 409.
	CodeHashConflict = "HASH_CONFLICT"
	// CodeEventConflict: eventUuid was already used with a different
	// eventType. 409.
	CodeEventConflict = "EVENT_UUID_CONFLICT"
	// CodeSnapshotConflict: a Resolve snapshot conflicts with existing
	// graph state; nothing was changed. 409.
	CodeSnapshotConflict = "SNAPSHOT_CONFLICT"
	// CodePayloadTooLarge: the request body or snapshot exceeds a server
	// limit. 413.
	CodePayloadTooLarge = "PAYLOAD_TOO_LARGE"
	// CodeStorageNotConfigured: the server lacks the storage
	// configuration this request needs (no storage guard, no virtual
	// location, no writable location). 409, 500 or 503.
	CodeStorageNotConfigured = "STORAGE_NOT_CONFIGURED"
	// CodeInternal: an unexpected server-side failure. 500.
	CodeInternal = "INTERNAL_ERROR"
	// CodeUnavailable: a 503 with no more specific code.
	CodeUnavailable = "SERVICE_UNAVAILABLE"
	// CodeForbidden, CodeNotFound, CodeConflict and CodeError are the
	// fallbacks CodeForStatus assigns to an error raised without an
	// explicit code (e.g. by Huma itself).
	CodeForbidden = "FORBIDDEN"
	CodeNotFound  = "NOT_FOUND"
	CodeConflict  = "CONFLICT"
	CodeError     = "ERROR"
)

// Problem is a problem+json error body with a stable Code. It satisfies
// huma.StatusError (and huma.ContentTypeFilter, so Huma serves it as
// application/problem+json) through the embedded ErrorModel.
type Problem struct {
	huma.ErrorModel
	Code string `json:"code" doc:"Stable machine-readable error code"`
}

// New builds a Problem. errs become ErrorModel.Errors entries exactly the
// way huma.NewError builds them, so switching a handler from huma.ErrorXXX
// to New changes nothing but the added code.
func New(status int, code, detail string, errs ...error) *Problem {
	p := &Problem{
		ErrorModel: huma.ErrorModel{
			Status: status,
			Title:  http.StatusText(status),
			Detail: detail,
		},
		Code: code,
	}
	for _, err := range errs {
		if err != nil {
			p.Add(err)
		}
	}
	return p
}

// FromModel wraps an existing ErrorModel (e.g. one Huma raised for a
// request-validation failure) with code.
func FromModel(m *huma.ErrorModel, code string) *Problem {
	return &Problem{ErrorModel: *m, Code: code}
}

// CodeForStatus is the code given to an agent-route error that was raised
// without one -- Huma's own request validation (400/422) being the main
// case.
func CodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return CodeValidationFailed
	case http.StatusUnauthorized:
		return CodeAuthInvalidKey
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusRequestEntityTooLarge:
		return CodePayloadTooLarge
	case http.StatusInternalServerError:
		return CodeInternal
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	default:
		return CodeError
	}
}

// Write writes p as an application/problem+json response, for handlers and
// middleware outside Huma.
func Write(w http.ResponseWriter, p *Problem) {
	WriteBody(w, p.Status, p)
}

// WriteBody writes body (a Problem, or a struct embedding one) as an
// application/problem+json response with the given status.
func WriteBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
