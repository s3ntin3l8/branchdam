# AI assistant support (MCP) — design

Status: **design, not yet implemented.** This document is the
reference for the MCP epic. Each phase below is its own issue and PR.

## Goal

Let AI assistants work with a branchDAM library:

- **Clients:** Claude Code and Claude Desktop on the LAN first, then
  claude.ai web and mobile as a remote connector.
- **Read:** search assets, explain lineage, inspect metadata and look
  at thumbnails.
- **Review:** help work through the edge-review queue.
- **Enrich:** add assistant-written tags and descriptions.

The protocol is the [Model Context Protocol](https://modelcontextprotocol.io)
(MCP).

## Decisions

| Question | Decision |
|---|---|
| Where does the MCP server run? | **Inside the branchDAM process**, Streamable HTTP at `/mcp`. There is no extra container and no separate CLI. |
| Separate CLI for the server? | **No.** `cmd/branchdam` has no client mode. A CLI would be a second HTTP client hitting the same auth wall as MCP. An in-process MCP server reuses the DB, queries, auth and audit directly, and every MCP client (including Claude Code) speaks HTTP. The workstation is a different story: `branchdam-agent mcp` (stdio) covers local state the server can't see. It is tracked in the branchdam-agent repo. |
| Credential | v1: a **personal access token** with new `mcp:*` scopes. Phase 2: also OAuth 2.1 access tokens issued by Authentik. |
| What may an assistant change? | Edge review, manual edges, trash/restore, sync retry, and (phase 4) its own tags and annotations. **Never:** users, PATs, pairings, settings, restart, hard delete, scan or prune triggers. |
| Does branchDAM call an LLM itself? | **No.** The client model does the "seeing" through `view_asset`. The server holds no model API keys and incurs no model cost. |
| SDK | `github.com/modelcontextprotocol/go-sdk`, the official SDK. v1.8.0 is the latest stable release at the time of writing; pin the then-current stable at implementation time. |

## Current constraints (why phase 0 exists)

1. **Every PAT is an admin token.**
   - The token lookup requires `users.is_admin = 1 AND disabled_at IS NULL`
     (`internal/db/queries/*pat*.sql`, `PATService.Lookup`).
   - `PATMiddleware.RequirePAT` attaches
     `LocalUserView{IsAdmin: true}` unconditionally
     (`internal/auth/pat.go`).
   - The grantable scopes are only `admin`, `pats:write`,
     `pairings:write` and `*` (`PATGrantableScopes`).
   - Handing an LLM a PAT today hands it delete, user management,
     settings and restart.
2. **Scope is chosen by URL prefix.** `patScopeFor` in
   `internal/httpapi/server.go` maps path prefixes to scopes, and
   everything unknown becomes `admin`. MCP sends every `tools/call` as a
   `POST /mcp`, so a URL prefix can't tell a read tool from a write
   tool. That decision has to be made per tool, inside the MCP layer.
3. **Ingress.** In `compose.yaml`, Authentik ForwardAuth sits in
   front of every path except `/api/v1/agent*`. A bearer PAT sent from
   outside to any other path never reaches branchDAM.
4. **Audit.** `actor_audit` records the token's owner but not that a
   PAT (or which PAT) was used. A PAT principal's `ExternalUID` carries
   `pat:<hash prefix>`, but it is not persisted.
5. **Search.**
   - `q` is a case-insensitive substring match on `file_name`
     (`instr(lower(file_name), lower(?))`, not `LIKE`, because sqlc
     can't handle `LIKE ... ESCAPE`). It **also** matches an exact
     `node_uuid`, so a pasted UUID finds its node
     (`internal/db/queries/media_nodes.sql`).
   - There is no FTS: `mattn/go-sqlite3` is built without the
     `sqlite_fts5` tag.
   - The perceptual hash is read only by the graph resolver and by
     `GET /api/v1/edges/audit`, which shows `phashDistance` between an
     edge's endpoints. There is no "find similar" query.
6. **No editable tags or annotations.** `node_metadata` holds
   extracted values only (sources `exiftool|ffprobe|internal`).

## Phase 0: auth, audit and ingress

### 0a. MCP PAT scopes

- **New grantable scopes:**
  - `mcp:read`: every read tool.
  - `mcp:review`: review and lifecycle tools. It does **not** imply
    `mcp:read`. Minting both is recommended, but not required: a token
    holding only `mcp:review` can still call the review tools.
  - `mcp:enrich`: tag and annotation writes (phase 4).
- **An "MCP-only token"** is one whose scopes are all `mcp:*`. A
  non-admin live user may mint one. Any token with a non-`mcp:*` scope
  keeps today's rule (the owner must be a live admin). A PAT-authenticated
  caller still can only mint scopes it already holds.
- **Lookup:**
  - The lookup accepts an MCP-only token whose owner is live
    (`disabled_at IS NULL`), admin or not.
  - Any other token keeps the live-admin predicate.
  - Disabled users still fail closed.
- **`RequirePAT`** sets `LocalUserView.IsAdmin` to the owner's
  **actual** `is_admin` for MCP-only tokens. Today it hard-codes
  `true`; a non-admin MCP token must not pass `RequireAdmin` anywhere.
- **`patScopeFor`** routes `/mcp` to a middleware entry that admits
  any token holding **at least one** `mcp:*` scope (or `*`). The
  route gate does not demand `mcp:read`, because that would turn away
  an `mcp:review`-only token before its tools run. The per-tool check
  decides what each call may do. An MCP-only token presented on **any
  other path** gets a 403, because its scopes never satisfy `admin`,
  `pats:write` or `pairings:write`.
- **Scope check helper:** `auth.PATScopesFrom(ctx)` already exposes
  the token's scopes to handlers. The MCP layer uses it (via
  `scopeSatisfied`, exported as `auth.HasScope`) for per-tool checks.
  Sessions and forward-auth principals (no PAT) are not accepted on
  `/mcp` in v1.

### 0b. Audit attribution

- **Migration** `ALTER TABLE actor_audit ADD COLUMN`:
  - `auth_provider TEXT` (nullable), with
    `CHECK (auth_provider IN ('authentik','local','pat','oauth'))`.
    The values follow the existing `users.auth_provider` vocabulary.
  - `pat_id INTEGER REFERENCES user_pats(id)` (nullable)

  We chose columns over `details_json` so the audit view can filter
  on them directly.
- **Plumbing:** add `PATID int64` to `auth.PATLookupResult` and to the
  `auth.Principal`. `audit.Service.WriteActorAudit` persists both
  fields from the principal it already receives.
- **Normalisation:** forward-auth principals carry
  `Principal.AuthProvider == ""`. `principal.go` documents that empty
  means `authentik`. `WriteActorAudit` must therefore write
  `'authentik'` for a `KindUser` principal with an empty provider, never
  `''`, and NULL for machine and system actors.
- **Audit view:** `GET /api/v1/audit` returns them. The web activity
  log shows a token-name chip ("via PAT 'claude-desktop'").

### 0c. Ingress

- **Router:** add a Traefik router to `compose.yaml` at priority 150:
  - rule ``PathPrefix(`/mcp`)``
  - middlewares: `strip-identity` only (no Authentik)

  This mirrors the agent router. The PAT (and in phase 2 the OAuth
  access token) is the only credential on that path. Phase 2 extends
  the rule to the discovery path (see below).
- **Docs:** document the router in `docs/deploy.md` next to the agent
  router.
- **Config:** a new `mcp.enabled` config key, default `false`. When it
  is off, `/mcp` returns 404.

## Phase 1: MCP server v1

- **Package:** `internal/mcp`, mounted in `server.go` at `/mcp` with
  the SDK's Streamable HTTP handler.
- **Wiring:** tools call the same sqlc queries and services the Huma
  handlers in `internal/httpapi/routes.go` use, not HTTP loopback.
  Where a handler's logic is tangled with Huma types, extract a shared
  function and call it from both.

### Read tools (`mcp:read`)

| Tool | Returns | Backed by |
|---|---|---|
| `search_assets` | Paged asset list. Inputs mirror `ListAssetsInput` (`routes.go`) one-to-one: `q`, `cameraModel`, `graphStatus`, `lifecycleState`, `storageLocationId`, `unlinkedOnly`, `uploadedByUserId`, `sort`, `limit`, `offset` (offset pagination; the list has no cursor) | list query behind `GET /api/v1/assets` |
| `list_facets` | Filter values with counts | `/assets/facets` |
| `get_asset` | Detail, extracted metadata and sync status in one call | `/assets/{id}`, `/metadata`, `/sync-status` |
| `get_lineage` | `{rootId, nodes, edges}`, `depth` 1–5 | `/assets/{id}/lineage` |
| `view_asset` | MCP **image content** (the 512px JPEG thumbnail) | `internal/thumbs` cache. While the thumbnail is pending, return a text result saying "thumbnail not ready yet", not a tool error |
| `list_review_queue` | `NEEDS_REVIEW` edges with confidence, tier, resolver and evidence, plus both endpoints' names | `/edges/audit` |
| `list_jobs`, `get_scan_progress` | Jobs and scan progress | `/jobs`, `/progress` |
| `storage_health` | Storage and agent health | `/storage-health`, `/storage-health/agents` |

### Review tools (`mcp:review`)

- **Tools:**
  - `confirm_edge`, `reject_edge`, `reset_edge_review`
  - `create_edge` (manual edge, same validation as `POST /api/v1/edges`)
  - `trash_asset`, `restore_asset`
  - `retry_sync`
- **Explicit ids only.** Each takes ids (or UUIDs) as arguments. A
  tool never acts on ids "found" in free text.
- **Audit.** Each writes the same `actor_audit` event as its REST
  counterpart, so phase 0b's attribution applies automatically.

### Assistant-confirmed edges

An LLM confirming an edge is a weaker signal than a human doing it.
Changing `review_state` would need a table rebuild, because its CHECK
constraint is fixed in `00001_init.sql`. So instead:

- **Schema:** a migration adds `media_edges.review_source TEXT`,
  nullable, `CHECK (review_source IN ('human','assistant'))`.
  `v_media_edges_resolved` is recreated to expose it.
- **Who sets it:** confirm, reject and manual-create set
  `review_source = 'assistant'` when called through MCP and `'human'`
  otherwise. Reset clears it along with `reviewed_at`.
- **Downstream:** `CONFIRMED` keeps its meaning for every existing
  consumer (status derivation, inheritance).
- **UI:** the review queue gains an "assistant-reviewed" filter so a
  human can spot-check, and the edge shows an "AI" marker in the web
  UI.

### Prompts

The server exposes MCP prompts for the common workflows:

- `triage-review-queue`: walk `NEEDS_REVIEW` edges, view both ends,
  propose confirm or reject with reasons, and act only on the user's
  go-ahead.
- `explain-asset`: lineage plus metadata narrative for one asset.

### Untrusted content

File names, EXIF/ffprobe values, XMP ids and `evidence_json` are
attacker-controllable text (anyone who can put a file on the NAS).
To limit prompt injection:

- **Tool descriptions** state that this content is data, never
  instructions.
- **Results** return it in structured fields, not merged into prose.
- **Mutating tools** require explicit ids.

## Phase 2: OAuth remote connector

- **Resource server:** `/mcp` becomes an OAuth 2.1 protected resource
  per the MCP authorization spec:
  - protected-resource metadata (RFC 9728) at
    `/.well-known/oauth-protected-resource`
  - `WWW-Authenticate` with `resource_metadata` on 401
  - audience-bound access tokens, validated as JWTs against the
    issuer's JWKS
- **Authorization server:** Authentik's OAuth2/OIDC provider, which is
  already deployed.
  - Authentik groups map to `mcp:read`, `mcp:review` and `mcp:enrich`.
  - Token subjects map to `users` rows through the existing
    forward-auth identity, i.e. the same user, not a new one.
  - A built-in authorization server is a fallback, only if Authentik
    can't satisfy what claude.ai custom connectors need (client
    registration in particular).
