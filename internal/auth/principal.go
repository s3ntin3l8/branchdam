// Package auth is the only place in branchDAM that reads Authentik's
// identity headers. Two request paths reach the server: browser traffic
// behind Traefik's ForwardAuth (which asserts X-Authentik-* headers) and
// agent traffic on /api/v1/agent/*, which bypasses ForwardAuth entirely and
// authenticates via a static X-API-Key instead (spec §5). Because the agent
// router bypasses ForwardAuth, a client hitting it directly could forge
// X-Authentik-Username -- BrowserChain and AgentChain together make that
// structurally impossible rather than a discipline everyone has to
// remember: AgentChain deletes every X-Authentik-* header before anything
// downstream runs, and BrowserChain is the ONLY code that ever reads them.
// TestNoDirectAuthentikHeaderReads enforces the second half of that by
// grepping the rest of the repo.
package auth

import "context"

// Kind distinguishes a human session (browser, via Authentik ForwardAuth)
// from a machine session (workstation agent, via X-API-Key).
type Kind string

const (
	KindUser    Kind = "user"
	KindMachine Kind = "machine"
	// KindSystem is reserved for background-worker attribution that
	// never came in over the wire (sweeper INCREMENTAL passes, prune,
	// anything that runs without a request Principal). It is NEVER
	// attached by BrowserChain or AgentChain -- callers that want to
	// log a system action must use internal/users.SystemActor (or pass
	// this Kind directly) explicitly.
	KindSystem Kind = "system"
)

// AuthProviderLocal identifies the auth_provider value used for users who
// authenticate via the local session-cookie chain (source='local' rows in
// the users table). The attribution layer (internal/users) uses this to
// resolve local-session Principals to stable users.id values without
// going through the Authentik-style INSERT path (which would violate the
// CHECK constraint on source/password_hash).
const AuthProviderLocal = "local"

// Principal is what a request is authenticated as. A machine Principal
// after #companion-pairing carries Name = agent_id (the device that owns
// the API key) for device-paired sessions, OR Name = "env-bootstrap"
// for legacy env-var-key sessions. Email and Groups are always empty
// for KindMachine -- they describe a human identity (from Authentik
// ForwardAuth) which is meaningless for a machine. Callers that render
// Principal must gate display on p.Kind == KindUser to avoid printing
// a string like "iphone-a3f9c2e1" in a place that expects a human name.
//
// ExternalUID is the stable identity key the multi-user attribution layer
// (internal/users) uses to lazy-provision a row in `users` keyed on
// (auth_provider, external_uid). For KindUser it comes from Authentik's
// X-Authentik-Uid header (stable across username renames) for forward-auth
// sessions, or the local username for local sessions. For KindMachine it
// is empty -- machine sessions never own user-attributed rows. Name is
// the display label (X-Authentik-Username or local username); ExternalUID
// is the key.
//
// AuthProvider identifies which auth backend authenticated this Principal.
// It maps to the `auth_provider` column in the `users` table and
// determines which UPSERT path ResolveOrCreate takes. Empty string
// defaults to "authentik" for backward compatibility with forward-auth
// Principals that predate this field. AuthProviderLocal ("local") is set
// by the session middleware for local-cookie sessions.
//
// Authenticated is true iff BrowserChain saw a non-empty
// X-Authentik-Username (#164). BrowserChain always attaches a Principal --
// even with zero Authentik headers -- so reads, /healthz, the SSE stream,
// and the SPA shell keep working regardless; Authenticated is what lets
// RequireAdmin distinguish "a real logged-in user with no matching admin
// group" from "no identity headers arrived at all" on a write route, which
// it could not do before this field existed (see authz.go's RequireAdmin).
// Always false (and never read) for a KindMachine Principal -- AgentChain
// gates by API key instead, not by this field.
type Principal struct {
	Kind          Kind
	Name          string
	Email         string
	Groups        []string
	ExternalUID   string
	AuthProvider  string
	Authenticated bool
}

type principalContextKey struct{}

// From returns the Principal attached to ctx by BrowserChain or AgentChain,
// and whether one was present at all.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

// WithPrincipal attaches a Principal to ctx (primarily used by BrowserChain, AgentChain, and tests).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return WithPrincipal(ctx, p)
}
