# Configuration reference

Field-by-field reference for `config.yaml`, sourced from [`internal/config/config.go`](../internal/config/config.go)
— that file is the ground truth; this document explains *effect*, not just shape.
[`config.example.yaml`](../config.example.yaml) is the copyable starting point; use this document
to look up what a field actually does, what happens if you get it wrong, and which fields are easy
to forget because they don't appear in the example at all.

All string values may reference the environment as `${VAR}`, expanded at load time
(`config.Load` → `expandEnv`). **An unset variable is left as the literal `${VAR}` text**, not
emptied — a typo'd variable name fails loudly (as an invalid tier string, an empty-looking-but-not
API URL, etc.) rather than silently producing an empty value.

## Precedence: `.env`/`config.yaml` vs. the settings UI

`.env`/`config.yaml` are the bootstrap: they are read once, at process start, by
`config.Load`. On top of that, `internal/settings` resolves an `app_settings` database table of
UI-configured overrides, keyed by field. **A field having a row in `app_settings` at all is what
makes it an override — its stored value can be an explicit empty string**, which beats a populated
`.env`/`config.yaml` value on purpose (e.g. disabling Immich from the UI even though
`IMMICH_API_URL` is still set in `.env`). This is not a "non-empty wins" merge; a present row always
wins, regardless of what it holds.

For a handful of fields whose own "empty" value already means "not configured" — `immich.apiUrl`
and `immich.libraryId`, where the sync worker already treats either as its off-switch — a literal,
never-expanded `${VAR}` left over from an unset environment variable is treated identically to an
empty base value when there is no override. This does **not** apply to most fields: the general
"fails loudly on a typo'd variable name" behavior described above is unchanged everywhere else.

`GET`/`PUT /api/v1/settings` (gated to an authenticated admin user, same `authz.groups`
membership every other write route uses — never a machine/agent principal, on either method) is
the API this drives; a UI page on top of it lands in a follow-up PR. It exposes every registered
field's current value, whether it's overridden or coming from `config.yaml`/`.env`
(`source: "override" | "config"`), and whether it takes effect immediately or needs a restart
(`applyMode: "live" | "restart"`) — see `internal/settings/registry.go` for the authoritative list.
Two domains are intentionally excluded from the registry, not just left for later:

- **`authz.groups` is display-only** (`applyMode: "never"`, `editable: false`) — it gates the
  settings route itself, so a UI edit that locked the operator out of every admin group would have
  no recovery path. Change it only via `config.yaml`/`.env`.
- **`pathRewrites` isn't in the registry at all.** It's a list of `{from, to}` objects, not a
  scalar or a string list, and doesn't fit the registry's value model. `GET /api/v1/config/path-rewrites`
  is unaffected; a typed registry entry for it is a candidate for a later PR, not a gap left by
  this one.

Secret-typed fields (e.g. `immich.apiKey`) are encrypted at rest with a key from
`BRANCHDAM_SECRET_KEY`, never returned by `GET` (only `hasValue: true`), and a `PUT` fails with
`422` if the key isn't configured — see [`operations.md`](operations.md) for backup/restore
implications and what happens if that key is absent or lost.

## Top level

| Key | Type | Default | Effect |
|---|---|---|---|
| `listenAddr` | string | `:8080` | The stdlib `http.Server` bind address. Only ever reached by Traefik in the shipped compose setup — the container publishes no ports. |
| `logLevel` | string | `info` | One of `debug`/`info`/`warn`/`error`. The `-debug` CLI flag or `BRANCHDAM_DEBUG` env var overrides this to `debug` regardless of what's here. |

## `database`

| Key | Type | Default | Effect |
|---|---|---|---|
| `path` | string | `/data/branchdam.db` | **Must be absolute.** `storage.Guard`'s `canonicalize` rejects a relative path; more subtly, a storage location with a bad `rootPath` (see below) doesn't fail startup — it's silently skipped and marked inactive, so check Storage Health after first boot rather than trusting a clean startup log alone. |

