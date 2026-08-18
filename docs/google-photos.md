# Google Photos: API feasibility spike

This document is the answer to issue [#56](https://github.com/s3ntin3l8/branchdam/issues/56), a
research spike against [`docs/spec/original-spec.md:193`](spec/original-spec.md)'s one-liner —
*"API Ingest: Uses `mediaItems:batchCreate` with OAuth2 refresh tokens."* Google cut the Photos
Library API down substantially on 2025-04-01, so that line needed re-checking against the API as
it exists today before Phase 7 (see [`docs/roadmap.md`](roadmap.md)) budgets any implementation
work against it. No code changes accompany this document; the `GOOGLE_PHOTOS` value in
`remote_sync_state.remote`'s `CHECK` constraint already exists in
[`00001_init.sql`](../internal/db/migrations/00001_init.sql) and stays unused pending this
outcome.

**Verified as of 2026-08-18.** The Photos APIs' own release notes show no changes in the 16
months since the 2025-04-01 cut, so this should be a stable read — but re-check the [release
notes](https://developers.google.com/photos/support/release-notes) before relying on it much
later than that date.

## What changed on 2025-04-01

Per [Updates to the Google Photos
APIs](https://developers.google.com/photos/support/updates) and the [release
notes](https://developers.google.com/photos/support/release-notes):

| Scope | Status |
|---|---|
| `photoslibrary.readonly` | **Removed.** Calls now return `403 PERMISSION_DENIED`. |
| `photoslibrary.sharing` | **Removed.** Same. |
| `photoslibrary` (broad) | **Removed.** Same. |
| `photoslibrary.appendonly` | Survives. Write-only: upload, create media items/albums, add enrichments. |
| `photoslibrary.readonly.appcreateddata` | Survives. Read-only, scoped to the calling app's own media items and albums. |
| `photoslibrary.edit.appcreateddata` | Survives. Edit, scoped the same way. |

The Library API's own framing now: *"Developers can only list, search, and retrieve albums and
media items that were created by your app."* Album-sharing methods (`albums.share`,
`albums.unshare`, `sharedAlbums`) are gone outright. Google's stated migration path for photo
*selection* UI is the new [Picker
API](https://developers.google.com/photos/picker/guides/get-started-picker), which is unrelated
to the ingest path this spike is about.

## Feasibility by direction

The spec calls `batchCreate` "API Ingest," but relative to branchDAM it is a **push**
(branchDAM → Google Photos). That distinction is what makes the restrictions legible — they hit
the three directions very differently:

| Direction | Verdict |
|---|---|
| **Push** (branchDAM uploads a rendered child asset) via `appendonly` + `batchCreate` | **Works.** Survived the 2025-04-01 cull intact. |
| **Read back what branchDAM pushed** via `readonly.appcreateddata` | Works, but lossy — see [Collisions with branchDAM's invariants](#collisions-with-branchdams-invariants) below. |
| **Import a user's existing Google Photos library** | **Dead.** The [Picker API](https://developers.google.com/photos/picker/guides/get-started-picker) requires an interactive user selecting photos inside the Google Photos app — it cannot be automated headlessly. Google Takeout is the only bulk-export path, and it is a manual, out-of-band one-time export, not an API. |

Only the push direction was ever in Phase 7's scope, and push is the direction that survived.

## OAuth

Every Photos API app *"must pass the OAuth verification review"* per [Authorization
scopes](https://developers.google.com/photos/overview/authorization). The trap for a
self-hosted/unattended app: a Cloud project with a publishing status of **Testing** issues
refresh tokens that expire in **7 days** — quoting [OAuth 2.0
Policies](https://developers.google.com/identity/protocols/oauth2/policies) — *"a publishing
status of 'Testing' is issued a refresh token expiring in 7 days."* That's fatal for an
unattended daemon: someone has to re-consent through a browser every week.

### The workaround

The 7-day rule is scoped to **publishing status**, not verification status. So for a plain
consumer Google account (no Workspace domain):

1. Create your own OAuth client in your own Google Cloud project, with the Photos Library API
   enabled, requesting `photoslibrary.appendonly`.
2. In the OAuth consent screen settings, click **Publish app → In production**, without
   completing Google's verification review.
3. Consent once through the resulting warning screen.

Google's own docs confirm step 3 is not a hard wall: [Authorization
scopes](https://developers.google.com/photos/overview/authorization) describes it as *"clicking
the Advanced option and then clicking Go to Project Name (unsafe)"* — a bypassable interstitial,
not the `Access blocked: this app's request is invalid` refusal some other Google APIs return
for unverified restricted scopes. That distinction matters: if Photos scopes triggered the hard
block, this workaround would be dead on arrival.

Once published (even unverified), refresh tokens behave normally: indefinite lifetime as long as
they're used at least once every 6 months, up to 100 per Google Account per client ID, until
revoked. The penalty for staying unverified in production is a lifetime cap of **100 new users**
ever consenting to that project — irrelevant for a single self-hosted user.

> ⚠️ **This step is inferred, not confirmed.** The 7-day expiry rule is documented only for
> Testing status; Google never states outright that production-unverified escapes it, and no
> third-party tool's issue tracker was found confirming it specifically for Photos scopes (see
> [Corroboration](#corroboration-and-its-limits) below). Treat it as untested until the empirical
> check below has run. **A follow-up issue tracks running that check** — see
> [Follow-up](#follow-up) below.
>
> Empirical test: authorize once under the production-unverified client, record the refresh
> token and consent timestamp, wait 8+ days, then attempt a token refresh. Pass = normal refresh
> response. Fail = `invalid_grant`.

### Corroboration and its limits

[rclone's Google Photos backend](https://rclone.org/googlephotos/) corroborates the narrower,
already-settled claim: it still uploads to Photos post-cull, using exactly the three surviving
scopes, and as of 2026 requires every user to create their own `client_id` (rclone's previously
shared one is being retired during 2026) — the same personal-client-ID posture this workaround
depends on. But rclone's docs say nothing about publishing status or refresh-token lifetime, so
they do **not** corroborate the production-unverified-escapes-7-days claim specifically. Don't
cite rclone for that part.

### Operational trap

Resources created under one OAuth client ID are pinned to it: per [Authorization
scopes](https://developers.google.com/photos/overview/authorization), *"resources created with a
specific client ID can only be accessed or modified using the same ID."* Rotating the client
orphans every previously uploaded asset — `readonly.appcreateddata` calls under a new client ID
simply won't see them anymore. The client ID becomes durable state on par with the database
itself; losing or rotating it is effectively data loss for the sync relationship.

### Alternatives considered and dismissed

- **Workspace "Internal" user type.** No verification requirement and no 7-day expiry — but
  requires a Google Workspace / Cloud Identity domain, and the target library would then be the
  Workspace user's Photos library, not a personal one. Not applicable here (plain gmail.com
  account, confirmed).
- **Service accounts.** [Authorization
  scopes](https://developers.google.com/photos/overview/authorization) states outright that
  *"service accounts are not supported"* for Photos API authentication.
- **Full OAuth verification.** Public homepage + privacy policy on a Search-Console-verified
  domain + an English demo video of the consent flow, taking up to ~10 days per [Sensitive scope
  verification](https://developers.google.com/identity/protocols/oauth2/production-readiness/sensitive-scope-verification).
  Would also need to be redone by *every* self-hoster individually, since an open-source binary
  can't ship a confidential client secret that all deployments share.

## Quota

Per the [Library API
limits](https://developers.google.com/photos/library/guides/api-limits-quotas):

- **10,000 requests/project/day** — covers uploads, listing, filtering.
- **75,000 media-byte requests/project/day** — the raw-byte upload step specifically.

Upload is two calls per file: one raw-byte `POST` to the uploads endpoint, then a
`mediaItems.batchCreate` call covering up to 50 items at once. Worked out: 50 photos = 50
byte-upload calls + 1 `batchCreate` call. Byte-uploads are the binding constraint, capping
throughput at **~75,000 photos/day**, and that only consumes ~1,500 of the 10,000-request daily
budget. Quota clears any plausible branchDAM ingest rate by roughly three orders of magnitude —
it is not the constraint on this feature.

Size limits: photos up to 200 MB, video up to 20 GB; files over 50 MB are noted as *"prone to
performance issues."* Uploads are stored at full original quality and count against the
uploading account's Google One storage — but note the scope here: per [spec
line 120](spec/original-spec.md), the pipeline would push *rendered child/derivative* assets to
Immich/Google Photos, never Tier 3 masters, so the storage cost is bounded by derivative size,
not by the archive.

## Collisions with branchDAM's invariants

1. **`full_hash` cannot round-trip.** BLAKE3-256 (`full_hash`) is this repo's stated integrity
   oracle (see the root [`CLAUDE.md`](../CLAUDE.md)). The only way to read bytes back from
   Google Photos is a `baseUrl` fetch with the `=d` download parameter, and [Access media
   items](https://developers.google.com/photos/library/guides/access-media-items) states that
   returns the image *"retaining all the Exif metadata except the location metadata."* GPS is
   stripped by design, so downloaded bytes can never re-hash to the originally stored
   `full_hash`. There is no verification story for a Google Photos copy — it functions as a
   delivery endpoint, not a synced, checksum-verifiable replica. `baseUrl`s also expire after 60
   minutes, so they can't be cached as stable references either.

2. **`sync.PushFunc`'s shape can't carry an upload.**
   [`internal/sync/manager.go`](../internal/sync/manager.go) is deliberately outbound-HTTP-free:
   `type PushFunc func(ctx context.Context, nodes []Node) error`. Two problems for a
   byte-transferring provider specifically:
   - The batch builder (`manager.go:140-143`) constructs `nodes[i] = Node{ID: id}` — `Checksum`
     is left empty and there is no file path at all. A provider that needs to upload bytes gets
     an ID and nothing else to work with.
   - `PushFunc` returns only `error`, with no per-node result. `setBatchStatus` is only ever
     called with `remoteAssetID: ""`, so **`remote_asset_id` is never populated in production**
     today — despite [spec line 194](spec/original-spec.md) wanting it recorded specifically to
     block duplicate uploads. Batch status is also all-or-nothing: one failure fails every node
     in the batch.

   This is a real, already-existing gap independent of Google Photos, but it doesn't block
   anything today: Immich's push (`POST /libraries/{id}/scan`, see
   [#55](https://github.com/s3ntin3l8/branchdam/issues/55)) returns no per-asset ID either, so
   the current `error`-only shape is sufficient for it. It would only start to matter for a
   provider that actually transfers bytes.

3. **`storage.Guard` correctly has no cloud concept, and shouldn't grow one.**
   [`internal/storage/guard.go`](../internal/storage/guard.go) is an `os`-only abstraction:
   `LoadGuard` hard-fails `filepath.EvalSymlinks` on every configured root, and the
   `storage_locations.tier` `CHECK` enumerates five local tiers with no cloud value. That's
   correct as-is — `GOOGLE_PHOTOS` belongs exactly where it already lives, as a
   `remote_sync_state.remote` value, never as a `storage_locations` row.

4. **The existing "push" transfers no bytes; Google Photos would be the first that does.**
   Immich's integration works because rendered exports land in a Tier-2 export directory on a
   filesystem mount Immich also reads, and the "push" is one HTTP call telling Immich to rescan
   it — see the spec's *"Renders written to `/storage/exports/immich/` are indexed natively by
   Immich without duplicating bytes."* `remote_sync_state` today tracks a *notification*, not a
   *transfer*. The repo currently has zero outbound HTTP client code, no `golang.org/x/oauth2`
   dependency, and no token storage of any kind anywhere — the only secret-handling precedent is
   the *inbound* agent `X-API-Key` in
   [`internal/config/config.go:73-79`](../internal/config/config.go). All of that would need to
   be built new to support Google Photos.

## Recommendation: no-go for Phase 7

The interesting finding here inverts the roadmap's stated premise. **Google's own restrictions
are not what blocks this feature**: push survived the 2025-04-01 cull intact, quota clears any
plausible ingest rate by roughly three orders of magnitude, and the OAuth problem has a workable
single-user path (pending the empirical 8-day confirmation below).

The actual blocker is branchDAM's own current shape. Google Photos would be the first remote
target that moves real bytes, and reaching it needs an OAuth2 token store, a refresh loop, an
outbound HTTP client, resumable upload, and a `PushFunc` signature change — none of which exist
today — in order to land on a destination that **cannot be integrity-verified** against
`full_hash`, whose accessibility is permanently pinned to one OAuth client ID, and that can never
be read back with GPS metadata intact.

**Sequencing:** land Immich ([#55](https://github.com/s3ntin3l8/branchdam/issues/55)) first, and
let the sync layer grow real byte-transfer and per-node-result capability there, driven by actual
need rather than speculatively for Google Photos. Revisit Google Photos only once **all three**
of these hold:

1. `sync.PushFunc` carries a file path in and returns a per-node result out.
2. The 8-day refresh-token test (below) has passed.
3. Someone actually wants a Google Photos delivery target.

## Follow-up

One follow-up issue is filed for the 8-day refresh-token test specifically — it's the single
fact that would flip this verdict, costs about 20 minutes of console work, and has irreducible
multi-day latency, so it's worth starting regardless of when (or whether) Google Photos work
resumes. No implementation issue is filed: per
[#26](https://github.com/s3ntin3l8/branchdam/issues/26)'s own tracking-issue body, this spike was
always scoped as *"research only, no implementation issue filed,"* and a no-go verdict shouldn't
change that.
