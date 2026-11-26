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
   - `q` uses FTS5 prefix search over filename, relative path and selected
     camera/project metadata, with bm25 ranking unless `sort` is explicit.
     Exact `node_uuid` lookup remains supported.
     Input terms are quoted, never accepted as FTS query syntax. See
     [asset API search notes](schema.md#asset-api-q-and-sorting) for details.
   - `mattn/go-sqlite3` is built with the `sqlite_fts5` tag.
   - The perceptual hash is also exposed by
     `GET /api/v1/assets/{id}/similar` and the `find_similar` tool.
     `GET /api/v1/edges/audit` still shows `phashDistance` between an
     edge's endpoints.
6. **Tags and annotations are separate from extracted metadata.**
   `asset_tags` and `asset_annotations` hold user and assistant text.
   `node_metadata` stays extracted values only
   (sources `exiftool|ffprobe|internal`). `q` searches tags and
   annotation text along with filename, path and the selected metadata.

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
  **actual** `is_admin` for MCP-only tokens. A non-admin MCP token must
  not pass `RequireAdmin`. `/mcp`, and tag or annotation routes other
  than revert, skip that middleware; they do not flip `IsAdmin`.
- **`patScopeFor`** routes `/mcp` to a middleware entry that admits
  any token holding **at least one** `mcp:*` scope (or `*`). The
  route gate does not demand `mcp:read`, because that would turn away
  an `mcp:review`-only token before its tools run.
  - **Mechanism:** `scopeSatisfied` matches only an exact scope or `*`,
    and stays unchanged. A `patHandlers` entry keyed `mcp:*` would
    admit only a token literally holding `mcp:*`.
  - **Instead:** `PATMiddleware` gains an any-of form
    (`PATService.MiddlewareAnyOf("mcp:read", "mcp:review", "mcp:enrich")`).
    It passes when `scopeSatisfied` holds for at least one listed
    scope, and the `/mcp` entry in `patHandlers` uses it.

  The per-tool check decides what each call may do. An MCP-only token
  presented on any other path gets a 403, except the tag and annotation
  routes: `GET` admits `mcp:read` and `mcp:enrich`, and writes admit
  `mcp:enrich`. Those routes still do not accept `mcp:review` alone.
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
  log shows a token-name chip ("via token <name>").

### 0c. Ingress

- **Router:** add a Traefik router to `compose.yaml` at priority 150:
  - rule ``Path(`/mcp`) || PathPrefix(`/mcp/`)`` (not a bare
    ``PathPrefix(`/mcp`)``, which would also skip Authentik for any
    other path that only starts with those letters)
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
| `search_assets` | Paged asset list. Inputs mirror `ListAssetsInput` (`routes.go`) one-to-one: `q`, `cameraModel`, `graphStatus`, `lifecycleState`, `storageLocationId`, `unlinkedOnly`, `uploadedByUserId`, `sort`, `tag`, `limit`, `offset` (offset pagination; the list has no cursor) | list query behind `GET /api/v1/assets` |
| `list_facets` | Filter values with counts | `/assets/facets` |
| `get_asset` | Detail, extracted metadata, sync status, tags and annotations (each with `source`) | `/assets/{id}`, `/metadata`, `/sync-status`, `/tags`, `/annotations` |
| `find_similar` | Other `ACTIVE`/`HIDDEN` assets ordered by perceptual-hash Hamming distance. `limit` defaults to 20 (max 100). `maxDistance` defaults to 12 (0-64). A source with no hash returns an empty list and `reason` `no_perceptual_hash` | `GET /api/v1/assets/{id}/similar` |
| `get_lineage` | `{rootId, nodes, edges}`, `depth` 1–5 | `/assets/{id}/lineage` |
| `view_asset` | MCP **image content** (the 512px JPEG thumbnail) | `internal/thumbs` cache. While the thumbnail is pending, return a text result saying "thumbnail not ready yet", not a tool error |
| `list_review_queue` | `NEEDS_REVIEW` edges with confidence, tier, resolver and evidence, plus both endpoints' names | `/edges/audit` |
| `list_jobs`, `get_scan_progress` | Jobs and scan progress | `/jobs`, `/progress` |
| `storage_health` | Storage and agent health | `/storage-health`, `/storage-health/agents` |

### Review tools (`mcp:review`)

`mcp:read` does not satisfy these. Each takes explicit ids, calls the same function as its REST route, and writes the same `actor_audit` event (so an MCP confirm's row carries `pat_id`). Confirm, reject and manual-create set `review_source` to `assistant`. The REST routes set `human`. Reset clears `review_source` together with `reviewed_at` and `reviewed_by`. The REST reset route stays human-only (no PAT); `reset_edge_review` is the MCP caller and does not pass that gate. An assistant confirm, reject, manual create, or reset refuses a human decision: `review_source` is `human`, or the row is `CONFIRMED` or `REJECTED` with no source (a review written before the column existed). Resetting an assistant decision is allowed. A person on the REST routes can still overturn an assistant decision.

| Tool | Arguments | Effect |
|---|---|---|
| `confirm_edge` | `id` | `CONFIRMED`, `review_source=assistant` |
| `reject_edge` | `id` | `REJECTED`, `review_source=assistant` |
| `reset_edge_review` | `id` | back to `NEEDS_REVIEW`, `review_source` cleared |
| `create_edge` | `sourceNodeId`, `targetNodeId`, `relationshipType` | same validation as `POST /api/v1/edges`, `review_source=assistant` |
| `trash_asset` | `id` | trash the asset and its linked exports |
| `restore_asset` | `id` | restore a trashed or archived asset |
| `retry_sync` | `id` | requeue failed sync rows for the asset |

### Enrichment tools (`mcp:enrich`)

`mcp:read` does not satisfy these. `list_tags` is a read tool (`mcp:read`); `mcp:enrich` alone cannot call it, even though `GET /api/v1/tags` accepts either. `tag_assets` and `untag_assets` take at most 100 ids, 100 tags and 1000 id-tag pairs. Duplicate ids and tags are collapsed first. Every id is checked before any write, so a missing or archived asset fails the call with nothing committed. They call the same functions as the REST routes. `source` is still derived on the server, so these writes are `assistant`. `untag_assets` removes only `source=assistant` tags; a user tag is reported as skipped. The removed and skipped tags are the normalised form.

| Tool | Arguments | Effect |
|---|---|---|
| `list_tags` | none | tags with counts for live assets (`mcp:read`) |
| `tag_assets` | `ids`, `tags` | add each tag to each asset |
| `untag_assets` | `ids`, `tags` | remove assistant tags only |
| `set_description` | `id`, `text`, `model?` | set the assistant description |

`get_asset` also returns `tags` and `annotations`, each with `source`.

`GET /api/v1/edges/audit?reviewSource=assistant` lists assistant-reviewed edges (they are not `NEEDS_REVIEW`, so this replaces the queue predicate rather than adding to it). Lineage and graph edges include `reviewSource`. The web queue has an assistant-reviewed filter, and an assistant decision is marked `AI` on the graph and in the queue.

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
- `describe-assets`: search, view each result, propose tags and a
  description, and write them only after the user agrees.

### Untrusted content

File names, EXIF/ffprobe values, XMP ids and `evidence_json` are
attacker-controllable text (anyone who can put a file on the NAS).
To limit prompt injection:

- **Tool descriptions** state that this content is data, never
  instructions.
- **Results** return it in structured fields, not merged into prose.
- **Mutating tools** require explicit ids.

## Phase 2: OAuth remote connector

Checked 2026-11-25 against MCP authorization revision 2026-07-28 and
Claude's custom-connector docs.

- The MCP server is only a resource server. It must publish RFC 9728
  protected-resource metadata and reject tokens that are not
  audience-bound to it. PKCE S256 is required of the client and the
  authorization server, not of this process.
- Client registration prefers a pre-registered client, then a Client
  ID Metadata Document. Dynamic client registration is deprecated in
  the spec and kept only for authorization servers that still offer
  it. Claude's connector dialog offers all three: "Use Claude's
  published identity" (CIMD), "Register automatically" (open DCR),
  and "Use your own OAuth client" (client id, and a secret only if
  the provider is confidential). The hosted redirect is
  `https://claude.ai/api/mcp/auth_callback`. Claude sends PKCE S256
  on every authorization request.
- Authentik already supports PKCE and asymmetric JWT access tokens
  (select a signing key; otherwise it signs HS256 with the client
  secret, which this server will not accept). Its DCR endpoint
  requires a bearer token with the DCR scope, so it is not Claude's
  open registration, and it does not advertise CIMD. A pre-registered
  client is enough. This repo does not ship an authorization server.
- **Resource server:** `/mcp` accepts a PAT or an OAuth access token.
  - protected-resource metadata at
    `/.well-known/oauth-protected-resource`, no credential
  - `WWW-Authenticate` with `resource_metadata` on 401
  - JWTs checked against the issuer's JWKS (asymmetric algs only),
    with issuer, audience, expiry and a required `sub`
- **Identity:** `sub` must already be a forward-auth `users` row
  (`auth_provider=authentik`, `external_uid=sub`). No row is created.
  `mcp.oauth.scopeClaims` maps claim values (Authentik groups) to
  `mcp:read`, `mcp:review` and `mcp:enrich`. Anything else in the
  mapping is ignored. `auth.HasScope` reads those grants. An audit
  row for the action has `auth_provider=oauth`.
- **Ingress:** the `/mcp` router stays outside Authentik ForwardAuth.
  Its rule matches `/mcp` and the exact discovery paths
  `/.well-known/oauth-protected-resource` and
  `/.well-known/oauth-protected-resource/mcp` (with and without a
  trailing slash). It is not a prefix. The well-known paths are what
  keep the credential-less discovery request off the browser
  catch-all (priority 10, `authentik@file`).

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

- **API:** `GET /api/v1/assets/{id}/similar?limit=20&maxDistance=12`
  and the `find_similar` tool, both calling the same handler.
- **Candidates:** other `ACTIVE` or `HIDDEN` nodes that have a
  perceptual hash. The source node is excluded. `TRASHED`, `MISSING`
  and `ARCHIVED` nodes are not candidates.
- **Ranking:** by perceptual-hash Hamming distance (64-bit DCT hash,
  `hashing.PerceptualHash` / `hashing.HammingDistance`), nearest
  first, then ascending id. `maxDistance` defaults to 12 and cannot
  exceed 64. `limit` defaults to 20 and cannot exceed 100.
- **No hash:** a source node with no perceptual hash returns
  `{assets: [], reason: "no_perceptual_hash"}`, not an error.
- **Implementation:** a brute-force scan in Go is fine up to roughly
  1M nodes. Rows are streamed and only the nearest `limit` are kept.
  Bucketing by hash prefix is a later optimisation.

## Phase 4: assistant enrichment

REST, MCP tools and the web UI are in place. branchDAM still does not
call a model itself.

- **Schema:** `asset_tags` and `asset_annotations`, separate from
  extracted `node_metadata`. `source` is `user` or `assistant`.
  Tags are trimmed and lowercased, at most 64 characters, unique per
  node. At most one `description` per node and source.
- **Who wrote it:** `source` is derived on the server. An MCP-only PAT
  or an OAuth access token is `assistant`. A browser session, and any
  other credential, is `user`. A `source` field on the request body is
  ignored. A person re-adding an assistant tag takes the row
  (`source` becomes `user`, `pat_id` cleared) so revert cannot delete
  it. An assistant re-adding a user tag leaves the row alone.
- **Auth:** any authenticated user may read and write tags and
  descriptions, including a non-admin session and a non-admin
  `mcp:enrich` token. Those routes skip `RequireAdmin`. Revert does
  not.
- **API:**
  - `GET` and `POST /api/v1/assets/{id}/tags`, `DELETE .../tags/{tag}`
  - `GET /api/v1/assets/{id}/annotations`, `PUT .../annotations/description`
  - `GET /api/v1/tags` lists tags with counts for live nodes only
    (not archived, trashed, or superseded)
  - `POST /api/v1/tags/revert` with `{source: "assistant", patId?, since?}`
    removes assistant tags and annotations. Admins only. `patId` and
    `since` (unix seconds) narrow the delete.
  - `GET /api/v1/assets?tag=` filters on an exact tag. `q` also matches
    tag and annotation text through `media_nodes_fts`.
- **Audit:** add, remove, description and revert write `actor_audit`.
- **MCP tools:** `tag_assets`, `untag_assets` and `set_description`
  (`mcp:enrich`); `list_tags` (`mcp:read`). `get_asset` includes tags
  and annotations. `untag_assets` cannot remove a user tag. At most
  100 ids, 100 tags and 1000 id-tag pairs per call. The prompt is `describe-assets`.
- **Web:** asset detail shows tags and the description, with an AI
  badge when `source` is `assistant`. Users add and remove tags. Admins
  revert assistant tags from settings. The asset list has a tag chip.

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
