# Forward-Auth / SSO (Authentik walkthrough)

branchDAM doesn't ship a built-in OIDC client -- it trusts identity headers asserted by a
forward-auth proxy sitting in front of it, per spec §5. Traefik's ForwardAuth middleware talks
to Authentik's outpost, which does the real authentication; branchDAM's browser router just
reads the result. Agent/API traffic (the workstation agent, once it exists) takes a completely
different path: it never goes through ForwardAuth at all, and authenticates with a static
`X-API-Key` instead. Getting the split between those two paths right is the entire point of
this document -- see [`internal/auth`](../internal/auth)'s package doc for the code-level
guarantees, and `docs/schema.md` fix notes for why the underlying design looks the way it does.

This walkthrough follows the same shape as `runway-ai-usage-tracker/docs/forward-auth.md` and
`traefik-viewer/docs/authentik.md` -- both already run this exact pattern in production.

## 1. Authentik: Proxy Provider + Outpost

1. In Authentik, create a **Provider** → *Proxy Provider*, mode **"Forward auth (single
   application)"**, pointed at branchDAM's external URL (e.g. `https://dam.example.com`).
2. Create an **Application** bound to that provider, and assign it to the group(s) that should
   be allowed to reach branchDAM. This is Authentik's own authorization gate -- it decides who
   ever reaches the outpost at all, before branchDAM sees a request.
3. Attach the provider to your outpost (the embedded outpost works fine for a single app).

