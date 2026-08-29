package auth

import (
	"log/slog"
	"net/http"
	"strings"
)

// AgentPathPrefix matches the spec's Traefik agent router rule
// (PathPrefix(`/api/v1/agent`)) and must stay in sync with it -- whichever
// paths Traefik routes around ForwardAuth are exactly the paths this
// package needs to treat as untrusted-until-keyed.
const AgentPathPrefix = "/api/v1/agent"

// Route is the only place that decides which auth chain applies to a
// request: AgentChain for AgentPathPrefix, BrowserChain
// for everything else. next is the shared handler both chains eventually call.
func Route(apiKey string, log *slog.Logger, next http.Handler) http.Handler {
	agent := AgentChain(apiKey, log)(next)
	browser := BrowserChain(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, AgentPathPrefix) {
			agent.ServeHTTP(w, r)
			return
		}
		browser.ServeHTTP(w, r)
	})
}