- **Re-check at implementation time:** the current MCP authorization
  spec revision and the claude.ai connector requirements. Both move.
- **Ingress:** the `/mcp` router stays outside Authentik ForwardAuth.
  It accepts a PAT or an OAuth access token.
  - **Discovery path:** its rule must also cover the discovery path,
    i.e.
    ``PathPrefix(`/mcp`) || PathPrefix(`/.well-known/oauth-protected-resource`)``.
    Otherwise the browser catch-all router (priority 10,
    `authentik@file`) sends the client's first, credential-less
    discovery request to a login redirect.
  - **Public exposure** follows the operator's existing pattern for
    remote connectors.

## Phase 3: search

**Full-text search**

- **Build:** add `-tags sqlite_fts5` to every Go build, test and lint
  invocation (Dockerfile, Makefile, CI). The tag is declared in
  `go-sqlite3`'s `sqlite3_opt_fts5.go` as `sqlite_fts5 || fts5`.
- **Index:** an FTS5 table `media_nodes_fts`, contentless or
  external-content, over:
  - `file_name`
  - relative path
  - a curated set of `node_metadata` keys: camera make/model, lens,
    capture date, GPS place fields if present, and project/timeline
    names
  - phase 4's tags and annotations
- **Sync:** triggers keep it current, and the goose migration
  backfills it.
