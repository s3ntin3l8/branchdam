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

Partial indexes (`WHERE lifecycle_state <> 'ARCHIVED'`) and `CREATE VIEW` both parsed without
issue. sqlc's `emit_interface: true` generates a `Querier` interface so `internal/pipeline` and
`internal/graph` can take a fake for testing rather than a real `*sql.DB`.

One follow-up, not blocking: `v_media_edges_resolved.parent_alive`/`parent_missing` — computed
boolean expressions in a view — generate as Go `interface{}` rather than `bool`, because sqlc
can't infer a type for a boolean predicate over a view. Add an `overrides:` entry in `sqlc.yaml`
mapping those two columns to `bool` when `internal/graph` (PR 7) starts consuming them.

## Post-Increment-1 Additions

### Issue #39 (Tier-3 EXIF Fields Migration)
- Promoted `camera_serial` (TEXT) and `lens_model` (TEXT) onto `media_nodes` from `node_metadata` overflow key-values so Tier-3 heuristic spatial-temporal queries can run efficiently in SQL without metadata joins.
- Added partial index `ix_media_nodes_camera_time ON media_nodes(camera_serial, captured_at_unix) WHERE camera_serial IS NOT NULL`.
- Added migration `00002_tier3_camera_fields.sql`.
- Added `ListTier3Candidates` query in `internal/db/queries/media_nodes.sql`.

### Phase 7 (#53): `remote_sync_state`'s first write path
- `remote_sync_state` is no longer DDL-only. Phase 7 landed its first write path (#53): the query surface + `internal/sync` push state machine — idempotent on the `(node_id, remote)` PK, per-remote-scoped, with an atomic claim.
- Plus the `ListLiveNodesForSync` enqueue source (#55) and the `ListRemoteSyncStateByNode` sync-status read (#156).
- No schema migration was needed — the table already existed in `00001_init.sql`.
