# Operations

Day-2 reference: what's running, how to upgrade safely, how to back up, and what to check when
something looks wrong. For first-time bring-up, see [`deploy.md`](deploy.md); for what every
config key does, see [`configuration.md`](configuration.md).

## What's running

Everything below is one process (`cmd/branchdam`), started and joined by `main.go`/`run()`. There
is no separate scheduler or sidecar to operate.

| Component | Cadence | Config gate | Log signal |
|---|---|---|---|
| HTTP server (SPA + REST + SSE) | always on | — | `listening addr=...` |
| Watcher supervisor (`internal/pipeline.WatcherSupervisor`) | continuous, per location | `storageLocations[].watch: true` | started only if any location opts in; per-location fsnotify events aren't logged individually |
| Sweeper supervisor (`internal/pipeline.SweeperSupervisor`) | periodic, per location (`sweepIntervalSecs`, default 10 min) | `storageLocations[].sweep: true` | same as above |
| Immich sync worker (`internal/sync.Worker`) | polls `remote_sync_state` every 10s, batches of 16 | `immich.apiUrl` + `immich.libraryId` both set and resolved | `sync: immich worker started libraryID=... exportPath=...`; absent entirely if not configured |
| Agent `event_queue` drainer (`internal/agent.Drainer`) | always on, one query per tick even when idle | none — one global queue, no per-location toggle | — |
| Thumbnail worker (`internal/thumbs.Worker`) | polls `media_nodes` for `PENDING` every `thumbnails.intervalSecs` (default 5s), batched | `thumbnails.enabled` (default `true`) | `thumbs: worker started cacheDir=...`; absent entirely if disabled or the cache dir couldn't be created |

A manual scan (`POST /api/v1/scan`) runs as its own tracked goroutine (`pipeline.ScanTracker`)
independent of the above; it's what the Ingest Jobs page shows.

