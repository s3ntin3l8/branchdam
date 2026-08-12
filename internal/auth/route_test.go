package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteSelectsChainByPathPrefix(t *testing.T) {
	var got Principal
	handler := Route(testKey, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = From(r.Context())
	}))

	cases := []struct {
		path     string
		wantKind Kind
	}{
		{"/api/v1/agent/hello", KindMachine},
		{"/api/v1/agent/events", KindMachine},
		{"/api/v1/assets", KindUser},
		{"/api/v1/edges/audit", KindUser},
		{"/", KindUser},
		{"/api/v1/agentic-but-not-really", KindMachine}, // prefix match is intentionally simple; document the edge case rather than hide it
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			req.Header.Set(apiKeyHeader, testKey) // present on every request; only consulted on the agent path
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if got.Kind != c.wantKind {
				t.Errorf("path %s: Kind = %q, want %q", c.path, got.Kind, c.wantKind)
			}
		})
	}
}

func TestRouteAgentPathRequiresKey(t *testing.T) {
	handlerCalled := false
	handler := Route(testKey, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, AgentPathPrefix+"/hello", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if handlerCalled {
		t.Error("handler was called on the agent path with no key")
	}
}
