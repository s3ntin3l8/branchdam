# First deploy

This is the bring-up runbook for a first branchDAM deployment: Traefik + Authentik in front,
`docker compose` running the published image behind it. It assumes you already have a working
Traefik v3 (with the **file** provider enabled) and Authentik instance — setting those up from
scratch is outside branchDAM's scope. For the deep detail on *why* each piece of the auth setup
looks the way it does, see [`forward-auth.md`](forward-auth.md); this document is the sequence of
steps to actually stand the container up, with pointers into that doc rather than duplicating it.
For field-by-field config reference, see [`configuration.md`](configuration.md). For what to do
once it's running — upgrades, backups, troubleshooting — see [`operations.md`](operations.md). For
a multi-machine setup — ingest and editing on separate workstations, a NAS-hosted master archive,
exports and Immich on a separate server — see §9 below, which cross-references
[`workflow-coverage.md`](workflow-coverage.md) for what that workflow does and does not support
today. This document stays the portable runbook for a plain `docker compose` deployment; if your
deployment target is managed by its own infrastructure-as-code (Ansible, Terraform, etc.) instead,
treat this as the contract that tooling needs to satisfy rather than a sequence to run by hand.

## 0. Prerequisites

- Traefik v3, with the `file` provider enabled (the two middlewares below are defined there, not
  via Docker labels — they apply repo-wide, not just to this one container). If your Traefik
  instance already centrally defines an equivalent ForwardAuth + identity-header-strip middleware
  chain, reference that instead of redefining the two below — the ordering (strip identity
  headers, then ForwardAuth) is what matters, not that this exact repo defines them.
- Authentik, reachable from Traefik, with an embedded or standalone outpost.
- An external Docker network named `proxy` that both Traefik and branchDAM's container join —
  `compose.yaml` declares it `external: true` rather than creating it, so `docker network create
  proxy` first if it doesn't already exist.
