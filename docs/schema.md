# Schema: corrections, deviation ledger, and the sqlc risk finding

This document is the bridge between [`docs/spec/original-spec.md`](spec/original-spec.md)
(the design spec as received, committed verbatim) and
[`internal/db/migrations/00001_init.sql`](../internal/db/migrations/00001_init.sql) (what
actually shipped). Where they disagree, the migration is right.

## The nine corrections

The spec's §6 DDL and §8 compose have defects that would be expensive to discover after data
exists. Each is numbered here and cross-referenced by comment (`fix #N`) at its point of use in
the migration.

1. **Tier vocabulary is incoherent and incomplete.** §2 defines four tiers (Tier 0 local
   staging, Tier 1 local scratch, Tier 2 exports, Tier 3 master archive) plus a `PROJECTS`
   location, but `storage_locations.tier` omits Tier 1 entirely — while Pillar 2's Cache Pruning
   Engine operates on exactly that tier. Separately, `media_nodes.location_type` is a *second,
   differently-spelled* enum for the same concept (`CENTRAL_TIER3` vs `TIER3_MASTER_ARCHIVE`).
   **Fix:** one vocabulary covering all five —
   `TIER0_LOCAL_STAGING, TIER1_LOCAL_SCRATCH, TIER2_EXPORTS, TIER3_MASTER_ARCHIVE, PROJECTS` —
   and `location_type` is deleted; tier is always derived by joining `storage_location_id`.
2. **The lifecycle state machine doesn't exist in the schema.** Pillar 3 specifies
   `CREATED → INDEXED → GRAPH_LINKED → PENDING_CLOUD_PUSH → PUSHED`; the spec's single `status`
   column permits none of those names. **Fix:** three orthogonal axes —
   `indexing_status` (how much we know about the bytes), `graph_status` (how much we know about
   lineage), and `lifecycle_state` (does the row currently exist / is it current) — with the
   sync axis living only in `remote_sync_state`, where the spec already had it.
3. **`file_path UNIQUE` makes the spec's own version-collision rule impossible.** §5 says
   re-exporting over a filename creates a *new* node and archives the old; both rows would then
   share a `file_path`, which `UNIQUE` forbids. **Fix:** partial unique index
   `ux_media_nodes_live_path ON media_nodes(file_path) WHERE lifecycle_state <> 'ARCHIVED'` plus
   a `superseded_by` column. Verified working on SQLite 3.45.1 before the migration was written.
4. **`DERIVED_FROM_MISSING_PARENT` is a state encoded as a relationship type.** The spec's own
   triggers prove it: `trg_media_nodes_missing_parent` only rewrites `DERIVED_FROM` edges, so
   children linked by `FINAL_EXPORT`/`PROXY_OF`/`PROJECT_SIDECAR` never get the unlinked badge
   Pillar 5 promises; the restore trigger is lossy; and rewriting the type can violate
   `UNIQUE(source, target, relationship_type)` and abort the transaction. **Fix:** drop both
   triggers, remove the value from the `relationship_type` enum, and derive parent/child
   liveness in `v_media_edges_resolved` — a join, computed on every read, so there is nothing to
   restore lossily and it works uniformly across every relationship type.
5. **`trg_media_nodes_updated_at` is unguarded self-recursion.** It only works because
   `PRAGMA recursive_triggers` defaults off. **Fix:** no trigger; every query in
   `internal/db/queries` that updates a row sets `updated_at = unixepoch()` explicitly.
6. **`ON DELETE CASCADE` on both edge FKs destroys the lineage the product exists to preserve.**
   **Fix:** every FK is `RESTRICT`. A file that disappears sets `lifecycle_state = 'MISSING'`;
   rows are never deleted. This fix is **inert without `PRAGMA foreign_keys = ON`**, which
   defaults off and is connection-scoped — it must be (and is) set in `internal/db`'s
   `ConnectHook` for both pools, not just once at startup. See `TestForeignKeysEnforced`.
