# Roadmap: increment 2 and beyond

Increment 1 (PRs #1–#18) shipped the schema, storage guard, hashing, probe, indexer, workers,
pipeline, Tier-2 graph resolution, auth, SSE, the Huma REST API, the React SPA, Docker, and full
CI with branch protection. This document phases everything that's left — both what the spec
still asks for, and a large slice of what increment 1 *built but never wired up*.

Each phase below is filed as GitHub issues (one issue per PR) tracked on this repo's board and
worked through [mullion task master](https://github.com/s3ntin3l8/mullion-session-manager). Task
master has no dependency or ordering concept of its own — it polls for issues labelled
`mullion-task` and claims whatever is `ready`, two at a time, in no particular order. Every issue
this roadmap files therefore carries a `Manual: true` line so it starts parked in backlog;
promoting an issue to `ready` is a deliberate act, done in dependency order, one at a time.

## Two findings that shape the ordering

1. **A third of increment 1's built surface has zero production callers**, verified by callsite
   census rather than inference. The most consequential: nothing in production ever sets
   `lifecycle_state = 'MISSING'`, so the move-detection branch in
   `internal/pipeline/commit.go` — built and tested in PR #6 — has never executed outside a test.
   Unlike the documented one-hop graph scope line, this isn't a stated limitation; it's a latent
   gap. Closing it needs no new dependencies and unblocks several later phases, so it's phase 1.
2. **The schema was built additive.** `remote_sync_state`, `node_metadata`, and `event_queue`
   already exist in their final shape from migration `00001_init.sql`. The delivery, metadata,
   and agent-contract phases below open with query files, not migrations. Only Tier-3 heuristic
   matching needs a real schema change (promoting `camera_serial`/`lens_model` onto
   `media_nodes`, indexing `captured_at_unix`).

## Ground rules every issue carries

- **Cite the spec for intent, the code for the contract.** `docs/spec/original-spec.md` is a
  historical input with nine documented deviations (`docs/schema.md`). In particular:
  `full_hash` is BLAKE3-256 (64 hex), not the spec's xxHash64; `media_nodes.status` is split into
  `indexing_status` × `graph_status` × `lifecycle_state`; edge `status` is `review_state`;
  `DERIVED_FROM_MISSING_PARENT` is the computed `parent_missing` column on
  `v_media_edges_resolved`, not a relationship type; events are SSE, not WebSockets. Values that
  *did* survive as binding contract: pHash Hamming ≤ 10, capture timestamp ± 2s, confidence
  < 0.85 → review, tier confidence bands 1.00 / 0.90–0.99 / 0.70–0.89.
- Any change to `internal/db/migrations/*.sql` or `internal/db/queries/*.sql` must run
  `sqlc generate` and commit `internal/db/sqlcgen/` in the same PR — CI has no codegen step.
- Any change to a route DTO must hand-update `web/src/api/types.ts` and `client.ts` — there is no
  generated client yet.
- Recursive CTEs must alias every column in the anchor `SELECT` or sqlc fails with
  `*ast.ResTarget has nil name`.
- All filesystem writes go through `storage.Guard` and must never target Tier 3.
- Branch off `origin/main`; one issue = one PR.

## Phases

| Phase | Goal | Depends on |
|---|---|---|
| 0 | Groundwork: labels, milestones, this document | — |
| 1 | Close the wiring gaps; fix the latent MISSING bug | 0 |
| 2 | Authorization — group checks on mutating routes and the OpenAPI surface | — |
| 3 | Perceptual hashing and the promoted camera fields Tier 3 needs | 1 |
| 4 | Tier-3 heuristic resolver (serial + lens + ±2s + Hamming ≤ 10) | 3 |
| 5 | Tier-1 project introspection: `.dam.json`, `.drp`, `.fcpxml`/`.edl` | 1 |
| 6 | Multi-hop graph traversal and the spec's remaining UI surfaces | 4, 5 |
| 7 | Delivery: EXIF/XMP inheritance, `remote_sync_state`, Immich push | 3 |
| 8 | Agent-server contract: `event_queue` drain, handshake, path rebase | 1 |
| 9 | Cache pruning engine and the differential mtime sweeper | 1, 7 |
| 10 | Workstation agent — placeholder tracking issue, decomposed later | 8 |

Phase 2 has no dependencies and can land at any point — it closes a live authorization gap
(`Principal.Groups` is parsed and echoed at `/api/v1/me` but nothing checks it today) rather than
adding a feature. It should land before phase 9 is promoted, since phase 9 is the first
production caller of `storage.Guard.Remove` and an unauthenticated purge endpoint is the worst
ordering accident available in this codebase.

Google Photos push is filed as a single research spike (phase 7), not an implementation phase —
third-party access to the Google Photos API is restricted enough that feasibility needs
confirming before any code is written. The workstation agent (phase 10) is filed as one
placeholder tracking issue; its repo location (separate repo vs. a subdirectory here) is an open
decision to make once phase 8 shows what the agent actually needs to talk to.

## Notable per-phase decisions

- **Phase 4 opens with a design issue, not implementation.** `internal/graph/engine.go` hardcodes
  a single `autoAcceptThreshold = 0.90` across every resolver tier, but the spec's stated global
  rule ("confidence < 0.85 → review") and its own Tier-3 band (0.70–0.89) are in tension: read
  literally, a Tier-3 candidate at 0.86 should not need review, yet under today's threshold it
  always will, since 0.89 is Tier 3's ceiling. The first phase-4 issue decides this deliberately
  (a per-tier threshold, with Tier 1/2 held at 0.90 so no existing edge's classification moves)
  before the resolver that would otherwise quietly inherit the ambiguity is built.
- **Phase 5 opens with a path-mapping design issue, not a parser.** `.drp`, `.fcpxml`, and `.edl`
  reference media by the host path the editing workstation saw, not the container path the
  server sees. Whether Tier-1 introspection is four easy parsers or a genuinely hard matching
  problem depends entirely on how that gap is closed, and that decision is made once, in writing,
  before any parser exists.
- **Perceptual hashing needs a decode step the codebase doesn't have.** `hashing.PerceptualHash`
  takes a decoded `image.Image`; Go's stdlib has no ARW/CR3/NEF decoder, so hashing a camera
  master requires extracting its embedded JPEG preview via `exiftool -b -PreviewImage` first —
  phase 3 builds that, not phase 4.

## Cross-cutting hazards for concurrent work

Every issue starts with `Manual: true`, so nothing auto-claims — these only matter once a human
promotes two issues to `ready` at the same time:

- Issues touching `internal/db/queries/*.sql` all regenerate `internal/db/sqlcgen/` wholesale via
  `sqlc generate`; never promote two at once, regardless of which phase they're in.
- Migrations are numbered up front to avoid two agents both claiming `00002_*.sql`; a new
  migration issue not already accounted for here claims the next free number and says so in its
  own body.
- `internal/pipeline/scan.go`'s `processFile` and `internal/httpapi/routes.go`'s
  `registerRoutes` are both edited by several issues across different phases; keep at most one
  unmarked at a time per function.
- New project-file parsers (phase 5) must self-register via `init()` in their own file rather
  than a shared registry list, specifically so they can be worked concurrently without
  conflicting.

## Issue tracking

Each phase has a tracking issue on this repo's issue board with the per-PR work filed as its
sub-issues, and native GitHub `blocked_by` dependency edges matching this document's ordering.
`docs/schema.md` remains the authority on the increment-1 schema decisions; this document is the
authority on what ships after it.
