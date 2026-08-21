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
| Startup logs `exiftool not found on PATH` | The container image lost its `exiftool` install, or you're running outside the container without it | EXIF/XMP extraction and RAW preview extraction (pHash fallback) are disabled; indexing still works via `fast_hash` alone (spec directive 9.4's documented fallback), so this degrades gracefully but silently reduces what gets extracted. |
| Startup logs `ffprobe not found on PATH` | Same as above, for video stream inspection | Video files still index; stream metadata (codec, duration, resolution) won't be extracted. |
| Thumbnails never appear | `thumb_state` stuck `PENDING` — worker disabled (`thumbnails.enabled: false`) or never started (check for `thumbs: worker started` at startup); `UNSUPPORTED` — no embedded preview and the file isn't natively decodable, expected for some formats, not a bug; `FAILED` with `thumb_attempts` at the retry bound — the file is unreadable at the stored path | For `PENDING`, check `thumbnails.enabled` and the cache dir creation log. `UNSUPPORTED` is terminal by design. For `FAILED`, confirm the file still exists at its indexed path; a rescan or `InvalidateThumbnail` (via a metadata inherit or a re-detected move) resets `thumb_attempts` and gives it a fresh set of tries. |
| A node reads `thumb_state = 'READY'` but `GET /api/v1/assets/{id}/thumbnail` 404s | `branchdam.db` was restored from backup without `/data/thumbs` alongside it (or the cache dir was otherwise wiped) — `thumb_state` lives in the database and comes back `READY`, but the cached JPEG it points to is gone | Nothing to do manually: `handleThumbnail` (`internal/httpapi/thumbnail.go`) self-heals a read miss by calling `InvalidateThumbnail`, resetting the node to `PENDING` so `internal/thumbs.Worker.ProcessPending` reclaims and regenerates it on its next pass. If thumbnails still don't reappear after that, treat it as the `PENDING` case above (worker disabled/not started). |