7. **Nothing enforces a DAG.** No `CHECK (source_node_id <> target_node_id)`, no cycle
   prevention, yet the UI walks the graph recursively. **Fix:** `CHECK (source_node_id <>
   target_node_id)` blocks self-loops in the engine (verified: it fires). Longer cycles can't be
   caught by a `CHECK` — SQLite constraints can't see other rows — so `internal/graph` (PR 7)
   runs a recursive-CTE descendant walk (`WouldCreateCycle`) inside the same write transaction
   as the edge insert, which `DB.InTx`'s single writer connection makes sound without extra
   application-level locking.
8. **`full_hash` is called "cryptographic" but specified as xxHash64.** It is not, and §4's
   "bit-for-bit verification before safe ejection" is not met by a 64-bit non-cryptographic
   digest. **Fix:** `full_hash` is BLAKE3-256 (64 hex, length-CHECKed); `fast_hash` stays
   xxHash64 (16 hex), which only needs to be a cheap remap key. A `CHECK` on both columns'
   `length()` makes it structurally impossible to write a 64-bit value into the "cryptographic"
   column.
9. **Node identity needs to be both an integer and a UUID.** The spec has
   `media_nodes.id TEXT PRIMARY KEY -- UUIDv7`. `INTEGER PRIMARY KEY` is materially better inside
   SQLite (rowid alias, smaller indexes, cheaper joins), but it can't be the *only* ID: Pillar 3
   requires the offline workstation agent to mint node IDs in its own `queue.db` **before the
   server has seen the file**, so lineage survives path rebasing `LOCAL_STAGING → CENTRAL_TIER3`;
   and Pillar 4 writes `node_id` into `XMP-dc:Identifier`/`XMP-xmpMM:DerivedFrom` on assets
   pushed to Immich and Google Photos, where an autoincrement integer is a globally meaningless
   value baked into third-party services. **Fix:** `id INTEGER PRIMARY KEY` for internal joins,
   plus `node_uuid TEXT NOT NULL UNIQUE` (UUIDv7) as the external identity used by the API
   surface, XMP tags, and agent-minted rows.

## Deviation ledger

| Spec | Becomes | Why |
| --- | --- | --- |
| `id TEXT PRIMARY KEY` (UUIDv7) | `id INTEGER PRIMARY KEY` + `node_uuid TEXT UNIQUE` | Fix #9 — internal join performance without losing externally-mintable identity |
| `location_type` enum | *deleted*, derived via `storage_location_id` | Fix #1 — two spellings of one concept |
| `status` enum | `indexing_status` × `graph_status` × `lifecycle_state` | Fix #2 — three orthogonal axes |
| `matching_mechanism` CHECK enum | free-text `resolver` + integer `tier` | A closed enum means a schema migration per new resolver; `tier` preserves the queryable grouping |
| `manual_override BOOLEAN` | folded into `review_state` + `reviewed_by`/`reviewed_at` | A boolean can't distinguish "human confirmed" from "human rejected", and loses who and when |
| `confidence_score` | `confidence` | Naming only |
| `DERIVED_FROM_MISSING_PARENT` relationship type | removed; `v_media_edges_resolved.parent_missing` (derived) | Fix #4 |
| Two triggers (`updated_at`, missing-parent propagation) | no triggers; set explicitly in queries / derived in the view | Fixes #4, #5 |
| `ON DELETE CASCADE` (both edge FKs) | `ON DELETE RESTRICT` (all FKs) | Fix #6 |
| `DATETIME`/`CURRENT_TIMESTAMP` columns | `INTEGER` unix-epoch columns (`unixepoch()`) | Consistent, comparable, no timezone-string parsing in Go |

## sqlc risk: resolved