- If branchDAM will trigger an Immich library scan, that library must be an Immich **external**
  library pointed at branchDAM's export path — an existing Immich-managed (internally-ingested)
  library is not reachable by this integration at all; see
  [`integrations.md` §4](integrations.md#4-immich-external-library-push).

## 1. Authentik: proxy provider, application, group

Create a **Provider** (Proxy Provider, "Forward auth (single application)" mode) pointed at
branchDAM's external URL, an **Application** bound to it, and assign the application to whichever
group(s) should be allowed to reach branchDAM at all — this is Authentik's own gate, upstream of
anything branchDAM itself checks. Full walkthrough: [`forward-auth.md` §1](forward-auth.md#1-authentik-proxy-provider--outpost).

## 2. Traefik: file middlewares

Define `strip-identity` and `authentik` in your dynamic config (`@file` provider). The exact
YAML is in [`forward-auth.md` §2](forward-auth.md#2-traefik-the-three-router-split) — copy it
as-is. The one detail worth restating here because it's easy to get backwards: `strip-identity`
must be attached to **both** of branchDAM's routers, not just the browser one. The agent router
bypasses Authentik by design, so nothing upstream of branchDAM would otherwise strip a
client-forged `X-Authentik-Username` header on that path.

## 3. `compose.override.yaml`

`compose.yaml` is committed and generic (placeholder `dam.example.com` host, placeholder
`/mnt/nas/*` mounts). Don't edit it — create a gitignored `compose.override.yaml` next to it;
`docker compose` merges the two automatically. Override exactly three things:

```yaml
services:
  branchdam:
    labels:
      traefik.http.routers.branchdam-agent.rule: "Host(`dam.yourdomain.example`) && PathPrefix(`/api/v1/agent`)"
      traefik.http.routers.branchdam.rule: "Host(`dam.yourdomain.example`)"
      traefik.http.routers.branchdam-outpost.rule: "Host(`dam.yourdomain.example`) && PathPrefix(`/outpost.goauthentik.io/`)"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - /your/real/staging:/storage/staging:rw
      - /your/real/exports:/storage/exports:rw
      # TIER1_LOCAL_SCRATCH and PROJECTS are typically workstation-local and
      # not mounted into the headless server. Uncomment the mount AND the
      # matching storageLocations entry in config.yaml only if your
      # deployment needs server-side access.
      # - /your/real/scratch:/storage/scratch:rw
      # - /your/real/projects:/storage/projects:rw
      - /your/real/archive:/storage/archive:rw   # Tier 3 — :rw for server-governed ingest
```

**No `image:` override.** This deployment deliberately stays on `ghcr.io/s3ntin3l8/branchdam:latest`
so a `docker compose pull` picks up every release automatically — see
[`operations.md`](operations.md#upgrades) for what that trades off, and for why you back up
*before* every pull, not after. `compose.yaml`'s own header comment suggests pinning a tag; that
advice is aimed at a production deploy, and is being knowingly declined here.

Only mount the storage tiers you actually have — drop volumes lines (and the corresponding
`storageLocations` entries in step 4) for tiers that don't apply yet. Tier 3 is mounted `:rw` for server-governed ingest (uploads, mobile uploads, agent uploads → archive); `readOnly: false` is the default in `config.yaml`. Set `readOnly: true` and use `:ro` only for archive-only deployments.

## 4. `config.yaml`

Copy `config.example.yaml` to `config.yaml` next to `compose.override.yaml` and edit it — every
field is explained in [`configuration.md`](configuration.md). The one thing worth getting right
before anything else: `storageLocations[].rootPath` is the **container** path, and must match the
right-hand side of the volume mount you just wrote, not the host path on the left.

| Tier | Container path (config.yaml `rootPath`) | Host path (compose `volumes:` left side) | Mount mode |
|---|---|---|---|
| `TIER0_LOCAL_STAGING` | `/storage/staging` | *(none — virtual namespace)* | Virtual (no volume mount needed) |
| `TIER2_EXPORTS` | `/storage/exports` | your exports dir | `rw` |
| `TIER3_MASTER_ARCHIVE` | `/storage/archive` | your archive dir | **`rw`** for server-governed ingest (`readOnly: false` in config; set `readOnly: true` and use `:ro` only for archive-only deployments) |
| `TIER1_LOCAL_SCRATCH` | `/storage/scratch` | your scratch dir | `rw` — optional, workstation-local in most topologies |
| `PROJECTS` | `/storage/projects` | your projects dir | `rw` — optional, rarely needed |

`database.path` must stay an **absolute** container path (`/data/branchdam.db` is the default and
is fine as-is) — `storage.Guard`'s `canonicalize` rejects a relative root outright, and the
failure mode for a storage location with a bad path is silent (that location is skipped and
marked inactive at startup, not a fatal error), so a typo here is easy to miss without checking
the **Storage Health** page after first boot.

## 5. `.env`

```sh
cp .env.example .env
```

Generate the agent key:

```sh
openssl rand -hex 32
```

and set `BRANCHDAM_AGENT_API_KEY` to the result. Under 32 characters and every `/api/v1/agent/*`
route fails closed with `503` (logged once at startup) — not silently open, but also not what you
want if you're trying to test the agent handshake/rebase endpoints.

Generate the secret encryption key (for UI-configured secrets such as Immich API keys):

```sh
openssl rand -base64 32
```

and set `BRANCHDAM_SECRET_KEY` to the result. If left unset, UI-configured secrets are unavailable
until set, but the server starts normally. If set, it must be valid base64-encoded 32 bytes or the
server refuses to start.

If using the Immich external library integration directly via environment variables, set
`IMMICH_API_URL`, `IMMICH_API_KEY`, and `IMMICH_LIBRARY_ID` as well. Note that `IMMICH_API_KEY` set in
`.env`/`config.yaml` serves as the initial base value, which can later be overridden from the Settings
UI (encrypted with `BRANCHDAM_SECRET_KEY` into `app_settings`; see [`configuration.md`](configuration.md)'s
precedence section).

## 6. `authz.groups`

In `config.yaml`, set `authz.groups` to the exact Authentik group name from step 1 (case-sensitive,
no typos — there's no validation that it matches anything real in Authentik). Leaving it empty is
the solo-homelab default: every authenticated user gets write access, and startup logs a WARN
naming the key. That's a legitimate choice for a first test deploy with one operator; just make it
on purpose.

## 7. Bring-up

```sh
docker compose config          # proves compose.override.yaml actually merged — check the Host()
                                # rules and volume list before going further
docker compose pull
docker compose images branchdam   # record the resolved digest; see operations.md
docker compose up -d
docker compose ps              # wait for STATUS to read (healthy), not just Up
```

Through Traefik, from outside the container:

```sh
curl -s https://dam.yourdomain.example/healthz
# → ok

curl -s https://dam.yourdomain.example/api/v1/me | jq
# → {"kind":"user","name":"you","email":"...","groups":[...]}

curl -s -o /dev/null -w '%{http_code}\n' -X POST https://dam.yourdomain.example/api/v1/agent/hello
# → 401 (no key presented)

curl -s -X POST -H "X-API-Key: $BRANCHDAM_AGENT_API_KEY" https://dam.yourdomain.example/api/v1/agent/hello | jq
# → {"ok":true,"version":"..."}
```

`hello` is registered `POST`-only (`internal/httpapi/routes.go`); `-X POST` above is required, not
optional. Auth runs ahead of routing, so the no-key check above is correct with or without it. But
a bare (`GET`) `curl` against this path is *not* an error at all -- the SPA's catch-all route
(`GET /`) absorbs it and returns `200` with the HTML shell, not `405` and not JSON. Forgetting
`-X POST` here looks like a pass (`200`) even though it never reached the agent handler.

If `/api/v1/me` returns an empty `name`, or a write that should work returns `403 authentication
required`, see [`forward-auth.md` §5](forward-auth.md#5-verifying-it-works) — almost always
`authResponseHeaders` on the Authentik middleware not listing a header Traefik would otherwise
strip.

## 8. First scan

Point one storage location at a **small fixture directory** first, not your real archive — a
handful of files including one RAW and its exported JPEG is enough to prove the whole pipeline:

```sh
curl -s -X POST https://dam.yourdomain.example/api/v1/scan \
  -H 'content-type: application/json' \
  -d '{"storageLocationId": 1}'
```

Then in the SPA: watch **Ingest Jobs** move to completion live (this exercises the SSE nudge
path), check **Assets** for the indexed files, and check **Audit Queue** for any Tier-2 lineage
candidate between the RAW and its JPEG. Check **Storage Health** shows every configured location
active — an inactive one means `storage.LoadGuard` couldn't resolve that mount at startup, usually
a path mismatch between `config.yaml` and the compose volume.

## 9. Multi-machine topology

Setup: ingest and editing on a Windows workstation and a MacBook; a Tier-3 master archive on a
NAS; Tier-2 exports and Immich on a separate Docker host on the same LAN.

### 9.1. Host decision: server and NAS are separate hosts

Run branchDAM on the same host as the exports directory and Immich, with the Tier-3 archive
mounted over NFS from the NAS, rather than running branchDAM on the NAS itself.

Exports, Immich, and `branchdam.db` all want to be local to one host — the archive is the only
thing that would argue for putting branchDAM on the NAS instead, and that argument is weak:

- A full archive scan re-reads every byte regardless of which host runs branchDAM, since
  `full_hash` is forced for every `TIER3_MASTER_ARCHIVE` node under the default
  `fullHashPolicy` (`tier3_and_collision` — keys on the tier, not the `readOnly` flag, so it
  holds whether the location is mounted `:rw` for server-governed ingest or `:ro` for
  archive-only deployments).
- Thumbnail generation reads every node with a `PENDING` thumbnail state with no tier filter, so
  every master gets read once for its thumbnail regardless of where branchDAM runs.

Both are one-time-per-node costs, not recurring ones, and they become close to zero once an
ingest client posts `EVENT_NODE_CREATED` directly instead of relying on a full rescan. Until
that exists, size the first full scan against the actual archive size before running it — for a
multi-terabyte archive over NFS this is a multi-hour operation, not a routine one.

### 9.2. Mount table

| Tier | Purpose | Mount |
|---|---|---|
| `TIER3_MASTER_ARCHIVE` | Camera originals | NAS, over NFS, mounted `:rw` in the compose layer and `readOnly: false` in `config.yaml` for server-governed ingest; set `:ro` / `readOnly: true` only for archive-only deployments |
| `TIER2_EXPORTS` | Renders/exports, shared with Immich | Local disk on the server host, read-write |
| `TIER0_LOCAL_STAGING` | Workstation ingest staging namespace | Virtual namespace (`/storage/staging`, `virtual: true`) — no host mount required; satisfies `storage.Guard.Resolve` for agent offline queue drain; actual bytes remain on workstation NVMe until synced |
| `TIER1_LOCAL_SCRATCH` | Workstation editing cache | Not mounted into the server at all — see §9.5 |
| `PROJECTS` | — | Not configured — project files are workstation-local |

### 9.3. Constraints

- **The archive must resolve as a real, symlink-free mount target inside the container.** The
  scanner writes `file_path` in the config-declared form; `storage.Guard` resolves the same root
  through `filepath.EvalSymlinks`. A symlink anywhere in the mounted root means scanner-written
  paths and any agent-supplied paths for the same location stop matching on exact-string lookup.
- **`branchdam.db` must sit on real local storage, not a NAS-backed user share.** A SQLite
  database in WAL mode over shfs/FUSE-style network filesystems is not a combination to discover
  is broken in production — keep the database volume local to the server host regardless of where
  the archive itself lives.
- **If the server host is a container/LXC without direct NFS client support, mount the archive on
  the outer host and bind it into the container** rather than mounting NFS from inside an
  unprivileged container.
- **One branchDAM process per database file, ever.** There is no file lock or PID guard; a second
  process pointed at the same `database.path` marks the first process's in-flight scans `FAILED`.
  This includes any second `-config` invocation for local debugging.
- **Do not set `fullHashPolicy: never` to speed up the first archive scan.** It permanently
  disables prune eligibility for every node it touches — `full_hash` is required, non-NULL, and
  64 hex characters for a node to ever be treated as a verified Tier-3 ancestor, and a node
  scanned under `never` does not self-repair on a later scan under a different policy without a
  further rescan.
- **Back up the database volume before every image update.** The image tracks a moving tag; goose
  migrations run automatically at container start with no reverse-migration path wired in. The
  database is the only place lineage history exists — there is no way to reconstruct it from the
  filesystem alone.

### 9.4. Deployment mechanism

This kind of multi-host topology is typically deployed and updated through Ansible playbooks (or
equivalent infrastructure-as-code), not a manually-run `docker compose up -d`. The mount table,
tier layout, and constraints above are still the contract the compose file has to satisfy; how
that compose file gets rendered and applied is out of scope for this repo. For a plain Docker
Compose deployment, the runbook in §1–§8 above applies directly.

### 9.5. Local editing tiers: scratch vs. staging visibility

`TIER1_LOCAL_SCRATCH` is not mounted into the server at all in this topology — it is
workstation-local NVMe, and mounting it over the network to make it server-visible would trade
away the local editing performance this topology is built to preserve.

In this architecture, ephemeral render caches (e.g. DaVinci Resolve `CacheClip/`) and proxy
media are managed client-side by `branchdam-agent`:
- The agent discovers local render caches and queries `POST /api/v1/agent/node-status` on the server
  to confirm all referenced source masters are live and hash-verified on `TIER3_MASTER_ARCHIVE`.
- Stale render caches are automatically evicted based on inactivity TTL and scratch disk watermarks,
  leaving camera originals on `LocalEditRoot` protected from automated deletion.
- Workstation scratch storage usage and reclaimed bytes are reported back to the server via
  `POST /api/v1/agent/telemetry`.
- Server-side cache pruning (`POST /api/v1/prune`, `pruning.enabled`, `cacheTtlHours`) remains
  available for any server-visible scratch locations.

`TIER0_LOCAL_STAGING`, by contrast, is configured in the server's `config.yaml` as a virtual
storage location (`/storage/staging`, `virtual: true`) purely so `storage.Guard.Resolve`
recognizes paths submitted during the agent's offline queue drain (`/storage/staging/<agentId>/...`)
before files are synced and rebased to the Tier-3 archive. No host directory creation or volume mount
is required — `storage.Guard` resolves virtual namespaces lexically and immediately tracks node metadata.

### 9.6. Reaching this server when a workstation isn't on the LAN

Everything above assumes the ingest workstation is on the same LAN as the server host. For a
travelling workstation doing field ingest away from home, see
[`forward-auth.md` §4](forward-auth.md#4-reaching-the-agent-route-off-lan): an overlay network
(Tailscale or equivalent) is the assumed transport, it has to land on the same Traefik hostname
and router rather than exposing the container port directly, and `/api/v1/agent/*`'s
`X-API-Key` auth model is unchanged either way — the security boundary is the key, not the
network.

## 10. Done when

- [ ] `docker compose ps` shows `(healthy)`.
- [ ] `/healthz` and `/api/v1/me` both return correctly through Traefik.
- [ ] The agent path returns `401` without a key and `200` with one.
- [ ] A scan against the fixture directory completes and produces assets you can see in the SPA.
- [ ] Every configured storage location shows active on the Storage Health page.

From here, [`operations.md`](operations.md) covers what changes once you're running for real:
upgrades, backups, pruning, and a troubleshooting table.
