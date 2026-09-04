# Deploy Topology: Multi-Machine Setup

This is a worked example of a specific multi-machine deployment, not a general guide — see
[`deploy.md`](deploy.md) for the portable first-deploy runbook. Read this alongside
[`workflow-coverage.md`](workflow-coverage.md), which covers what the workflow this topology
serves does and does not support today.

Setup: ingest and editing on a Windows workstation and a MacBook; a Tier-3 master archive on a
NAS; Tier-2 exports and Immich on a separate Docker host on the same LAN.

## 1. Host decision: server and NAS are separate hosts

Run branchDAM on the same host as the exports directory and Immich, with the Tier-3 archive
mounted read-only over NFS from the NAS, rather than running branchDAM on the NAS itself.

Exports, Immich, and `branchdam.db` all want to be local to one host — the archive is the only
thing that would argue for putting branchDAM on the NAS instead, and that argument is weak:

- A full archive scan re-reads every byte regardless of which host runs branchDAM, since
  `full_hash` is forced for any read-only tier under the default `fullHashPolicy`.
- Thumbnail generation reads every node with a `PENDING` thumbnail state with no tier filter, so
  every master gets read once for its thumbnail regardless of where branchDAM runs.

Both are one-time-per-node costs, not recurring ones, and they become close to zero once an
ingest client posts `EVENT_NODE_CREATED` directly instead of relying on a full rescan (see
[`workflow-coverage.md` §4](workflow-coverage.md#4-ingesting-a-new-master-without-a-full-rescan)).
Until that exists, size the first full scan against the actual archive size before running it —
for a multi-terabyte archive over NFS this is a multi-hour operation, not a routine one.

## 2. Mount table

| Tier | Purpose | Mount |
|---|---|---|
| `TIER3_MASTER_ARCHIVE` | Camera originals | NAS, over NFS, **read-only** at both the compose layer (`:ro`) and `config.yaml` (`readOnly: true`) (Tier 3 is read-only unless `readOnly: false` is configured; the `:ro` mount remains a defense-in-depth default) |
| `TIER2_EXPORTS` | Renders/exports, shared with Immich | Local disk on the server host, read-write |
| `TIER0_LOCAL_STAGING` | Workstation ingest staging root | Server-local mount/directory (`/storage/staging`, `rw`) — empty stub satisfying `storage.Guard.Resolve` for agent offline queue drain; actual bytes remain on workstation NVMe until synced |
| `TIER1_LOCAL_SCRATCH` | Workstation editing cache | Not mounted into the server at all — see §5 |
| `PROJECTS` | — | Not configured — project files are workstation-local; see `workflow-coverage.md` §7 |

## 3. Constraints

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

## 4. Deployment mechanism

This host is deployed and updated through Ansible playbooks (`ansible-playbooks` in the
inventory this document was written against), not a manually-run `docker compose up -d`. The
mount table, tier layout, and constraints above are still the contract the compose file has to
satisfy; how that compose file gets rendered and applied is out of scope for this repo. If your
own deployment target uses plain Docker Compose instead, [`deploy.md`](deploy.md) is the runbook
to follow directly.

## 5. Local editing tiers: scratch vs. staging visibility

`TIER1_LOCAL_SCRATCH` is not mounted into the server at all in this topology — it is
workstation-local NVMe, and mounting it over the network to make it server-visible would trade
away the local editing performance this topology is built to preserve.

In this architecture (Option 3 / #266), ephemeral render caches (e.g. DaVinci Resolve `CacheClip/`)
and proxy media are managed client-side by `branchdam-agent`:
- The agent discovers local render caches and queries `POST /api/v1/agent/node-status` on the server
  to confirm all referenced source masters are live and hash-verified on `TIER3_MASTER_ARCHIVE`.
- Stale render caches are automatically evicted based on inactivity TTL and scratch disk watermarks,
  leaving camera originals on `LocalEditRoot` protected from automated deletion.
- Workstation scratch storage usage and reclaimed bytes are reported back to the server via
  `POST /api/v1/agent/telemetry`.
- Server-side cache pruning (`POST /api/v1/prune`, `pruning.enabled`, `cacheTtlHours`) remains
  available for any server-visible scratch locations.

`TIER0_LOCAL_STAGING`, by contrast, is configured in the server's `config.yaml` (and mounted as an
empty directory, e.g. `/storage/staging`) purely so `storage.Guard.Resolve` recognizes paths
submitted during the agent's offline queue drain (`/storage/staging/<agentId>/...`) before files
are synced and rebased to the Tier-3 archive. The actual media bytes do not need to be transferred
or mounted into `/storage/staging` — an empty directory satisfies `storage.Guard`'s symlink
canonicalization and lets the node metadata be tracked immediately.

## 6. Reaching this server when a workstation isn't on the LAN

Everything above assumes the ingest workstation is on the same LAN as the server host. For a
travelling workstation doing field ingest away from home, see
[`forward-auth.md` §4](forward-auth.md#4-reaching-the-agent-route-off-lan): an overlay network
(Tailscale or equivalent) is the assumed transport, it has to land on the same Traefik hostname
and router rather than exposing the container port directly, and `/api/v1/agent/*`'s
`X-API-Key` auth model is unchanged either way — the security boundary is the key, not the
network.
