package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func csrfStatus(t *testing.T, method, path string, hdr map[string]string) int {
	t.Helper()
	s := clientIPServer(nil)
	h := s.crossSiteGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(method, "http://dam.example.com"+path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr.Code
}

func TestCrossSiteGuard(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		hdr    map[string]string
		want   int
	}{
		{"GET is never blocked", http.MethodGet, "/api/v1/assets", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusNoContent},
		{"same-origin POST ok", http.MethodPost, "/api/v1/restart", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNoContent},
		{"user-initiated POST ok", http.MethodPost, "/api/v1/restart", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusNoContent},
		{"same-site (sibling subdomain) POST refused", http.MethodPost, "/api/v1/restart", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-site POST refused", http.MethodPost, "/api/v1/assets/1/trash", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"matching Origin ok (no Sec-Fetch-Site)", http.MethodPost, "/api/v1/login", map[string]string{"Origin": "https://dam.example.com"}, http.StatusNoContent},
		{"foreign Origin refused", http.MethodPost, "/api/v1/login", map[string]string{"Origin": "https://evil.example.org"}, http.StatusForbidden},
		{"sibling-subdomain Origin refused", http.MethodDelete, "/api/v1/x", map[string]string{"Origin": "https://blog.example.com"}, http.StatusForbidden},
		{"no browser headers (curl/PAT/companion) ok", http.MethodPost, "/api/v1/restart", nil, http.StatusNoContent},
		{"agent routes exempt", http.MethodPost, "/api/v1/agent/events", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, csrfStatus(t, tt.method, tt.path, tt.hdr))
		})
	}
}

func TestDecodeSmallJSON_RejectsOversizeBody(t *testing.T) {
	big := `{"username":"` + strings.Repeat("a", maxSmallJSONBody) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(big))
	var body struct {
		Username string `json:"username"`
	}
	assert.Error(t, decodeSmallJSON(httptest.NewRecorder(), r, &body))

	r = httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"alice"}`))
	assert.NoError(t, decodeSmallJSON(httptest.NewRecorder(), r, &body))
	assert.Equal(t, "alice", body.Username)
}