The outpost asserts identity via headers on the request it forwards through:
`X-Authentik-Username`, `X-Authentik-Email`, `X-Authentik-Groups` (pipe-`|`-delimited --
branchDAM's `internal/auth.BrowserChain` splits on `|`, not `,`).

## 2. Traefik: the three-router split

branchDAM's `compose.yaml` ships three routers on one service, which is the fix for a real
defect in the original spec's compose file (two `traefik.http.services.*` blocks, neither
router setting `.service=`, which Traefik can't resolve). Each has an explicit `.service=` and
`priority` -- never rely on rule-length ordering to decide which router wins.

```yaml
labels:
  traefik.enable: "true"
  traefik.http.services.branchdam.loadbalancer.server.port: "8080"

  # Agent API -- bypasses ForwardAuth by design (static X-API-Key instead)
  traefik.http.routers.branchdam-agent.rule: "Host(`dam.example.com`) && PathPrefix(`/api/v1/agent`)"
  traefik.http.routers.branchdam-agent.priority: "100"
  traefik.http.routers.branchdam-agent.service: "branchdam"
  traefik.http.routers.branchdam-agent.middlewares: "strip-identity@file"

  # Browser UI -- behind ForwardAuth
  traefik.http.routers.branchdam.rule: "Host(`dam.example.com`)"
  traefik.http.routers.branchdam.priority: "10"
  traefik.http.routers.branchdam.service: "branchdam"
  traefik.http.routers.branchdam.middlewares: "strip-identity@file,authentik@file"

  # Outpost's own login-flow/logout path -- routed to Authentik, not this app
  traefik.http.routers.branchdam-outpost.rule: "Host(`dam.example.com`) && PathPrefix(`/outpost.goauthentik.io/`)"
  traefik.http.routers.branchdam-outpost.priority: "200"
  traefik.http.routers.branchdam-outpost.service: "authentik"
```

Define the two middlewares once in your Traefik dynamic config (`@file` provider, not
`@docker` -- these apply repo-wide, not just to this one container):

```yaml
# dynamic/middlewares.yml
http:
  middlewares:
    strip-identity:
      headers:
        customRequestHeaders:
          X-Authentik-Username: ""
          X-Authentik-Email: ""
          X-Authentik-Groups: ""
          X-Authentik-Uid: ""
          X-Authentik-Name: ""
    authentik:
      forwardAuth:
        address: "http://authentik-outpost:9000/outpost.goauthentik.io/auth/traefik"
        trustForwardHeader: true
        authResponseHeaders:
          - X-Authentik-Username
          - X-Authentik-Groups
          - X-Authentik-Email
          - X-Authentik-Uid
          - X-Authentik-Name
```

### Why `strip-identity@file` is on the agent router too

This is the load-bearing detail, and it's easy to get backwards. The agent router
(`branchdam-agent`) **bypasses** `authentik@file` entirely -- that's the whole point, since a
workstation agent authenticates with a shared key, not an interactive SSO session. But bypassing
the outpost means nothing upstream of branchDAM ever strips a client-supplied
`X-Authentik-Username` header on that path. Without `strip-identity@file` on *both* routers, a
request straight to `/api/v1/agent/*` could set `X-Authentik-Username: admin` itself and have it
survive all the way to the application.

`internal/auth.AgentChain` also strips these headers in Go, unconditionally, before checking the
API key at all (see its doc comment) -- defense in depth, not a substitute for stripping at the
edge. `internal/auth.BrowserChain` is the *only* code in the repo permitted to read
`X-Authentik-*` at all; `TestNoDirectAuthentikHeaderReads` greps the rest of the repo for that
string and fails the build if anything else references it.

## 3. branchDAM: agent key and trust configuration

```bash
# .env (gitignored) -- see .env.example
BRANCHDAM_AGENT_API_KEY=<32+ random characters, e.g. `openssl rand -hex 32`>
```

If this key is unset or under 32 characters, every `/api/v1/agent/*` request fails closed with
`503` -- logged once at startup, not silently treated as "no auth required"
(`internal/auth.AgentChain`).

The browser side needs no configuration of its own: `BrowserChain` trusts whatever
`X-Authentik-*` headers arrive, because Traefik's `strip-identity@file → authentik@file` chain
(§2) is what makes that trust well-founded. branchDAM has no equivalent of runway's
`TRUSTED_PROXY_IPS` allowlist yet -- the network boundary (branchDAM's container publishes no
port; only Traefik can reach `:8080` at all, per `compose.yaml`) is the current enforcement
point. Revisit if branchDAM's network topology ever changes to allow other containers to reach
it directly.

A request with **no** `X-Authentik-Username` header at all -- Traefik's `authResponseHeaders`
misconfigured to drop it, or a direct hit on the port from inside the network boundary above --
is treated differently depending on the request. Reads (`GET`/`HEAD`/`OPTIONS`), `/healthz`, the
SSE stream, and the SPA shell still work: `BrowserChain` always attaches a `Principal`, so those
paths are unaffected. A write (`POST`/`PUT`/etc.) now gets `403 authentication required`
instead of silently running as a full admin session with a blank username -- previously, an
empty `authz.groups` (the solo-homelab default) meant *any* authenticated-looking principal,
including one with no real identity behind it, could write. See `internal/auth.Principal`'s
`Authenticated` field and `RequireAdmin`.

## 4. Reaching the agent route off-LAN

Everything above assumes the caller can reach Traefik at all. That's a given on the home LAN;
it isn't for a travelling workstation doing field ingest away from the home network. The spec's
Pillar 3 names the expected answer explicitly: "Upon reconnecting (LAN or Tailscale)"
(`docs/spec/original-spec.md`) -- an overlay network (Tailscale, or an equivalent
WireGuard-based mesh) is the assumed transport for a workstation that isn't on the LAN.
branchDAM ships nothing Tailscale-specific to make this work -- no config key, no code path --
it's an operator-provided deployment prerequisite, same as the LAN itself.

The one detail that matters: the overlay has to land traffic on the same Traefik hostname and
router, not on the container port directly. §3 already states why a direct hit on `:8080`
matters -- "branchDAM's container publishes no port; only Traefik can reach `:8080` at all, per
`compose.yaml` ... Revisit if branchDAM's network topology ever changes to allow other
containers to reach it directly." Exposing `:8080` to tailnet peers would be exactly that
topology change, and it would defeat both of §2's enforcement points at once: `BrowserChain`
trusting `X-Authentik-*` unconditionally (no `strip-identity@file`, no `authentik@file` in
front of it), and the agent router's `strip-identity@file` -- the "load-bearing detail" in §2 --
that stops a caller from setting `X-Authentik-Username: admin` itself. The correct shape is
Tailscale (or split-DNS/MagicDNS) resolving `dam.example.com` to the tailnet address and still
routing through Traefik, so `Host()` matching, the TLS cert, and both middleware chains apply
unchanged -- no new router, no `:8080` exposure, no second `Host()` rule needed.

Given that, the auth model does **not** change based on network location. `AgentChain` itself
authenticates on `X-API-Key` and has no path-matching logic of its own -- the `PathPrefix`
dispatch to it happens in `internal/auth.Route` ("the only place that decides which auth chain
applies to a request", per its own doc comment) and in Traefik's `branchdam-agent` router rule
(§2), not in `AgentChain`. Neither `Route` nor `AgentChain` reads source IP or cares which
network the request arrived over, and (per §3) branchDAM has no `TRUSTED_PROXY_IPS`-style
allowlist to reconfigure per network either way. The security boundary is the key, not the
network: the same `BRANCHDAM_AGENT_API_KEY` that authenticates a request from the LAN
authenticates one arriving over the tailnet, with the same 503-on-unset-or-short-key behavior
described in §3 and the same 401-on-missing-key behavior demonstrated in §5's curl example.

The overlay is what makes the *sync* land promptly, not what makes ingest work at all -- per
Pillar 3 the (not-yet-built) workstation agent queues ingest events locally regardless of
connectivity and executes `SYNC_HANDSHAKE` on reconnect. That offline-queue/dual-copy/rebase
behavior is `s3ntin3l8/branchdam-agent#2` ("SD-card ingest engine -- dual-copy writer, offline
queue, Tier-0/3 rebase"; this repo's own #234, which originally tracked it, is closed in favor of
that issue), not #62 -- #62 is now scoped to only the tray app and the Luminar `catalog.db`
reader, which also ships in `branchdam-agent` (see `docs/roadmap.md`). Reachability just
determines whether "reconnect" means "back on the LAN" or "the tailnet came up."

## 5. Verifying it works

```bash
# Through the proxy, as a real browser session would see it:
curl -s https://dam.example.com/api/v1/me | jq
# → {"kind": "user", "name": "your-username", "groups": [...]}

# Agent path, with the shared key:
curl -s -X POST -H "X-API-Key: $BRANCHDAM_AGENT_API_KEY" https://dam.example.com/api/v1/agent/hello | jq
# → {"ok": true, "version": "..."}

# Agent path WITHOUT the key -- must be 401, not silently authenticated:
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://dam.example.com/api/v1/agent/hello
# → 401
```

`hello` is registered `POST`-only, so `-X POST` above is required. Auth runs ahead of routing, so
the no-key 401 check fires the same with or without it. A bare (`GET`) `curl` against this path is
not an error, either -- the SPA's catch-all route (`GET /`) absorbs it and returns `200` with the
HTML shell, not `405` and not JSON, which makes it easy to mistake for a working call.

If `/api/v1/me` returns `"kind": "user"` with an empty `name` -- or a write request that used to
work now returns `403 authentication required` -- confirm:

- `authResponseHeaders` on the `authentik` middleware actually lists the headers (Traefik
  strips anything not explicitly listed there, even from a trusted forwardAuth response).
- The outpost's application binding actually includes your account's group.
- You're hitting the `branchdam` router, not accidentally `branchdam-agent` (check the request
  path doesn't start with `/api/v1/agent`).
