package auth

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// AgentPathPrefix matches the spec's Traefik agent router rule
// (PathPrefix(`/api/v1/agent`)) and must stay in sync with it -- whichever
// paths Traefik routes around ForwardAuth are exactly the paths this
// package needs to treat as untrusted-until-keyed.
const AgentPathPrefix = "/api/v1/agent"

// AuthMode selects which auth chains the routing layer runs. The mode
// governs how BrowserChain (forward-auth) and LocalChain (session-cookie)
// compose into a single Principal -- see PLAN.md §3 for the full
// semantics.
type AuthMode string

const (
	// AuthModeForward is today's behavior: forward-auth headers only.
	AuthModeForward AuthMode = "forward"
	// AuthModeLocal is the local-only mode: session-cookie only.
	AuthModeLocal AuthMode = "local"
	// AuthModeBoth runs both chains and merges (union of Groups, prefer
	// local for Name/Email). Default for new installs.
	AuthModeBoth AuthMode = "both"
)

// ChainBuilder is the constructor shape for any auth chain -- takes a
// `next` handler, returns a wrapped handler that runs identity work and
// then calls next. BrowserChain and the local session middleware both
// satisfy this; tests use it to inject fakes.
type ChainBuilder func(next http.Handler) http.Handler

// Route is the only place that decides which auth chain applies to a
// request: AgentChain for AgentPathPrefix, then forward-auth and/or
// local-cookie on the browser path. next is the shared handler both
// chains eventually call.
func Route(apiKey string, log *slog.Logger, next http.Handler) http.Handler {
	return RouteWithConfig(AgentConfig{APIKey: apiKey}, AuthModeForward, nil, log, next)
}

// JITProvisioner is the function-shape hook RouteWithConfig calls
// after merging the forward + local Principals but before the real
// next runs. The hook is the integration point for forward-JIT
// provisioning (a forward-auth admin user who has no local row yet
// gets one created on first sight, so the local `is_admin` override
// fires for them) -- see PLAN.md §5b. Nil disables the hook.
//
// Inputs: the merged Principal (may be nil if neither chain
// authenticated the request), the config-driven allowlist of admin
// groups, and the requireEmailForJIT knob. Output: a LocalUserView to
// attach to ctx if a user was created (or already exists).
type JITProvisioner func(ctx context.Context, merged *Principal, adminGroups []string, requireEmail bool) (LocalUserView, error)

// RouteWithConfig routes requests using the provided AgentConfig.
//
//   - mode governs which browser-side chains run.
//   - localBuilder may be nil in forward-only mode. When set and
//     mode != AuthModeForward, it runs alongside the forward chain
//     and the two outputs are merged.
//
// No-JIT variant. For JIT-enabled construction (auth.mode == "both"
// with config.forward.adminGroups), use RouteWithConfigAndJIT -- the
// no-JIT default keeps the common path's signature short and its
// semantics obvious.
func RouteWithConfig(cfg AgentConfig, mode AuthMode, localBuilder ChainBuilder, log *slog.Logger, next http.Handler) http.Handler {
	return RouteWithConfigAndJIT(cfg, mode, localBuilder, nil, nil, false, log, next)
}