## `http`

| Key | Type | Default | Effect |
|---|---|---|---|
| `readTimeoutSecs` | int | 15 | `http.Server.ReadTimeout`. |
| `writeTimeoutSecs` | int | 15 | `http.Server.WriteTimeout`. |
| `exposeOpenAPI` | bool | `false` | Serves `/openapi.json`, `/openapi.yaml`, and `/docs`. **Recommended `true` for a test deploy**: the route is already behind Authentik like everything else, and a live, always-current API reference beats a hand-written one that drifts. Leave `false` once you're not actively poking at the API. |

## `workers`

| Key | Type | Default | Effect |
|---|---|---|---|
| `hashWorkers` | int | `0` (auto: `min(4, NumCPU)`) | Goroutine count for the hash pool. Hashing is I/O-bound on a NAS — going wider than the default can thrash disks rather than speed anything up. |
| `fullHashPolicy` | string | `tier3_and_collision` | One of `always` / `tier3_and_collision` / `never`. Controls when the expensive BLAKE3-256 `full_hash` runs, versus relying on the cheap xxHash64 `fast_hash` remap key alone. |
| `perceptualHash` | bool | `true` | **Missing from `config.example.yaml` — this is the default, not something the template shows you turning on.** Enables pHash extraction (`internal/hashing.PerceptualHash`, via `internal/probe`'s RAW-preview fallback chain when a file isn't natively decodable). Everything Tier-3 heuristic matching does (Hamming distance ≤ 10) depends on this being on. Turn it off only if you want faster scans and don't need Tier-3 spatial-temporal matching. |

## `agent`

| Key | Type | Default | Effect |
|---|---|---|---|
| `apiKey` | string | — | The machine-principal key for `/api/v1/agent/*`, which bypasses Authentik ForwardAuth by design (see [`forward-auth.md`](forward-auth.md)). Set via `${BRANCHDAM_AGENT_API_KEY}` from a gitignored `.env`, never inline. **Under 32 characters and every agent route fails closed with `503`**, logged once at startup — this is deliberate fail-closed behavior, not a bug to work around. |

## `authz`

| Key | Type | Default | Effect |
|---|---|---|---|
| `groups` | list of string | empty | Groups permitted write (mutating-method) access on browser-routed endpoints, matched against `X-Authentik-Groups`. **Empty means every authenticated user has write access** — the solo-homelab default — and logs a startup WARN naming `authz.groups` so the choice isn't silent. Must match the Authentik group name exactly; there's no validation against Authentik's own group list. |

## `immich`

Configures the external-library scan-trigger client (`internal/immich`) and its sync worker
(`internal/sync`). All four fields matter together — the worker is either fully configured or off,
there's no partial mode.

All four are `applyMode: "live"` in the settings registry — the only fields that are. A
`PUT /api/v1/settings` changing any of them runs synchronously inside `internal/sync.Supervisor`'s
`Reload` (registered via `settingsStore.Subscribe` in `cmd/branchdam/main.go`), which stops the
currently running worker, waits for it to fully exit, and starts a replacement built from the new
client config — or leaves it stopped, per the off-switch rules below. `Reload` no-ops when a
settings write changed something unrelated (e.g. `logLevel`), so unrelated writes don't bounce the
worker. No restart of the branchDAM process itself is needed for an Immich change to take effect.

| Key | Type | Default | Effect |
|---|---|---|---|
| `apiUrl` | string | — | **Empty, or containing an unresolved `${VAR}`, disables the sync worker entirely** (`internal/sync.Supervisor`). This is a deliberate off-switch, not a misconfiguration — no Immich instance is required to run branchDAM. |
| `apiKey` | string | — | Immich API key. |
| `libraryId` | string | — | Immich external-library ID to trigger scans against. **Also disables the worker if empty or unresolved** — an empty library ID would otherwise call `POST /api/libraries//scan` and 404 forever, retrying until the per-row retry bound trips and the row is stuck `PUSH_FAILED` with no recovery short of a config fix. branchDAM refuses to start the worker rather than run one that can only fail. |
| `exportPath` | string | `/storage/exports/immich` | Container path where Immich's external-library mount indexes; the worker enqueues live nodes under this path. |

