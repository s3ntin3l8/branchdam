package auth

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestBrowserChainAttachesPrincipalFromHeaders(t *testing.T) {
	var got Principal
	handler := BrowserChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := From(r.Context())
		if !ok {
			t.Error("handler saw no Principal in context")
		}
		got = p
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	req.Header.Set(authentikUsernameHeader, "alice")
	req.Header.Set(authentikEmailHeader, "alice@example.com")
	req.Header.Set(authentikGroupsHeader, "dam-admins|dam-users")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	want := Principal{Kind: KindUser, Name: "alice", Email: "alice@example.com", Groups: []string{"dam-admins", "dam-users"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Principal = %+v, want %+v", got, want)
	}
}

func TestBrowserChainNoHeadersYieldsAnonymousPrincipal(t *testing.T) {
	var got Principal
	handler := BrowserChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = From(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got.Kind != KindUser {
		t.Errorf("Kind = %q, want %q", got.Kind, KindUser)
	}
	if got.Name != "" || len(got.Groups) != 0 {
		t.Errorf("Principal = %+v, want empty Name/Groups when no headers are set", got)
	}
}

func TestBrowserChainXAPIKeyGrantsNothing(t *testing.T) {
	// An X-API-Key on a browser-chain request must not somehow grant
	// machine-level access -- BrowserChain never even looks at it.
	var got Principal
	handler := BrowserChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = From(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/edges/audit", nil)
	req.Header.Set(apiKeyHeader, testKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got.Kind != KindUser {
		t.Errorf("Kind = %q, want %q (X-API-Key alone must not grant machine access on browser routes)", got.Kind, KindUser)
	}
}

func TestSplitGroups(t *testing.T) {
	cases := map[string][]string{
		"":        nil,
		"a":       {"a"},
		"a|b|c":   {"a", "b", "c"},
		"a| b |c": {"a", "b", "c"},
		"a||b":    {"a", "b"},
		"a,b":     {"a,b"}, // comma is NOT the delimiter -- must not be split on
	}
	for in, want := range cases {
		got := splitGroups(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("splitGroups(%q) = %v, want %v", in, got, want)
		}
	}
}
