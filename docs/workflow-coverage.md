# Workflow Coverage: SD Card to Immich

This document walks one specific real-world deployment shape — SD card ingest on a Windows
workstation and a MacBook, local NVMe editing in DaVinci Resolve and Luminar, a Tier-3 master
archive on a NAS, Tier-2 exports and Immich on a separate host — against what branchDAM's server
actually does today, as opposed to what the spec or roadmap describe as eventually landing. It is
a coverage audit, not a tutorial; see [`deploy.md`](deploy.md) for bring-up steps and
[`deploy-topology.md`](deploy-topology.md) for a specific host layout.

## 1. Summary

The server half of this workflow is complete: scanning, hashing, all three resolver tiers, the
audit queue, multi-hop lineage, EXIF/XMP inheritance, thumbnails, the Immich scan trigger, the
pruning engine, and the full `/api/v1/agent/*` contract with its `event_queue` drainer are wired
at boot (`cmd/branchdam/main.go`). **This repo's own `cmd/` still contains exactly one binary** —
but the client that talks to that contract now exists, shipped as phase 10 in the separate
`s3ntin3l8/branchdam-agent` repo (v1.0.1+): SD-card ingest, an offline queue, a DaVinci Resolve
post-render hook, and a Luminar catalog reader are all landed there. What follows describes what
this repo's own build does end-to-end when the agent isn't in the picture, and what the agent
adds when it is.

Three things follow from that:

1. **Without the agent, getting a new master into the graph still means an operator-triggered
   scan** — Tier 3 is deliberately excluded from the always-on background sweeper (§4), so there
   is no automatic pickup absent an ingest client. As of #226, that manual scan no longer has to
   be a full BLAKE3 re-hash of every file already on the archive: `POST /api/v1/scan
   {differential: true}` reuses the same touch-only fast path the background sweeper uses for
   other tiers, so only new or changed files are opened and hashed. This is a meaningful cost
   reduction for a multi-terabyte archive, but it's still a manual step unless the agent's
   `ingest` subcommand is doing the work instead (see §4).
2. **A DaVinci Resolve post-render hook needed no tray app, database, or UI to build.** `.dam.json`
   manifest ingestion already existed server-side and yields confidence-1.00 lineage edges; the
   only missing piece was the small script that writes the manifest — that shipped as
   `branchdam-agent`'s `hooks/resolve/` alongside the rest of phase 10.
3. **Local editing does not break lineage, provided the local copy mirrors the archive's folder
   structure.** See §3.

## 2. Step-by-step coverage

