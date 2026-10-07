# Configuration reference

Field-by-field reference for `config.yaml`, sourced from [`internal/config/config.go`](../internal/config/config.go)
— that file is the ground truth; this document explains *effect*, not just shape.
[`config.example.yaml`](../config.example.yaml) is the copyable starting point; use this document
to look up what a field actually does, what happens if you get it wrong, and which fields are easy
to forget because they don't appear in the example at all.

All string values may reference the environment as `${VAR}`, expanded at load time
(`config.Load` → `expandEnv`). **An unset variable is left as the literal `${VAR}` text**, not
emptied — a typo'd variable name fails loudly (as an invalid tier string, etc.) rather than
silently producing an empty value. Two exceptions are handled explicitly by `config.Load`:

- An unresolved `${VAR}` in `immich.apiUrl` (e.g. `IMMICH_API_URL` unset) is treated as **empty**
  and logs a warning: Immich sync is disabled. An unresolved `${VAR}` in `immich.apiKey` is a
  **fatal load error** (`validateSecretExpansion`: "unresolved environment variables in
  security-sensitive fields") only when `immich.apiUrl` is non-empty after that step; with no URL
  the key is ignored. A variable that is *set but empty* (as in `.env.example`) is also fine and
  leaves Immich disabled.
- An unresolved `${VAR}` in `admin.bootstrapPAT` is treated as **empty** (bootstrap off), so the
  literal `${...}` text never becomes a wildcard admin PAT secret.

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
never-expanded `${VAR}` (e.g. one that reaches the sync supervisor through the settings layer
rather than through `config.Load`, which already blanks an unresolved `immich.apiUrl`) is treated identically to an empty value by the worker
(`internal/sync.Supervisor` checks for `${`). This does **not** apply to most fields: the general
"fails loudly on a typo'd variable name" behavior described above is unchanged everywhere else.

`GET`/`PUT /api/v1/settings` (gated to an authenticated admin user, same `authz.groups`
membership every other write route uses — never a machine/agent principal, on either method) is
the API this drives; a UI page on top of it lands in a follow-up PR. It exposes every registered
field's current value, whether it's overridden or coming from `config.yaml`/`.env`
(`source: "override" | "config"`), and whether it takes effect immediately or needs a restart
(`applyMode: "live" | "restart"`) — see `internal/settings/registry.go` for the authoritative list.
One domain is intentionally excluded from the registry, not just left for later:

- **`authz.groups` is display-only** (`applyMode: "never"`, `editable: false`) — it gates the
  settings route itself, so a UI edit that locked the operator out of every admin group would have
  no recovery path. Change it only via `config.yaml`/`.env`.

`applyMode` per registered field (authoritative: `internal/settings/registry.go`):

- `live`: `immich.apiUrl`, `immich.apiKey`, `immich.libraryId`, `immich.exportPath`,
  `pruning.enabled`, `pathRewrites`, `ingest.namingTemplate`, `trash.retentionDays`,
  `metadata.autoInherit`.
- `restart`: `logLevel`, `workers.*`, `thumbnails.*`, `http.readTimeoutSecs`,
  `http.writeTimeoutSecs`, `http.exposeOpenAPI`, `http.trustedProxies`, `agent.signedRequests`,
  `agent.replayWindowSecs`, `agent.signedMaxBodyBytes`, `agent.skipSignaturePaths`.
- `never` (display-only): `listenAddr`, `database.path`, `authz.groups`.

Operator Path Rewrites (`pathRewrites`) are registered with `applyMode: "live"` and `editable: true` — UI overrides take effect immediately for subsequent project file introspection. Reverting the override restores the rules from `config.yaml`.

Naming Template (`ingest.namingTemplate`) and Trash Retention Days (`trash.retentionDays`) are registered with `applyMode: "live"` and `editable: true` — UI overrides take effect immediately for subsequent ingest and background trash pruning passes.

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
| `trustedProxies` | list of string | unset | IPs / CIDRs (or `"*"`) from which `X-Forwarded-*` headers are honored. **Unset trusts every source** (backward-compat; logs a startup WARN). **An explicit empty list `[]` denies all forwarded headers.** In `auth.mode: both` forward-auth identity headers are honored only from a non-empty list; with it unset/empty, forward login is disabled (local login still works). Restart to change. |
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
| `signedRequests` | bool | `false` | Require HMAC-SHA256 signatures and replay protection on agent endpoints. |
| `replayWindowSecs` | int | `300` | Maximum allowed clock drift / nonce replay window in seconds. |
| `signedMaxBodyBytes` | int | `16777216` (16 MiB; `0` = default) | Max body the signature validator buffers on signed JSON agent endpoints; over-limit requests get `413`. |
| `skipSignaturePaths` | list of string | `[/api/v1/agent/upload]` | Paths that bypass signature validation (the API key check still runs). Entries ending in `/` match as a prefix, others exactly. Unset uses the default (the streaming upload route); an explicit empty list exempts nothing. |

Agent authentication is per-device pairing only — no shared `apiKey` field exists. Each paired device gets its own key via `POST /api/v1/companion/pairings` (see [`agent-api.md`](agent-api.md)).

## `authz`

| Key | Type | Default | Effect |
|---|---|---|---|
| `groups` | list of string | empty | Groups permitted write (mutating-method) access on browser-routed endpoints, matched against `X-Authentik-Groups`. **Empty means every authenticated user has write access** — the solo-homelab default — and logs a startup WARN naming `authz.groups` so the choice isn't silent. Must match the Authentik group name exactly; there's no validation against Authentik's own group list. |

## `auth`

Selects which authentication chain runs, and tunes the local-auth
surface. See [`docs/local-auth.md`](local-auth.md) for the full operator
guide (cookie contract, argon2id parameters, rate-limit thresholds,
forward-JIT semantics, operational notes). The `auth.mode` field is
the only one most operators ever touch; the rest have safe defaults.

| Key | Type | Default | Effect |
|---|---|---|---|
| `mode` | string | `forward` | One of `forward` (default, pre-#407 behavior), `local` (password login only), or `both` (either path authenticates; local wins on Name/Email collision). Empty / unset is treated as `forward`, so existing configs upgrade without modification. |
| `local.cookieName` | string | `branchdam_session` | The session cookie name. Change if you run multiple branchDAM instances behind the same parent domain and need to disambiguate. |
| `local.idleTimeout` | duration | `24h` | Idle-timeout: the session is revoked if no authenticated request lands within this window after the last `TouchSession`. |
| `local.absoluteTimeout` | duration | `720h` (30d) | Absolute timeout: the session is revoked once this elapses, regardless of activity. Reset on each successful login. |
| `local.argon2.memoryKB` | int | `19456` (19 MiB) | Memory cost of the password hash. See local-auth.md §5 for tuning guidance. |
| `local.argon2.iterations` | int | `2` | Iteration count. |
| `local.argon2.parallelism` | int | `1` | Parallelism per hash. |
| `local.argon2.saltLength` | int | `16` | Salt length in bytes. |
| `local.argon2.keyLength` | int | `32` | Derived key length in bytes. |
| `local.rateLimit.maxFailuresFast` | int | `5` | Fast-window failure threshold (per source IP). |
| `local.rateLimit.fastWindow` | duration | `5m` | Sliding window for the fast threshold. |
| `local.rateLimit.coolOffFast` | duration | `60s` | Cool-off applied when the fast threshold trips. |
| `local.rateLimit.maxFailuresSlow` | int | `10` | Slow-window failure threshold. |
| `local.rateLimit.slowWindow` | duration | `5m` (defaults to the fast window; never smaller than it) | Sliding window for the slow threshold. |
| `local.rateLimit.coolOffSlow` | duration | `5m` | Cool-off applied when the slow threshold trips. |
| `local.passwordReset.tokenTTL` | duration | `24h` | Lifetime of a self-service password-reset token. Blank or non-positive uses `24h`; a malformed value refuses to boot. |
| `forward.adminGroups` | list of string | empty | Forward-auth asserted group names that trigger JIT provisioning of a local `is_admin=1` account. Only meaningful when `auth.mode='both'`. Empty list disables JIT entirely. |
| `forward.requireEmailForJIT` | bool | `true` | When true, refuse JIT provisioning if the forward-auth asserted email is empty. A homelab Authentik deployment that doesn't surface email can set this to `false`; the JIT user is then keyed by username. |
| `email.provider` | string | `log` | Outbound-email backend. `log` (default) prints would-be-sent messages to slog — the password-reset handler still mints tokens either way, but the rendered preview (and the `smtp` send) both require `email.baseURL` to be set; see below. `smtp` delivers via `auth.email.host:port`. |
| `email.host` | string | — | SMTP server hostname. Used by `provider=smtp`. |
| `email.port` | int | `587` | SMTP server port. `587` for STARTTLS, `465` for implicit TLS, `25` for plain. |
| `email.username` | string | empty | SMTP AUTH username. Empty = no AUTH. |
| `email.password` | string | empty | SMTP AUTH password. Supports `${VAR}` expansion; keep in a gitignored `.env`. |
| `email.from` | string | — | Sender address, e.g. `branchDAM <noreply@example.com>`. Format-validated at send time (`mail.ParseAddress`); a CR/LF in the configured value refuses to send. |
| `email.tls` | string | `starttls` | TLS mode. `starttls` (default): require STARTTLS — refuses to send if the server doesn't advertise it (no silent downgrade). `implicit`: TLS-from-dial (port 465). `none`: plain (dev only). |
| `email.baseURL` | string | empty | Public-facing base URL used to build links in outbound messages, e.g. `https://branchdam.example.com`. **Required for the reset link to be sent at all, for every provider** (including `log`): there is no fallback to the inbound request's `Host` header, which is attacker-controlled. When `baseURL` is unset, the handler logs a `WARN` and skips email delivery for that request — including the `provider=log` preview — rather than embed an untrusted host in the link. |

When `auth.mode` is `local` or `both`, the server **refuses to boot**
unless `BRANCHDAM_SECRET_KEY` is set and is valid base64-decoded 32 bytes
(see [`docs/local-auth.md`](local-auth.md) §2). When `auth.mode='forward'`
(the default), `BRANCHDAM_SECRET_KEY` is only used for app-settings
encryption (immich / agent API keys stored in the SPA), not for
session-cookie signing — that path is closed off entirely.

## `ingest`

| Key | Type | Default | Effect |
|---|---|---|---|
| `namingTemplate` | string | `{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}` | Path and filename pattern evaluated by server when ingesting media (`POST /api/v1/agent/upload`). Available tokens: `{yyyy}`, `{mm}`, `{dd}`, `{camera_model}`, `{original_name}`, `{stem}`, `{ext}`. Editable live via web UI settings. |

## `pruning`

| Key | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `true` | Global kill-switch for `POST /api/v1/prune`; when `false` the endpoint answers `409` ("pruning is disabled by server configuration"). Per-location eligibility (`prunable`, `cacheTtlHours`) is under `storageLocations`. Live-editable via the settings API. |

## `metadata`

| Key | Type | Default | Effect |
|---|---|---|---|
| `autoInherit` | bool | `true` | Automated EXIF/XMP identity-metadata inheritance from the winning parent edge to the child on edge confirmation or auto-acceptance. Live-editable via the settings API. |

## `admin`

| Key | Type | Default | Effect |
|---|---|---|---|
| `bootstrapPAT` | string | empty | When non-empty, the first boot mints a one-shot wildcard admin PAT and writes the plaintext to `<dir of database.path>/bootstrap-pat.txt` (mode 0600); later boots refuse to mint while that file exists. Empty disables bootstrap. An unresolved `${VAR}` is treated as empty. See [`admin-pats.md`](admin-pats.md). |

## Environment variables and CLI flags

| Name | Effect |
|---|---|
| `BRANCHDAM_CONFIG` | Default for the `-config` flag (`config.yaml` if unset). |
| `BRANCHDAM_DEBUG` | Non-empty enables debug logging (same as `-debug`). |
| `BRANCHDAM_SECRET_KEY` | Base64-encoded 32-byte key for sealing secret settings and signing session cookies; required for `auth.mode` `local`/`both`. |

`-healthcheck` probes the local `/healthz` and exits (used by the container `HEALTHCHECK`).
Any other `${VAR}` is whatever your `config.yaml` references (e.g. `IMMICH_API_URL`).

## `trash`

| Key | Type | Default | Effect |
|---|---|---|---|
| `retentionDays` | int | `30` | Number of days deleted files are safely preserved under `.trash/` in their storage location before permanent background prune. `0` disables automated unlinking. Only writable locations are purged; a `readOnly` location is never touched. Editable live via web UI settings. |

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
| `apiKey` | string | — | Immich API key. An unresolved `${VAR}` here is a fatal load error only when `apiUrl` is set; with no `apiUrl` it is ignored. |
| `libraryId` | string | — | Immich external-library ID to trigger scans against. **Also disables the worker if empty or unresolved** — an empty library ID would otherwise call `POST /api/libraries//scan` and 404 forever, retrying until the per-row retry bound trips and the row is stuck `PUSH_FAILED` with no recovery short of a config fix. branchDAM refuses to start the worker rather than run one that can only fail. |
| `exportPath` | string | `/storage/exports/immich` | Container path where Immich's external-library mount indexes; the worker enqueues live nodes under this path. |

## `pathRewrites`

Configures the operator-declared host-path → container-path prefix rewrites that Tier-1
project-file parsers (`.dam.json`, `.drp`, `.fcpxml`, `.edl`) need to resolve the paths those files
reference (an editing workstation's `D:\Footage\...` or `/Volumes/Video/...`, not the container's
`/storage/projects/...`). Full resolution strategy, ambiguity policy, and worked examples:
[`integrations.md#2-nle-timelines--path-rewrites`](integrations.md#2-nle-timelines--path-rewrites).
Without at least one matching rule, project-file references fall through to basename-only fallback matching.
Commented out by default in `config.example.yaml` (and omitted entirely from `config.dev.yaml`) — uncomment and adapt for your workstations.

```yaml
pathRewrites:
  - from: "D:\\Footage\\"
    to: "/storage/projects/Footage/"
  - from: "/Volumes/Video/Projects/"
    to: "/storage/projects/Video/"
```

`pathRewrites` can also be configured and updated live via the Settings UI / Settings API (`PUT /api/v1/settings`), taking effect immediately for all subsequent project file parses without requiring a server restart. Reverting the UI override restores the base rules from `config.yaml`.

## `storageLocations`

One entry per mounted storage tier. `tier` must be one of `TIER0_LOCAL_STAGING`,
`TIER1_LOCAL_SCRATCH`, `TIER2_EXPORTS`, `TIER3_MASTER_ARCHIVE`, `PROJECTS`. Applied idempotently on
every startup (`seedStorageLocations`, keyed on `rootPath`'s `UNIQUE` constraint) — no separate
migration step needed when a mount is added, changed, or removed from config.

A `TIER0_LOCAL_STAGING` location serves as the server-side registration namespace for
`branchdam-agent`'s offline ingest queue drain (`EVENT_NODE_CREATED` posted as soon as a file lands
on a workstation, before its bytes reach the Tier-3 archive). Configured with `virtual: true` (or defaulting to virtual for Tier 0), it needs no real media bytes or host directory on the server host — `storage.Guard` resolves virtual locations lexically and skips `EvalSymlinks` and filesystem `statfs` checks.
Per-machine subtree paths (`/storage/staging/<agentId>/...`) should be used to prevent path
collisions across multiple workstations. Virtual locations are never watched or swept, and `TIER0_LOCAL_STAGING` nodes never get a generated thumbnail (`ListPendingThumbnails`
excludes this tier by design, #231) — the node rebases to Tier 3 shortly after, so generation work
is skipped until the final synced master arrives. This is a permanent property of the tier, not a
bug to work around.

| Key | Type | Default | Effect |
|---|---|---|---|
| `name` | string | — | Display name only (non-unique; `rootPath` is the unique mount key, so `rootPath` can be freely edited under an unchanged `name`). |
| `rootPath` | string | — | **Container path**, not host path — must match the right-hand side of the corresponding compose volume mount (or virtual namespace path). See [`deploy.md`](deploy.md)'s tier table. |
| `tier` | string | — | See above. |
| `virtual` | bool | `false` (auto `true` for `TIER0_LOCAL_STAGING`) | Marks the location as a virtual metadata-only namespace. Skips symlink canonicalization and disk presence checks; write attempts via `Guard` are always refused. No container/host volume mount is required. |
| `readOnly` | bool | `false` | Enforced by `storage.Guard.CheckWrite`, which refuses any write against a read-only location before any syscall. Tier 3 is writable by default for server-governed ingest; `readOnly: true` and `:ro` mount are opt-in for archive-only deployments. |
| `prunable` | bool | `false` | Opts this location into TTL cache pruning eligibility (`POST /api/v1/prune`). The schema itself restricts this to `TIER1_LOCAL_SCRATCH` — setting it elsewhere is a config error caught at startup. |
| `cacheTtlHours` | int | `0` (never eligible) | Age by `mtime_unix` (not scan recency) past which an `ACTIVE` node here becomes prunable, **provided it also has a verified `full_hash` on a live Tier-3 ancestor** — `prunable: true` alone never makes anything eligible. **Setting this on a non-prunable location is a fatal startup error** (`validatePruneConfig`), not a silent no-op; a negative value is also rejected outright, since `handlePrune` would otherwise treat it identically to zero and the mistake would never surface. |
| `watch` | bool | `false` | Opts into continuous fsnotify watching. Local NVMe only — fsnotify does not fire reliably over SMB/NFS; use `sweep` for those. **Never honored on Tier 3 or virtual locations** regardless of this flag — the master archive and virtual namespaces are never watched. |
| `sweep` | bool | `false` | Opts into a low-priority differential mtime sweep — the polling adjunct for SMB/NFS shares where `watch` doesn't fire. **Never honored on Tier 3 or virtual locations** — the master archive and virtual namespaces are never swept (a manual scan already covers the MISSING-detection case). |
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
