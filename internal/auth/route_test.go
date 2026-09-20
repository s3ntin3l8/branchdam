package auth

import (
	"context"
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
		Kind:          KindUser,
		Name:          "alice",
		Email:         "alice@example.com",
		ExternalUID:   "alice",
		AuthProvider:  AuthProviderLocal,
		Authenticated: true,
	}
	fwd := &Principal{
		Kind:          KindUser,
		Name:          "alice",
		Email:         "alice@authentik",
		ExternalUID:   "abc123-uuid",
		AuthProvider:  "authentik",
		Groups:        []string{"dam-admins", "users"},
		Authenticated: true,
	}
	got := mergePrincipals(fwd, local, AuthModeBoth)
	assert.NotNil(t, got)
	assert.Equal(t, "alice", got.Name)
	// Local email wins on collision (it's the interactive login's source of truth).
	assert.Equal(t, "alice@example.com", got.Email)
	// Local ExternalUID/AuthProvider win (interactive login).
	assert.Equal(t, "alice", got.ExternalUID)
	assert.Equal(t, AuthProviderLocal, got.AuthProvider)
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

	h := RouteWithConfig(AgentConfig{}, AuthModeForward, nil, testLogger(), rec)
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

	h := RouteWithConfig(AgentConfig{}, AuthModeBoth, localChain, testLogger(), rec)
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
	h := RouteWithConfig(AgentConfig{}, AuthModeBoth, localChain, testLogger(), rec)
	h.ServeHTTP(w, r)

	// AgentChain didn't authenticate (no API key), so no Principal -- but
	// importantly, the forward/local "alice" was NOT attached.
	assert.Nil(t, observed)
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeChainWithView is a ChainBuilder that sets both a Principal and a
// LocalUserView on the request context. Used to test that LocalUserView
// survives the capture-merge flow in RouteWithConfigAndJIT.
func fakeChainWithView(p Principal, view LocalUserView) ChainBuilder {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if p.Name != "" || p.Email != "" || len(p.Groups) > 0 || p.Authenticated {
				ctx = WithPrincipal(ctx, p)
			}
			if view.UserID != 0 {
				ctx = WithLocalUserView(ctx, view)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func TestRouteWithConfig_BothMode_LocalViewPropagated(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	// No forward-auth headers — local chain only.

	var observedLocalView *LocalUserView
	rec := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		v, ok := FromUser(r.Context())
		if ok {
			observedLocalView = &v
		}
	})

	localChain := fakeChainWithView(
		Principal{Kind: KindUser, Name: "bob", Authenticated: true},
		LocalUserView{UserID: 42, IsAdmin: true},
	)

	h := RouteWithConfig(AgentConfig{}, AuthModeBoth, localChain, testLogger(), rec)
	h.ServeHTTP(w, r)

	if observedLocalView == nil {
		t.Fatal("LocalUserView not propagated through capture merge")
	}
	if observedLocalView.UserID != 42 {
		t.Errorf("LocalUserView.UserID = %d, want 42", observedLocalView.UserID)
	}
	if !observedLocalView.IsAdmin {
		t.Error("LocalUserView.IsAdmin = false, want true")
	}
}

func TestRouteWithConfig_BothMode_JITDoesNotOverwriteLocalView(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	r.Header.Set("X-Authentik-Username", "alice-fwd")

	var observedLocalView *LocalUserView
	var observedPrincipal *Principal
	rec := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if p, ok := From(r.Context()); ok {
			observedPrincipal = &p
		}
		v, ok := FromUser(r.Context())
		if ok {
			observedLocalView = &v
		}
	})

	// Local chain fires with a session — JIT should NOT run (localCap.principal != nil).
	localChain := fakeChainWithView(
		Principal{Kind: KindUser, Name: "alice-local", Authenticated: true},
		LocalUserView{UserID: 7, IsAdmin: false},
	)

	// JIT provisioner that would create a view — should not be called.
	jitCalled := false
	jit := func(_ context.Context, _ *Principal, _ []string, _ bool) (LocalUserView, error) {
		jitCalled = true
		return LocalUserView{UserID: 99, IsAdmin: true}, nil
	}

	h := RouteWithConfigAndJIT(
		AgentConfig{}, AuthModeBoth, localChain,
		jit, nil, false, testLogger(), rec,
	)
	h.ServeHTTP(w, r)

	if jitCalled {
		t.Error("JIT provisioner should not fire when local session exists")
	}
	if observedLocalView == nil {
		t.Fatal("LocalUserView not propagated from local session")
	}
	if observedLocalView.UserID != 7 {
		t.Errorf("LocalUserView.UserID = %d, want 7 (from local session, not JIT)", observedLocalView.UserID)
	}
	if observedPrincipal == nil {
		t.Fatal("Principal not propagated")
	}
	// Local name wins in merge.
	if observedPrincipal.Name != "alice-local" {
		t.Errorf("Principal.Name = %q, want %q", observedPrincipal.Name, "alice-local")
	}
}