// RouteWithConfigAndJIT is the full constructor with JIT support.
//
//   - jit, when non-nil, runs after the merge to provision a local
//     user row for a forward-auth admin who has no local account
//     yet. Nil = no JIT.
//   - adminGroups is the allowlist of forward-auth group names that
//     trigger JIT provisioning (see Config.Auth.Forward.AdminGroups).
//   - requireEmail is the requireEmailForJIT config knob.
//
// IMPORTANT: the agent chain is constructed ONCE here, not per-request.
// Per-request construction would build a fresh in-memory ReplayCache
// per call (AgentConfig.Cache defaults to nil -> NewReplayCache() inside
// AgentChainWithConfig), defeating replay protection.
func RouteWithConfigAndJIT(cfg AgentConfig, mode AuthMode, localBuilder ChainBuilder, jit JITProvisioner, adminGroups []string, requireEmail bool, log *slog.Logger, next http.Handler) http.Handler {
	agentHandler := AgentChainWithConfig(cfg, log)(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, AgentPathPrefix) {
			agentHandler.ServeHTTP(w, r)
			return
		}

		fwdCap := &principalCapture{}
		localCap := &principalCapture{}

		// Reconstruct forward + (when applicable) local against the
		// capture-only nexts each request. Forward is stateless
		// (BrowserChain), so per-request reconstruction is cheap.
		// Local MUST be supplied as a builder for the same reason --
		// the session middleware carries per-request state (the
		// request's cookie) but NOT shared state across requests.
		forwardBuilder := func(n http.Handler) http.Handler { return BrowserChain(n) }
		forwardBuilder(captureHandler(fwdCap)).ServeHTTP(&blackholeRW{ResponseWriter: w}, r)

		if mode != AuthModeForward && localBuilder != nil {
			localBuilder(captureHandler(localCap)).ServeHTTP(&blackholeRW{ResponseWriter: w}, r)
		}

		merged := mergePrincipals(fwdCap.principal, localCap.principal, mode)

		// Forward-JIT: a forward-auth admin with no local row yet gets
		// one provisioned on first sight, so the local `is_admin`
		// override (IsAdmin's LocalUserView branch) fires for them.
		// Only runs when (a) mode is "both" so the local DB is even
		// active, (b) the merged principal came from the forward chain
		// alone (no local session), and (c) the forward-auth asserted
		// groups intersect the configured adminGroups allowlist. The
		// hook is responsible for the actual DB write.
		var localView LocalUserView
		if jit != nil && mode == AuthModeBoth && merged != nil && localCap.principal == nil {
			view, jitErr := jit(r.Context(), merged, adminGroups, requireEmail)
			if jitErr != nil {
				log.Warn("auth: forward-JIT provisioning failed; continuing without local-`is_admin` override", "err", jitErr.Error())
			} else if view.UserID != 0 {
				localView = view
			}
		}

		if merged != nil {
			r = r.WithContext(WithPrincipal(r.Context(), *merged))
		}
		if localView.UserID != 0 {
			r = r.WithContext(WithLocalUserView(r.Context(), localView))
		}
		next.ServeHTTP(w, r)
	})
}

// principalCapture is the slot both chains write their would-be-set
// Principal into. Captured from the per-chain capture-only next.
type principalCapture struct {
	principal *Principal
}

func captureHandler(cap *principalCapture) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if p, ok := From(r.Context()); ok {
			cap.principal = &p
		}
	})
}

// blackholeRW is a http.ResponseWriter that discards everything written
// to it. Used to invoke a chain for its side effect (ctx Principal
// attachment) without producing output.
type blackholeRW struct {
	http.ResponseWriter
}

func (b *blackholeRW) Write(p []byte) (int, error) { return len(p), nil }
func (b *blackholeRW) WriteHeader(_ int)           {}

// mergePrincipals returns the unioned Principal per PLAN.md §5.
//
//   - forward-only: returns fwd (or nil if no header arrived).
//   - local-only: returns local (or nil if no cookie).
//   - both: union of Groups; Name/Email prefer local (it's an
//     explicit interactive login); Authenticated = either.
//
// Returns nil when neither chain produced a Principal -- the request
// continues with no Principal attached, exactly matching today's
// BrowserChain behavior for unauthenticated requests.
func mergePrincipals(fwd, local *Principal, mode AuthMode) *Principal {
	switch mode {
	case AuthModeForward:
		return fwd
	case AuthModeLocal:
		return local
	case AuthModeBoth:
		if fwd == nil && local == nil {
			return nil
		}
		if fwd == nil {
			p := *local
			return &p
		}
		if local == nil {
			p := *fwd
			return &p
		}
		merged := Principal{
			Kind:          KindUser,
			Name:          pickName(local.Name, fwd.Name),
			Email:         pickName(local.Email, fwd.Email),
			Groups:        unionGroups(local.Groups, fwd.Groups),
			Authenticated: local.Authenticated || fwd.Authenticated,
		}
		return &merged
	}
	return nil
}

// pickName returns the first non-empty string. Used to prefer a local
// interactive login's name/email over a forward-auth assertion when
// both are present.
func pickName(local, forward string) string {
	if local != "" {
		return local
	}
	return forward
}

// unionGroups returns the union of two group slices. Allocation-free
// for the empty case; O(N+M) otherwise.
func unionGroups(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	for _, g := range b {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}