## `pathRewrites`

**Missing from both `config.example.yaml` and `config.dev.yaml` entirely — a first deploy copying
the example ships with Tier-1 project-file introspection silently inert unless this is added by
hand.** Configures the operator-declared host-path → container-path prefix rewrites that Tier-1
project-file parsers (`.dam.json`, `.drp`, `.fcpxml`, `.edl`) need to resolve the paths those files
reference (an editing workstation's `D:\Footage\...` or `/Volumes/Video/...`, not the container's
`/storage/projects/...`). Full resolution strategy, ambiguity policy, and worked examples:
[`project-paths.md`](project-paths.md). Without at least one matching rule, project-file references
fall through to basename-only fallback matching (the size half of that fallback is aspirational --
see [`project-paths.md`](project-paths.md#1-primary-path-resolution-strategy) -- no parser
currently supplies a file size for it to filter on).

```yaml
pathRewrites:
  - from: "D:\\Footage\\"
    to: "/storage/projects/Footage/"
  - from: "/Volumes/Video/Projects/"
    to: "/storage/projects/Video/"
```

## `storageLocations`

One entry per mounted storage tier. `tier` must be one of `TIER0_LOCAL_STAGING`,
`TIER1_LOCAL_SCRATCH`, `TIER2_EXPORTS`, `TIER3_MASTER_ARCHIVE`, `PROJECTS`. Applied idempotently on
every startup (`seedStorageLocations`, keyed on `rootPath`'s `UNIQUE` constraint) — no separate
migration step needed when a mount is added, changed, or removed from config.

A `TIER0_LOCAL_STAGING` location serves as the server-side registration stub for
`branchdam-agent`'s offline ingest queue drain (`EVENT_NODE_CREATED` posted as soon as a file lands
on a workstation, before its bytes reach the Tier-3 archive). It needs no real media bytes on disk
on the server host — an empty directory satisfies `storage.Guard`'s `EvalSymlinks` canonicalize
step, allowing `storage.Guard.Resolve` to match the path and track the node metadata immediately.
Per-machine subtree paths (`/storage/staging/<agentId>/...`) should be used to prevent path
collisions across multiple workstations. A `TIER0_LOCAL_STAGING` location is scanned and indexed
like any other tier, but its nodes never get a generated thumbnail (`ListPendingThumbnails`
excludes this tier by design, #231) — the node rebases to Tier 3 shortly after, so generation work
is skipped until the final synced master arrives. This is a permanent property of the tier, not a
bug to work around.

| Key | Type | Default | Effect |
|---|---|---|---|
| `name` | string | — | Display name only (non-unique; `rootPath` is the unique mount key, so `rootPath` can be freely edited under an unchanged `name`). |
| `rootPath` | string | — | **Container path**, not host path — must match the right-hand side of the corresponding compose volume mount. See [`deploy.md`](deploy.md)'s tier table. |
| `tier` | string | — | See above. |
| `readOnly` | bool | `false` | Enforced twice: at the DB `CHECK` level and by `storage.Guard.CheckWrite`, which refuses any write against a read-only location before any syscall. Tier 3 must always be `true`. |
| `prunable` | bool | `false` | Opts this location into TTL cache pruning eligibility (`POST /api/v1/prune`). The schema itself restricts this to `TIER1_LOCAL_SCRATCH` — setting it elsewhere is a config error caught at startup. |
| `cacheTtlHours` | int | `0` (never eligible) | Age by `mtime_unix` (not scan recency) past which an `ACTIVE` node here becomes prunable, **provided it also has a verified `full_hash` on a live Tier-3 ancestor** — `prunable: true` alone never makes anything eligible. **Setting this on a non-prunable location is a fatal startup error** (`validatePruneConfig`), not a silent no-op; a negative value is also rejected outright, since `handlePrune` would otherwise treat it identically to zero and the mistake would never surface. |
| `watch` | bool | `false` | Opts into continuous fsnotify watching. Local NVMe only — fsnotify does not fire reliably over SMB/NFS; use `sweep` for those. **Never honored on Tier 3** regardless of this flag — the master archive is never watched. |
| `sweep` | bool | `false` | Opts into a low-priority differential mtime sweep — the polling adjunct for SMB/NFS shares where `watch` doesn't fire. **Never honored on Tier 3** — it's read-only, so nothing there can ever be ingested; a manual scan already covers the MISSING-detection case. |
| `sweepIntervalSecs` | int | `600` (10 min) | Interval between sweep passes. Zero or negative both fall back to the default rather than busy-looping — but a negative value isn't a meaningful setting, just a value that happens to be handled safely. |

Setting both `watch` and `sweep` on the same location is logged as a WARN at startup — wasteful
(both mechanisms will independently notice the same change) but not corrupting, since the writer
DB connection is single-connection and serializes both Commits.

## `thumbnails`

Configures the JPEG thumbnail cache (`internal/thumbs`). The cache directory lives on the app's
own `/data` volume, not inside any `storageLocations` tier, and deliberately does not route
through `storage.Guard` — see `internal/thumbs`' package doc.

| Key | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `true` | Turns the background thumbnail worker on or off. Reads of a JPEG already on disk still work either way; disabling just stops new/invalidated thumbnails from ever leaving `PENDING`. |
| `cacheDir` | string | `/data/thumbs` | Root directory thumbnails are written under, sharded `<uuid[0:2]>/<uuid[2:4]>/<uuid>.jpg`. Must be absolute, same constraint as `database.path`. Already covered by the `branchdam-data` volume mount — no separate compose change needed. |
| `maxEdgePx` | int | `0` (auto: `thumbs.DefaultMaxEdgePx`, 512px) | Longest-edge target in pixels a thumbnail is scaled to; never upscaled. |
| `workers` | int | `0` (auto: `min(4, NumCPU)`) | Goroutine count the worker fans a batch out across for `Generate`/encode, which is CPU/exiftool-subprocess-bound. Mirrors `workers.hashWorkers`' "0 = auto" convention. Each node's DB write still serializes through the single-connection writer pool regardless of this value. |
| `intervalSecs` | int | `0` (auto: `thumbs.DefaultInterval`, 5s) | Polling interval between batches when the pending-thumbnail queue is empty, mirroring `storageLocations[].sweepIntervalSecs`. |

`thumb_state` on a node is one of `PENDING` (queued or just invalidated), `READY` (cached JPEG
exists at `Cache.Path(uuid)`), `UNSUPPORTED` (neither natively decodable, nor carrying an embedded
preview, nor — for video — yielding a decodable poster frame via `ffmpeg` — terminal, not
retried), or `FAILED` (retried up to `internal/thumbs.DefaultMaxAttempts` times, then left alone).
`GET /api/v1/assets/{id}/thumbnail` 404s unless `thumb_state = READY`.

Video files get a thumbnail the same way RAW stills do: `Cache.Generate` falls back to
`probe.Prober.ExtractVideoPoster` (a single representative frame via `ffmpeg -ss ... -frames:v 1`,
trying a one-second-in seek first and the very first frame as a guaranteed-to-exist fallback) when
the source is neither natively decodable nor carries an exiftool-extractable embedded preview.
This is why the runtime image ships `ffmpeg` alongside `ffprobe` as of #224 — see the Dockerfile's
ffprobe/ffmpeg stage comment and docs/operations.md's Docker image size note for the size
tradeoff that decision carries (an extra static binary, on the same order as `ffprobe` itself),
and why video thumbnails weren't bundled into the original thumbnail-cache work.
