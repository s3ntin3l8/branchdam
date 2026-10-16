package apierr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/apierr"
)

func TestNewIsAHumaStatusErrorWithCode(t *testing.T) {
	p := apierr.New(http.StatusConflict, apierr.CodeHashConflict, "node already has a different fullHash", errors.New("inner"), nil)
	var se huma.StatusError = p
	assert.Equal(t, http.StatusConflict, se.GetStatus())
	assert.Equal(t, "node already has a different fullHash", se.Error())
	assert.Equal(t, "application/problem+json", p.ContentType("application/json"))
	require.Len(t, p.Errors, 1, "nil errs are skipped, like huma.NewError")

	b, err := json.Marshal(p)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "HASH_CONFLICT", m["code"])
	assert.Equal(t, "Conflict", m["title"])
	assert.EqualValues(t, 409, m["status"])
}

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	apierr.Write(rec, apierr.New(http.StatusUnauthorized, apierr.CodeAuthInvalidKey, "invalid or missing X-API-Key"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"title":"Unauthorized","status":401,"detail":"invalid or missing X-API-Key","code":"AUTH_INVALID_KEY"}`, rec.Body.String())
}

func TestCodeForStatus(t *testing.T) {
	for status, want := range map[int]string{
		400: apierr.CodeValidationFailed, 422: apierr.CodeValidationFailed,
		401: apierr.CodeAuthInvalidKey, 403: apierr.CodeForbidden, 404: apierr.CodeNotFound,
		409: apierr.CodeConflict, 413: apierr.CodePayloadTooLarge, 500: apierr.CodeInternal,
		503: apierr.CodeUnavailable, 418: apierr.CodeError,
	} {
		assert.Equal(t, want, apierr.CodeForStatus(status), "status %d", status)
	}
}