**Docker image size:** the runtime image ships both `ffprobe` and `ffmpeg` (statically built,
`mwader/static-ffmpeg`, pinned by digest — see the Dockerfile's ffprobe/ffmpeg stage comment).
Adding `ffmpeg` (#224, for video poster-frame thumbnails via
`probe.Prober.ExtractVideoPoster`/`internal/thumbs.Cache.Generate`) adds one more static binary
versus `ffprobe` alone — measured against the exact pinned digest, `ffmpeg` is ~134MB
uncompressed (about the same as `ffprobe` itself) and ~55MB gzip-compressed, so that's roughly
what the final image grows by on disk and what a registry pull transfers, respectively — a
deliberate, documented tradeoff against copying just the smaller `ffprobe` binary from the same
base stage, not an oversight. This is why video thumbnail support wasn't bundled into the
original thumbnail-cache work (PRs #218–#223): it was a scope decision about the image, not a
missing feature.

## Upgrades

This deployment tracks `ghcr.io/s3ntin3l8/branchdam:latest` on purpose (see
[`deploy.md`](deploy.md#3-composeoverrideyaml)), so a release can roll forward on any
`docker compose pull`, not just when you deliberately choose to upgrade. That has one real
consequence: **goose migrations run automatically at container startup and are one-way.** There is
no `goose down` path wired into the app, and even where a migration technically has a `Down` step,
some are documented as deliberate no-ops (data corrections aren't reconstructible — see
`docs/schema.md`'s note on migration `00006`).

So, every time, in this order:

```sh
# 1. Back up first — see below. Before the pull, not after.
# 2. Then:
docker compose pull
docker compose images branchdam   # record the resolved digest
docker compose up -d
docker compose logs -f branchdam  # watch for migration errors, not just a clean "listening" line
```

Recording the digest (or the `-X main.version` build stamp visible via `GET /api/v1/me` /
the SPA) is what makes "did the last pull cause this?" an answerable question later — a floating
tag with no record of what you were actually running isn't reproducible.

## Backup and restore

Stop the container, then copy the entire `/data` volume — **not** just `branchdam.db`. SQLite runs
in WAL mode here, so the live state is spread across `branchdam.db`, `branchdam.db-wal`, and
`branchdam.db-shm`; the main file alone is not a valid backup while the WAL hasn't been
checkpointed.

```sh
docker compose stop branchdam
docker run --rm -v branchdam_branchdam-data:/data -v "$PWD":/backup alpine \
  tar czf /backup/branchdam-data-$(date +%Y%m%d).tar.gz -C /data .
docker compose start branchdam
```

To restore, stop the container, extract the archive back into the volume, and start it again.

There is no automated backup scheduler — this is a manual, deliberate step, not a background job.

**`branchdam.db` can hold encrypted secrets once the settings UI is used.** UI-configured
secret fields (e.g. an Immich API key set through the settings page, see
[`configuration.md`](configuration.md)'s precedence section) are stored in the `app_settings`
table encrypted with `BRANCHDAM_SECRET_KEY` (AES-256-GCM). Two consequences:

- **Back up `BRANCHDAM_SECRET_KEY` (in `.env`) with the same care as the database itself.** Losing
  it while secret overrides exist doesn't corrupt anything — the server still starts, logs an
  error, and falls back to the `.env`/`config.yaml` value for that field — but the encrypted
  override itself is unrecoverable; you'll need to re-enter it through the UI.
- **A `branchdam.db` backup pairs with the `BRANCHDAM_SECRET_KEY` that was active when it was
  taken.** Restoring the database onto a host with a *different* key produces the same graceful
  degradation as a lost key, not a restore failure — but any secret set through the UI has to be
  re-entered.

## One instance per database file

Startup (`reconcileOrphanedScanJobs`) marks every `scan_jobs` row still `RUNNING` as `FAILED`,
on the assumption that this process is the only writer and any `RUNNING` row it finds was left by
a crashed *previous* instance of itself. This is a real, enforced assumption, not just a
convention: running `make dev` (or a second container) against a database file a running instance
already owns will mark that other instance's genuinely in-flight jobs `FAILED` out from under it.
There is no flock/pid guard — don't point two branchDAM processes at the same `database.path`,
ever, including briefly for debugging.

## Pruning

`POST /api/v1/prune` is dry-run by default; the request body must set `execute: true` to actually
delete anything. There is no scheduler — pruning only happens when you call this endpoint, which
for a first test deploy is the right default: it's worth understanding what a dry run reports
before ever letting it delete real files.

A Tier-1 node is eligible only if **all** of:
- it's `ACTIVE` on a `prunable: true` (Tier-1-only) storage location,
- its `mtime_unix` is older than that location's `cacheTtlHours`,
- it has a *live* ancestor (`ACTIVE` or `HIDDEN`, walked through non-`REJECTED` edges) on a
  `TIER3_MASTER_ARCHIVE` location with a verified, non-`NULL`, 64-character `full_hash`.

`Execute` re-verifies eligibility twice more, immediately before deleting, to close two TOCTOU
windows a dry-run `Plan` can't see: the DB-side Tier-3 ancestor can lose its verified hash between
`Plan` and `Execute` (aborts `ErrNoLongerEligible`), and the on-disk file can change after the last
scan observed it (aborts `ErrFileChangedSincePlan`, checked via an immediate `Lstat` against the
`(mtime, size)` `Plan` recorded). Purged nodes are marked `MISSING`, never deleted from the
database — matching the "rows are never deleted" invariant everywhere else in the schema.

## Storage location safe-field overrides

The Storage Health page's inline **Edit** control lets an operator override six fields per
location from the UI: `name`, `watch`, `sweep`, `sweepIntervalSecs`, `cacheTtlHours`, and
`enabled`. `rootPath`, `tier`, and `readOnly` stay config-only -- they gate `storage.Guard`'s
Tier-3 write refusal and the Tier-1 prune authorization, and a UI edit to any of them could
silently invalidate those guarantees. Like every other UI-configurable setting, all six take
effect on the **next restart** -- the seeder (`seedStorageLocations`) is what applies them, and it
only runs at startup. The one exception is display: the Storage Health page's **DISABLED** badge
and the six fields' merged values in the API response reflect an override immediately, since
those are read straight from the override row -- it's only the actual watch/sweep/scan-target
*behavior* that waits for the next restart.

**`enabled: false` is narrower than it looks.** It means: no watch, no sweep, not offered as a
manual scan target, and rendered as disabled in the UI. It does **not** mean the location stops
being read or stops authorizing prunes:

- `storage.Guard` still resolves the location, so thumbnails and `inherit-metadata` on already-
  indexed nodes under it keep working.
- If the location is a `TIER3_MASTER_ARCHIVE`, disabling it does **not** revoke its authorization
  of Tier-1 cache purges -- `ListPrunableNodes` doesn't filter on `is_active`.

The Storage Health page shows a distinct **DISABLED** badge (sourced from the override) separate
from **INACTIVE** (sourced from `is_active`, which only means the mount failed to resolve at
startup and self-heals on the next successful scan) -- don't conflate the two when reading them:
"I turned this off" and "the NAS fell off the network" look different on purpose. The server
refuses to disable the last enabled storage location (`422`), and refuses a positive
`cacheTtlHours` override on a non-prunable location (`422`, mirroring `validatePruneConfig`'s
startup check).

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Container never reaches `(healthy)` | `/healthz` unreachable inside the container, or a startup error before the server binds | `docker compose logs branchdam` — a config load, migration, or storage guard error exits the process before `listening` is ever logged. `/healthz` itself needs no auth headers, so a healthy process should always pass its own HEALTHCHECK. |
| Storage Health shows a location inactive | `storageLocations[].rootPath` in `config.yaml` doesn't match the compose volume's mount target, or the host path doesn't exist | Check the tier table in `deploy.md` §4; `storage.LoadGuard` skips an unresolvable mount rather than failing startup, and `deactivateStorageLocations` marks it inactive — this is silent by design (M6), so check this page after every config change. |
| `GET /api/v1/me` returns an empty `name` | Authentik's outpost `authResponseHeaders` doesn't list `X-Authentik-Username` (or the other identity headers), so Traefik strips it even though the outpost set it | `forward-auth.md` §4. |
| A write returns `403 authentication required` | No `X-Authentik-Username` header reached branchDAM at all — misconfigured `authResponseHeaders`, or a request that reached `:8080` without going through the `authentik@file` middleware | Confirm the request hit the `branchdam` router, not `branchdam-agent` (path prefix `/api/v1/agent` routes differently) or a direct hit on the port. |
| `/api/v1/agent/*` returns `503` | `BRANCHDAM_AGENT_API_KEY` unset or under 32 characters | `openssl rand -hex 32`, set it in `.env`, restart. |
| `/api/v1/agent/*` returns `401` with a key set | Wrong key, or `X-API-Key` header not being forwarded by Traefik | Confirm the key in your request matches `.env` exactly; confirm the agent router's rule matches the request path. |
| Immich sync worker never starts | `immich.apiUrl` or `immich.libraryId` empty, or still holding an unresolved `${VAR}` (the environment variable was never set) | Expected if you don't have Immich configured — this is the documented off switch, not a bug. If you do expect it running, check `.env` for the referenced variable. |
| A scan job sits `RUNNING` forever | The process crashed mid-scan and hasn't restarted yet | On the *next* startup, `reconcileOrphanedScanJobs` marks it `FAILED` automatically — nothing to do manually. If the process is still alive and a job is genuinely stuck, that's a real bug, not this. |
| A scan job is `COMPLETED` but `lastError` says `MISSING sweep skipped: scan saw zero files ...` | The walk completed without error but observed zero files under the location while it still had prior `ACTIVE` nodes — a stale NFS handle recovered as an empty directory, or a remount at the wrong subpath, are the common causes (#225). This is **not a failure**: the scan genuinely completed, and — unlike the pre-#225 behavior — every previously `ACTIVE` node was deliberately left untouched rather than being swept `MISSING` just because this one pass saw nothing. | Check that the mount is actually present and populated at `storageLocations[].rootPath`, then re-run the scan. A location that's supposed to be empty (nothing left to index) can ignore this — it's informational, and no node was affected. A genuinely deleted file is still caught by the sweep on any later pass that sees at least one file. |
| Startup logs `exiftool not found on PATH` | The container image lost its `exiftool` install, or you're running outside the container without it | EXIF/XMP extraction and RAW preview extraction (pHash fallback) are disabled; indexing still works via `fast_hash` alone (spec directive 9.4's documented fallback), so this degrades gracefully but silently reduces what gets extracted. |
| Startup logs `ffprobe not found on PATH` | Same as above, for video stream inspection | Video files still index; stream metadata (codec, duration, resolution) won't be extracted. |
| Video assets stuck `thumb_state = 'UNSUPPORTED'` | `ffmpeg` (not `ffprobe` — a separate binary, separate `Prober.HasFFmpeg()` check) is missing from the container image or the host's `PATH`, so `probe.Prober.ExtractVideoPoster` returns no bytes and `Cache.Generate` has no remaining fallback (#224) | The shipped image includes `ffmpeg` as of #224 — if you're running outside the container or on an older/custom-built image, install it. `docker compose logs branchdam \| grep -i ffmpeg` won't show a startup log the way exiftool/ffprobe do (`Prober` resolves it silently, same as the other two); confirm with `docker compose exec branchdam ffmpeg -version`. |
| Thumbnails never appear | `thumb_state` stuck `PENDING` — worker disabled (`thumbnails.enabled: false`) or never started (check for `thumbs: worker started` at startup); `UNSUPPORTED` — no embedded preview, the file isn't natively decodable, and (for video) no ffmpeg-extractable poster frame either, expected for some formats, not a bug; `FAILED` with `thumb_attempts` at the retry bound — the file is unreadable at the stored path | For `PENDING`, check `thumbnails.enabled` and the cache dir creation log. `UNSUPPORTED` is terminal by design. For `FAILED`, confirm the file still exists at its indexed path; a rescan or `InvalidateThumbnail` (via a metadata inherit or a re-detected move) resets `thumb_attempts` and gives it a fresh set of tries. |
| A node reads `thumb_state = 'READY'` but `GET /api/v1/assets/{id}/thumbnail` 404s | `branchdam.db` was restored from backup without `/data/thumbs` alongside it (or the cache dir was otherwise wiped) — `thumb_state` lives in the database and comes back `READY`, but the cached JPEG it points to is gone | Nothing to do manually for a still-live node (`lifecycle_state` `ACTIVE`/`HIDDEN`): `handleThumbnail` (`internal/httpapi/thumbnail.go`) self-heals a read miss by calling `InvalidateThumbnail`, resetting the node to `PENDING` so `internal/thumbs.Worker.ProcessPending` reclaims and regenerates it on its next pass. An `ARCHIVED`/`MISSING` node's stale `READY` state is left as-is on a read miss — there's no live file to regenerate a thumbnail from, and `ListPendingThumbnails` would never reclaim it either. If thumbnails still don't reappear for a live node after that, treat it as the `PENDING` case above (worker disabled/not started). |
