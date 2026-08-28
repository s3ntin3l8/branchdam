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
)

// Principal is what a request is authenticated as. A machine Principal
// deliberately carries no Name/Email/Groups -- there is nothing for the
// agent router to assert about *which* user is making the request, only
// that the request holds the shared secret. Giving it empty identity
// fields (rather than, say, a "system" placeholder name) makes it visibly
// distinct from a real user in any audit log or UI that renders Principal.
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
