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
exports and Immich on a separate server — see [`deploy-topology.md`](deploy-topology.md), which
covers a specific worked topology and cross-references
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
  [`workflow-coverage.md` §6](workflow-coverage.md#6-immich-integration).

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
      - /your/real/scratch:/storage/scratch:rw
      - /your/real/exports:/storage/exports:rw
      - /your/real/projects:/storage/projects:rw
      - /your/real/archive:/storage/archive:ro   # Tier 3 — always :ro
```

**No `image:` override.** This deployment deliberately stays on `ghcr.io/s3ntin3l8/branchdam:latest`
so a `docker compose pull` picks up every release automatically — see
[`operations.md`](operations.md#upgrades) for what that trades off, and for why you back up
*before* every pull, not after. `compose.yaml`'s own header comment suggests pinning a tag; that
advice is aimed at a production deploy, and is being knowingly declined here.

Only mount the storage tiers you actually have — drop volumes lines (and the corresponding
`storageLocations` entries in step 4) for tiers that don't apply yet. Tier 3 must always be
mounted `:ro`; `storage.Guard` also refuses to write to it at the application level regardless of
the mount, but the mount is the first line of defense.

## 4. `config.yaml`

Copy `config.example.yaml` to `config.yaml` next to `compose.override.yaml` and edit it — every
field is explained in [`configuration.md`](configuration.md). The one thing worth getting right
before anything else: `storageLocations[].rootPath` is the **container** path, and must match the
right-hand side of the volume mount you just wrote, not the host path on the left.

| Tier | Container path (config.yaml `rootPath`) | Host path (compose `volumes:` left side) | Mount mode |
|---|---|---|---|
| `TIER0_LOCAL_STAGING` | `/storage/staging` | your staging dir | `rw` |
| `TIER1_LOCAL_SCRATCH` | `/storage/scratch` | your scratch dir | `rw` |
| `TIER2_EXPORTS` | `/storage/exports` | your exports dir | `rw` |
| `PROJECTS` | `/storage/projects` | your projects dir | `rw` |
| `TIER3_MASTER_ARCHIVE` | `/storage/archive` | your archive dir | **`ro`**, and `readOnly: true` in config |

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

curl -s -o /dev/null -w '%{http_code}\n' https://dam.yourdomain.example/api/v1/agent/hello
# → 401 (no key presented)

curl -s -H "X-API-Key: $BRANCHDAM_AGENT_API_KEY" https://dam.yourdomain.example/api/v1/agent/hello | jq
# → {"ok":true,"version":"..."}
```

If `/api/v1/me` returns an empty `name`, or a write that should work returns `403 authentication
required`, see [`forward-auth.md` §4](forward-auth.md#4-verifying-it-works) — almost always
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

## 9. Done when

- [ ] `docker compose ps` shows `(healthy)`.
- [ ] `/healthz` and `/api/v1/me` both return correctly through Traefik.
- [ ] The agent path returns `401` without a key and `200` with one.
- [ ] A scan against the fixture directory completes and produces assets you can see in the SPA.
- [ ] Every configured storage location shows active on the Storage Health page.

From here, [`operations.md`](operations.md) covers what changes once you're running for real:
upgrades, backups, pruning, and a troubleshooting table.