| # | Step | Status | Detail |
|---|---|---|---|
| 1–2 | SD card → one copy to the NAS archive, one to local NVMe for editing | Built, in `branchdam-agent` | Not in this repo — `branchdam-agent`'s `ingest` subcommand (dual-copy write, bit-for-bit verification, safe-eject signalling). A manual copy still works without the agent, but loses that verification and the automatic folder mirror between the two copies |
| 3 | Server learns about the new master | Manual, cheaper since #226 | `POST /api/v1/scan` (full) or `{differential: true}` (touch-only fast path for unchanged files, Tier-3 only) — still operator-triggered, not automatic; see §4 |
| 4 | Master indexed: fast/full hash, EXIF, pHash, promoted camera columns | Works | Requires `exiftool` and `ffprobe`; degrades gracefully (fast-hash only) if either is absent |
| 5 | Luminar: edit local copy → export to Tier 2 | Works via heuristics by default; confidence-1.00 available via `branchdam-agent` | Without the agent, falls back to Tier 2 (filename stem, `XMP:OriginalDocumentID`) and Tier 3 (camera serial + lens + ±2s + pHash Hamming ≤ 10). `branchdam-agent`'s `luminar-sync` reads Luminar's `catalog.db` directly, though its schema mapping is unverified against a real catalog — see that repo's `docs/luminar-catalog.md` |
| 6 | DaVinci Resolve: edit local copy → render + `.dam.json` to Tier 2 | Built, both sides | The `.dam.json` parser is live server-side and yields confidence-1.00 edges; the post-render hook that writes the manifest ships as `branchdam-agent`'s `hooks/resolve/` — see §5 |
| 7 | Tier-2 export auto-detected | Works | `watch: true` (fsnotify, local disk) or `sweep: true` (differential mtime poll, for NFS/SMB) |
| 8 | Export linked to its source master | Works | Tier 1 (`.dam.json`/`.drp`/`.fcpxml`/`.edl`), Tier 2 (filename stem, `XMP:OriginalDocumentID`), Tier 3 (heuristic spatial-temporal) |
| 9 | Low-confidence matches routed to the audit queue | Works | `GET /api/v1/edges/audit`; a human `CONFIRMED`/`REJECTED` decision is permanent and is never overridden by a later resolver run |
| 10 | Child export inherits parent EXIF/GPS | Works | `POST /api/v1/assets/{id}/inherit-metadata` — a manual call, not triggered automatically when an export is detected |
| 11 | Immich indexes the export | Works, with prerequisites | Requires a new **external** library in Immich; see §6 |
| 12 | Local scratch pruned once the master is verified | Built, agent-orchestrated (Option 3 / #266) | Ephemeral NLE render caches (`CacheClip/`) and proxies are pruned client-side by `branchdam-agent` once `POST /api/v1/agent/node-status` confirms Tier-3 masters are live and hash-verified. Camera originals on `LocalEditRoot` remain user-governed. Storage metrics are reported via `POST /api/v1/agent/telemetry` |
| 13 | Google Photos push | No-go by decision | See [`google-photos.md`](google-photos.md) — the sync layer carries no file path, so no byte transfer is possible without reshaping it |

## 3. The folder-mirror requirement

`ProjectSidecarResolver` resolves a project-file reference in three steps: exact container-path
match, an operator-declared prefix rewrite (`pathRewrites`), then a basename fallback (see
[`integrations.md` §2](integrations.md#2-nle-timelines--path-rewrites)). There is no relative-path resolution against the
manifest or project file's own location, and there is no automatic path discovery beyond those
three steps.

If local scratch instead **mirrors** the archive's relative subtree, one `pathRewrites` rule per
workstation resolves every reference exactly:

```yaml
pathRewrites:
  - from: "D:\\scratch\\"
    to: "/storage/archive/"
  - from: "/Users/<user>/scratch/"
    to: "/storage/archive/"
```

This costs nothing in local editing performance — the recommendation is only that the copy step
preserve the archive's relative path instead of flattening it. `config.example.yaml` ships
`pathRewrites` commented out; a first deploy has it off by default. Verify the active rules with
`GET /api/v1/config/path-rewrites`.

**Scan order matters, and nothing retries automatically.** The graph engine resolves each node
when its batch commits. If the exports location is scanned before the archive location has
inserted the master a manifest references, the reference misses and no candidate edge is
emitted — and because edge confidence only ever increases, nothing re-runs that resolver on the
manifest node later on its own. Scan the archive location before the exports location; if a
manifest lands before its referenced master, rescan exports.

**What the resulting graph looks like:** `.dam.json` makes the manifest node a child of every
reference it lists — master and any render both connect through the sidecar node rather than to
each other directly (`role: media` for sources, `role: export` for a render). A direct
render→master edge, if one exists, comes from the Tier 2/3 resolvers independently.

## 4. Ingesting a new master without a full rescan

As of #226, `POST /api/v1/scan` accepts a `differential: true` option, permitted specifically
against `TIER3_MASTER_ARCHIVE` locations: it reuses the same touch-only fast path
(`internal/pipeline/sweep.go`'s `sweepUnchanged`/`touchBatcher`) the always-on background
sweeper already uses for other tiers — a file whose `(mtime, size)` still match its stored node
is touched, never reopened or re-hashed, so only a new or genuinely changed master pays the
BLAKE3 cost. This closes the "full re-hash of the entire archive on every pass" cost called out
in §1, but two things are unchanged: the walk itself (a cheap `Lstat` per file, not a hash) still
covers the whole archive, and the trigger is still manual, not automatic — Tier 3 stays
deliberately excluded from the always-on background `SweeperSupervisor`
(`cmd/branchdam/main.go`'s `sweptFromConfig`), since branchDAM never writes to it and the
motivating cost (re-hashing) is what #226 addresses directly.

The recommended way to record a master without an operator-triggered scan is the workstation agent
(`branchdam-agent ingest -card <path>`), which posts `EVENT_NODE_CREATED` directly over REST.

**A differential pass never backfills a missing or unverified `full_hash`.** `sweepUnchanged`
gates purely on `lifecycle_state = 'ACTIVE'` plus a matching `(mtime, size)` — it never looks at
`full_hash`/`indexing_status`, so a node that was never fully hashed (or whose hash failed) stays
that way forever under a differential-only maintenance routine; only a full scan's
`needsFullHash(policy, tierReadOnly=true, ...)` computes it. This matters beyond integrity
verification: `ListPrunableNodes` requires a non-NULL, 64-length `full_hash` on the live Tier-3
ancestor before a Tier-1 cache copy is purge-eligible (see [`operations.md#cache-pruning-runbook`](operations.md#cache-pruning-runbook)), so
an archive maintained exclusively via `differential: true` passes can silently block Tier-1
pruning for any node whose hash was never computed. A periodic full (non-differential) scan is
still what verifies integrity and keeps Tier-1 pruning eligibility current — differential is a
cost reduction for routine "did anything change" passes, not a full scan's replacement.

## 5. The DaVinci Resolve hook needs no dedicated `PROJECTS` tier

Because `ProjectSidecarResolver` selects its parser by file extension, not by the tier of the
location the file lives in, and `indexer.Walk` applies no extension filter, a `.dam.json`
manifest written into a Tier-2 exports location is parsed exactly the same as one written into a
dedicated projects tier. Combined with local project files being otherwise unreachable (see §7),
the practical consequence is that **no `PROJECTS` storage location is needed for this workflow at
all** — a Resolve post-render hook that writes `render_name.dam.json` alongside its export into
the already-scanned exports directory is sufficient on its own. This was originally filed as
independently buildable in *this* repo (#233), on the strength of that same
no-`PROJECTS`-tier argument; the hook itself still needs nothing beyond what's described above, but
the issue tracking it has since moved to `s3ntin3l8/branchdam-agent#5`, alongside the rest of
phase 10 (see `docs/roadmap.md`).

## 6. Immich integration

Two things to configure explicitly:

- **An Immich library that is managed (internal ingest) rather than external is invisible to
  branchDAM permanently.** The integration is exactly one call —
  `POST /api/libraries/{library_id}/scan` against an **external** library — and transfers no
  bytes. Existing managed-library assets do not appear in branchDAM's graph and never will
  without a separate migration; only assets that later land in the export path and get indexed
  via a new external library are covered.
- **branchDAM's `immich.exportPath` and Immich's external-library path must be the same string.**
  The sync worker filters live nodes by `exportPath` as branchDAM resolves it; Immich scans the
  library path as Immich resolves it. If the two containers mount the shared export directory at
  different paths, the sync worker enqueues nothing, indefinitely, with no error surfaced.
  Mounting the same host directory at the same container path in both containers avoids this.
- The sync worker is all-or-nothing: an empty or unresolved `immich.apiUrl` or `immich.libraryId`
  disables it entirely, logged once at startup.

## 7. What local-only project files mean for lineage

Because Resolve/Luminar project files and catalogs live only on the editing workstation:

- `.drp`/`.fcpxml`/`.edl` introspection cannot fire — `ProjectSidecarResolver` opens the project
  file directly (`os.Open`), which requires a path the server itself can read. A workstation-local
  project file is never reachable this way.
- The Luminar `catalog.db` reader is unaffected by this — it is phase-10, workstation-side by
  design regardless of where the catalog lives.
- `.dam.json` is unaffected, per §5 — it is written into an already-scanned Tier-2 location, not
  read from the workstation.

Do not configure a `PROJECTS` storage location for this setup; nothing would ever populate it.
