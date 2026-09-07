package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeChain is a ChainBuilder that sets a Principal on the request's
// context if set != nil; otherwise attaches nothing. Used to test
// mergePrincipals' union behavior without a real session middleware.
func fakeChain(p Principal) ChainBuilder {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if p.Name != "" || p.Email != "" || len(p.Groups) > 0 || p.Authenticated {
				ctx = WithPrincipal(ctx, p)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func noopChain() ChainBuilder {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}
}

func TestMergePrincipals_ForwardOnly(t *testing.T) {
	fwd := &Principal{Kind: KindUser, Name: "alice", Email: "alice@x", Groups: []string{"dam-admins"}, Authenticated: true}
	got := mergePrincipals(fwd, nil, AuthModeForward)
	assert.NotNil(t, got)
	assert.Equal(t, "alice", got.Name)
}

func TestMergePrincipals_LocalOnly(t *testing.T) {
	local := &Principal{Kind: KindUser, Name: "bob", Email: "bob@x", Authenticated: true}
	got := mergePrincipals(nil, local, AuthModeLocal)
	assert.NotNil(t, got)
	assert.Equal(t, "bob", got.Name)
}

func TestMergePrincipals_Both_Neither(t *testing.T) {
	assert.Nil(t, mergePrincipals(nil, nil, AuthModeBoth))
}

func TestMergePrincipals_Both_OnlyLocal(t *testing.T) {
	local := &Principal{Kind: KindUser, Name: "bob", Authenticated: true}
	got := mergePrincipals(nil, local, AuthModeBoth)
	assert.NotNil(t, got)
	assert.Equal(t, "bob", got.Name)
}

func TestMergePrincipals_Both_OnlyForward(t *testing.T) {
	fwd := &Principal{Kind: KindUser, Name: "alice", Groups: []string{"g1"}, Authenticated: true}
	got := mergePrincipals(fwd, nil, AuthModeBoth)
	assert.NotNil(t, got)
	assert.Equal(t, "alice", got.Name)
	assert.Equal(t, []string{"g1"}, got.Groups)
}

func TestMergePrincipals_Both_Both_UnionGroupsAndPreferLocal(t *testing.T) {
	local := &Principal{
		Kind: KindUser, Name: "alice", Email: "alice@example.com",
		Authenticated: true,
	}
	fwd := &Principal{
		Kind: KindUser, Name: "alice", Email: "alice@authentik",
		Groups:        []string{"dam-admins", "users"},
		Authenticated: true,
	}
	got := mergePrincipals(fwd, local, AuthModeBoth)
	assert.NotNil(t, got)
	assert.Equal(t, "alice", got.Name)
	// Local email wins on collision (it's the interactive login's source of truth).
	assert.Equal(t, "alice@example.com", got.Email)
	// Groups union even though local has none.
	assert.Equal(t, []string{"dam-admins", "users"}, got.Groups)
	assert.True(t, got.Authenticated)
}

func TestRouteWithConfig_ForwardMode_FallsBackToForwardWhenLocalIsNil(t *testing.T) {
	// Smoke test: a forward-only request gets a forward Principal.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("X-Authentik-Username", "alice")
	r.Header.Set("X-Authentik-Groups", "dam-admins")

	var observed *Principal
	rec := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if p, ok := From(r.Context()); ok {
			observed = &p
		}
	})

	h := RouteWithConfig(AgentConfig{APIKey: ""}, AuthModeForward, nil, testLogger(), rec)
	h.ServeHTTP(w, r)

	assert.NotNil(t, observed)
	assert.Equal(t, "alice", observed.Name)
}

func TestRouteWithConfig_BothMode_LocalOverridesForward(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	// Forward-auth says "alice@authentik"
	r.Header.Set("X-Authentik-Username", "alice-fwd")
	r.Header.Set("X-Authentik-Email", "alice@authentik")
	r.Header.Set("X-Authentik-Groups", "dam-admins")
	// Local chain says "alice" via the cookie chain's Principal.

	var observed *Principal
	rec := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if p, ok := From(r.Context()); ok {
			observed = &p
		}
	})

	localChain := fakeChain(Principal{
		Kind:          KindUser,
		Name:          "alice-local",
		Email:         "alice@example.com",
		Authenticated: true,
	})

	h := RouteWithConfig(AgentConfig{APIKey: ""}, AuthModeBoth, localChain, testLogger(), rec)
	h.ServeHTTP(w, r)

	assert.NotNil(t, observed)
	// Local wins on Name collision (it's the interactive login).
	assert.Equal(t, "alice-local", observed.Name)
	assert.Equal(t, "alice@example.com", observed.Email)
	// Groups union: forward's "dam-admins" carries through.
	assert.Equal(t, []string{"dam-admins"}, observed.Groups)
}

func TestRouteWithConfig_AgentPathSkipsBothChains(t *testing.T) {
	// Agent path always uses AgentChain (X-API-Key). Both chain
	// outputs must NOT contaminate the request's Principal.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	r.Header.Set("X-Authentik-Username", "alice") // would normally attach forward

	var observed *Principal
	rec := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if p, ok := From(r.Context()); ok {
			observed = &p
		}
	})

	localChain := fakeChain(Principal{Kind: KindUser, Name: "alice", Authenticated: true})
	h := RouteWithConfig(AgentConfig{APIKey: ""}, AuthModeBoth, localChain, testLogger(), rec)
	h.ServeHTTP(w, r)

	// AgentChain didn't authenticate (no API key), so no Principal -- but
	// importantly, the forward/local "alice" was NOT attached.
	assert.Nil(t, observed)
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