- **Query routing:** `q` on `GET /api/v1/assets` uses FTS when the
  table exists, so the web UI gains it too. `search_assets` uses the
  same path.
- **Exact-UUID lookup must keep working.** A `q` that equals a node's
  `node_uuid` must still return that node. Keep the `node_uuid = q`
  branch alongside the FTS match (OR it in, or check it first), so
  pasting a UUID doesn't silently stop working.

**Find similar**

- **API:** `GET /api/v1/assets/{id}/similar` and the
  `find_similar` tool.
- **Ranking:** by perceptual-hash Hamming distance (64-bit DCT hash,
  `hashing.PerceptualHash`), using the same distance computation the
  spatial-temporal resolver uses.
- **Implementation:** a brute-force scan in Go is fine up to roughly
  1M nodes. Bucketing by hash prefix is a later optimisation.

## Phase 4: assistant enrichment

- **Schema:** two new tables, separate from extracted `node_metadata`:
  - `asset_tags(node_id, tag, source, created_by_user_id, pat_id, created_at)`
  - `asset_annotations(node_id, kind, text, source, model, created_by_user_id, pat_id, created_at)`

  `source IN ('user','assistant')`, so assistant-written data can
  always be filtered out or bulk-reverted.
- **API:** Huma routes to list, add and remove tags and annotations.
  The web asset detail shows them with an "AI" badge for
  `source = 'assistant'`.
- **MCP tools:**
  - `tag_assets` and `set_description` (scope `mcp:enrich`)
  - `list_tags` (scope `mcp:read`)
- **Search:** both feed `media_nodes_fts`.

## Phase ordering

| Phase | Depends on |
|---|---|
| 0a scopes | none |
| 0b audit | none |
| 0c ingress | none |
| 1 MCP v1 | 0a, 0b, 0c |
| 2 OAuth | 1 |
| 3 FTS / similar | none for the REST and web side; the MCP tool lands after 1 |
| 4 enrichment | 1, 3 |
| Workstation (`branchdam-agent mcp`) | none (separate repo) |