The build plan flagged [sqlc#3286](https://github.com/sqlc-dev/sqlc/issues/3286) (`RECURSIVE cte
failed with star expansion failed for query`, open since March 2024) as a risk to recursive
lineage traversal, sqlc/SQLite's weakest area with ~65 open engine issues at the time of writing.

**What actually happened when spiked:** the failure wasn't star-expansion — it was a *bare
parameter* as a CTE anchor's select target (`SELECT ?1`), which sqlc's SQLite parser can't infer
a column name for (`*ast.ResTarget has nil name`). The fix is a one-line house rule, not a
fallback:

> **Always name or alias every column in a recursive CTE's anchor `SELECT`.** `SELECT
> sqlc.arg(child_node_id) AS id` works; `SELECT ?1` does not.

See `WouldCreateCycle` in [`internal/db/queries/media_edges.sql`](../internal/db/queries/media_edges.sql)
for the working pattern. `sqlc.arg(...)` is also strictly nicer than positional `?N` — it
generates a named Go struct field (`ParentNodeID`) instead of `Column1`.

**Caveat found in #61:** that preference has a real limit. `sqlc.arg(name)` fails to parse
(cascading "extraneous input" / "no viable alternative" errors that look like they're pointing at
the *new* query) when the same `.sql` file already has an earlier query using plain positional
`?N` placeholders — reproduced by bisection with a trivial two-query file, so it's a real sqlc
v1.31.1 bug against this schema, not a one-off. `internal/db/queries/media_nodes.sql` in
particular has both styles already (`ListTier3Candidates` etc. use `?N`), so any new query added
there should use plain `?N` too, not `sqlc.arg`. `GLOB` is also unsupported outright ("no viable
alternative at input 'GLOB'") — use `length(x)`/other operators.

Partial indexes (`WHERE lifecycle_state <> 'ARCHIVED'`) and `CREATE VIEW` both parsed without
issue. sqlc's `emit_interface: true` generates a `Querier` interface so `internal/pipeline` and
`internal/graph` can take a fake for testing rather than a real `*sql.DB`.

One follow-up, not blocking: `v_media_edges_resolved.parent_alive`/`parent_missing` — computed
boolean expressions in a view — generate as Go `interface{}` rather than `bool`, because sqlc
can't infer a type for a boolean predicate over a view. Add an `overrides:` entry in `sqlc.yaml`
mapping those two columns to `bool` when `internal/graph` (PR 7) starts consuming them.

## Post-Increment-1 Additions

Every migration after `00001_init.sql`, in order:

| Migration | Adds | Why |
|---|---|---|
| `00002_tier3_camera_fields.sql` | `media_nodes.camera_serial`, `.lens_model`; partial index `ix_media_nodes_camera_time` | Phase 3/issue #39 — see below |
| `00003_event_queue_retry_count.sql` | `event_queue.retry_count` | Phase 8's agent drainer bounds retries the same way `remote_sync_state` does (below), rather than retrying a poison event forever |
| `00004_remote_sync_state_index.sql` | `ix_remote_sync_state_remote_status ON remote_sync_state(remote, sync_status, last_attempt_at, node_id)` | Issue #165 — every hot query against this table (the sync worker's claim query, both crash-recovery re-claim queries) filters on `remote` + `sync_status`, but the only existing index was the `(node_id, remote)` PK; every one of those queries was a full table scan plus a filesort. `node_id` trails the index specifically to also satisfy the claim query's tiebreaker without an extra in-memory sort |
| `00005_remote_sync_state_retry_count.sql` | `remote_sync_state.retry_count` | #182/#207 — bounds Immich push retries so a permanently-failing row (e.g. a stale `libraryId`) surfaces as exhausted rather than retrying forever |
| `00006_downgrade_index_suffix_stem_edges.sql` | one-time `UPDATE` (not a schema change) | Issue #132's data-correction migration — see below |

### Issue #39 (Tier-3 EXIF Fields Migration)
- Promoted `camera_serial` (TEXT) and `lens_model` (TEXT) onto `media_nodes` from `node_metadata` overflow key-values so Tier-3 heuristic spatial-temporal queries can run efficiently in SQL without metadata joins.
- Added partial index `ix_media_nodes_camera_time ON media_nodes(camera_serial, captured_at_unix) WHERE camera_serial IS NOT NULL`.
- Added migration `00002_tier3_camera_fields.sql`.
- Added `ListTier3Candidates` query in `internal/db/queries/media_nodes.sql`.

### Issue #132 (index-suffix `filename_stem` matches capped below auto-accept)
- `internal/graph.FilenameStemResolver` now requires a live, bare anchor node before emitting a
  candidate at all, and caps an index-suffix-derived match (`-N`/`(N)`, e.g. `trip-1.jpg`) at
  `0.89` — strictly below Tier 2's `0.90` auto-accept threshold — so it always lands in the audit
  queue unless corroborated by a non-`filename_stem` resolver. Role suffixes (`_edit`, `_proxy`,
  `_vN`, `` copy``) are unaffected; collapsing those siblings to a shared stem remains the
  resolver's intended case. See `CLAUDE.md`'s Key invariants for the full rationale.
- This is a code fix, not a schema change, but `UpsertMediaEdge`'s confidence-only-increases rule
  (`ON CONFLICT ... DO UPDATE ... confidence = MAX(excluded, stored)`) means it only governs
  *future* resolves — an edge already written `AUTO_ACCEPTED` under the old, unbounded logic would
  never self-correct on a rescan. Migration `00006_downgrade_index_suffix_stem_edges.sql` is the
  matching one-time data correction: it downgrades exactly the rows the new logic wouldn't have
  written (`resolver = 'filename_stem'`, `review_state = 'AUTO_ACCEPTED'`, and a character-adjacency
  test that identifies the index-suffix case without `GLOB` or `LIKE` — see the migration's own
  comment for the documented, deliberately conservative miss on multi-marker filenames), and
  recomputes `graph_status` for the narrow `LINKED → NEEDS_REVIEW` transition that downgrade can
  cause. `CONFIRMED`/`REJECTED` rows are never touched, migrations included.

### Phase 7 (#53): `remote_sync_state`'s first write path
- `remote_sync_state` is no longer DDL-only. Phase 7 landed its first write path (#53): the query surface + `internal/sync` push state machine — idempotent on the `(node_id, remote)` PK, per-remote-scoped, with an atomic claim.
- Plus the `ListLiveNodesForSync` enqueue source (#55) and the `ListRemoteSyncStateByNode` sync-status read (#156).
- No schema migration was needed — the table already existed in `00001_init.sql`.

### Issue #61 (TTL Cache Pruning Engine)
- No migration — eligibility is expressible against the existing schema (`full_hash`'s length
  `CHECK`, `lifecycle_state`, `storage_locations.tier`/`prunable`). TTL itself is config-only
  (`StorageLocation.CacheTTLHours`), not a column — `prunable` alone was already plumbed
  end-to-end (config → DB → both storage DTOs → `StorageHealthPage.tsx`) but had no TTL semantics
  before this issue gave it one.
- Added `ListPrunableNodes` in `internal/db/queries/media_nodes.sql`: a Tier-1 `ACTIVE` node past
  its TTL (`mtime_unix`, not `last_seen_at`) is eligible only if a *live*
  (`lifecycle_state IN ('ACTIVE','HIDDEN')`) ancestor — walked via `media_edges` target→source,
  `REJECTED` edges excluded — on a `TIER3_MASTER_ARCHIVE` location has a non-NULL, 64-length
  `full_hash`. Uses plain `?1`/`?2` positional params, not `sqlc.arg`, and `length(full_hash) = 64`
  instead of a `GLOB` hex check — see the sqlc risk caveat above for why.
- Added `internal/prune` (`Plan`/`Execute`): `Execute` is `storage.Guard.Remove`'s first real
  production caller, gated by `Guard.CheckWrite` before every deletion. Purged nodes are marked
  `MISSING`, never deleted, matching the "rows are never deleted" invariant.
- Added `POST /api/v1/prune` — admin-gated automatically (`auth.RequireAdmin` gates by HTTP
  method, not per-route), dry-run by default (`execute` must be set explicitly to purge).
- **Follow-up (late Hermes finding on #177):** `Execute` re-verifies eligibility inside the
  delete's own transaction (`ErrNoLongerEligible` if the DB-side Tier-3 ancestor lost its verified
  hash since `Plan`), but that alone doesn't cover a stale `mtime_unix` — the DB row is only as
  fresh as the last scan/sweep. `Execute` now also `os.Lstat`s the file immediately before
  `guard.Remove` and aborts with `ErrFileChangedSincePlan` on an `(mtime, size)` mismatch against
  what `Plan` recorded; a file already gone on its own is treated as success (nothing left to
  remove, node still lands `MISSING`). See `CLAUDE.md`'s Key invariants for the full rationale.
