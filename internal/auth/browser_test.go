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
	req.Header.Set(authentikUidHeader, "alice-uid-stable")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	want := Principal{Kind: KindUser, Name: "alice", Email: "alice@example.com", Groups: []string{"dam-admins", "dam-users"}, ExternalUID: "alice-uid-stable", Authenticated: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Principal = %+v, want %+v", got, want)
	}
}

// TestBrowserChainNoHeadersYieldsUnauthenticatedPrincipal backs #164: with
// zero Authentik headers, BrowserChain still attaches a Principal (so
// reads, /healthz, SSE, and the SPA shell keep working) but it must be
// Authenticated: false -- that's what lets RequireAdmin fail closed on a
// write route instead of treating this as a real logged-in user with no
// matching group.
func TestBrowserChainNoHeadersYieldsUnauthenticatedPrincipal(t *testing.T) {
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
	if got.Authenticated {
		t.Errorf("Principal.Authenticated = true, want false when no headers are set")
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
	if got.Authenticated {
		t.Errorf("Principal.Authenticated = true, want false (X-API-Key is not an Authentik identity header)")
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

// TestBrowserChainExternalUIDFallback: when X-Authentik-Uid is missing
// (older Authentik deployments or dev setups that skip Traefik),
// BrowserChain falls back to the username so the attribution layer still
// has a non-empty key. The unique index on (auth_provider, external_uid)
// means this is safe across the same Authentik instance -- only a rename
// would fragment attribution, and that's exactly the case the
// X-Authentik-Uid header is supposed to prevent.
func TestBrowserChainExternalUIDFallback(t *testing.T) {
	var got Principal
	handler := BrowserChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = From(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	req.Header.Set(authentikUsernameHeader, "alice")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got.ExternalUID != "alice" {
		t.Errorf("ExternalUID = %q, want %q (fallback to username)", got.ExternalUID, "alice")
	}
}

func TestPickExternalUID(t *testing.T) {
	cases := []struct {
		uid, name, want string
	}{
		{"stable-uid", "alice", "stable-uid"}, // preferred: opaque id wins
		{"", "alice", "alice"},                // fallback: missing uid uses name
		{"", "", ""},                          // nothing in -> nothing out
		{"stable-uid", "", "stable-uid"},      // uid alone still propagates
	}
	for _, c := range cases {
		if got := pickExternalUID(c.uid, c.name); got != c.want {
			t.Errorf("pickExternalUID(%q,%q) = %q, want %q", c.uid, c.name, got, c.want)
		}
	}
}
