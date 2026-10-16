# Agent REST API Reference

This document is the wire-format reference for branchDAM's `/api/v1/agent/*` endpoints. It
documents the **normative** REST DTOs (`AgentHandshakeInput`, `AgentEventInput`, etc.) and the
behavioral caveats that aren't obvious from the code — advisory fields the server ignores,
idempotency gaps, the Tier-3 rebase exemption. The handlers themselves live in
`internal/httpapi/routes.go` and the type definitions in `internal/agent/types.go`.

For the workstation agent ecosystem overview (offline queue, dual-copy ingest, scan-trigger),
see [`integrations.md` §5](integrations.md#5-workstation-companion-agent). For the mobile
companion app that shares this wire format, see [`mobile.md`](mobile.md).

---

## Request signing (`agent.signedRequests`)

Every `/api/v1/agent/*` request authenticates with the device's paired API key in `X-API-Key`.
When `agent.signedRequests` is `true`, each request must also carry:

| Header | Value |
|---|---|
| `X-Timestamp` | Request time as Unix **nanoseconds**, base 10. Must be within `agent.replayWindowSecs` (default 300 s) of server time. |
| `X-Nonce` | A fresh random value per request (the clients send 16 random bytes, hex). A nonce seen again inside the replay window is rejected. |
| `X-Signature` | Lowercase hex HMAC-SHA256, as below. |

The signature is:

```
key       = the raw API key bytes (the exact X-API-Key value)
canonical = METHOD + "\n" + requestURI + "\n" + nonce + "\n" + timestamp + "\n"
signature = hex(HMAC-SHA256(key, canonical || body))
```

`requestURI` is the path plus `?` and the raw query string exactly as sent (for example
`/api/v1/agent/check-content?fastHash=0123456789abcdef`), and `body` is the raw request body
bytes (empty for a GET or a body-less POST). This is byte-for-byte what `branchdam-agent`'s and
`branchdam-mobile`'s `Client.signRequest` compute; the server's side is
`internal/auth.SigningKeyForAPIKey` plus `AgentChainWithConfig`.

Earlier servers verified against `HMAC-SHA256(pepper, "sign:" + apiKey)`, which no client can
compute, so turning `signedRequests` on rejected every device. Servers from this fix on verify
with the raw key. Keying with the raw API key exposes nothing new: the key is already sent in
`X-API-Key` on the same request, and the signature protects the request against replay and
tampering, not the key's secrecy (use TLS for that).

Paths in `agent.skipSignaturePaths` (default: `/api/v1/agent/upload`, whose body is the binary
stream) skip the signature check; the API key check always runs. A missing, stale, replayed or
mismatched signature returns `401 invalid or missing signature` (`AUTH_INVALID_SIGNATURE`); a signed
body over `agent.signedMaxBodyBytes` returns `413` (`PAYLOAD_TOO_LARGE`).

---

## Errors

Every error from an `/api/v1/agent/*` route -- the handlers, the agent auth middleware in front of
them, and the raw `POST /api/v1/agent/upload` stream -- is one envelope: an RFC 9457
`application/problem+json` body (Huma's error model) plus a stable, machine-readable `code`:

```json
{
  "title": "Conflict",
  "status": 409,
  "detail": "node already has a different fullHash",
  "code": "HASH_CONFLICT"
}
```

Classify a failure by `code` (and HTTP status) only. `detail` is human-readable text whose wording
may change; `errors[]` may carry extra per-field detail. Upload error bodies additionally keep the
legacy `"error"` field (same text as `detail`) for clients written before the envelope. HTTP status
codes are unchanged from before the envelope existed. Codes are append-only: a published code never
changes meaning. A client that receives a code it doesn't know should fall back to the HTTP status.
The constants live in `internal/apierr`.

| Code | Status | Raised when | Retry? |
|---|---|---|---|
| `AUTH_NOT_CONFIGURED` | 503 | The server has no device-key lookup (Companion Pairing) wired; no agent request can authenticate. | Yes, later (server-side fix) |
| `AUTH_INVALID_KEY` | 401 | `X-API-Key` missing, unknown or revoked. | No -- re-pair the device |
| `AUTH_INVALID_SIGNATURE` | 401 | Signed requests are on and `X-Timestamp`/`X-Nonce`/`X-Signature` is missing, malformed, outside the replay window, replayed or wrong. | Yes, with a fresh nonce/timestamp (check the clock) |
| `AUTH_FORBIDDEN` | 403 | Authenticated, but not as a paired agent device. | No |
| `AGENT_ID_MISMATCH` | 403 | Body `agentId` is not the authenticated device's agent id. | No |
| `NOT_NODE_OWNER` | 403 | The node exists but was not created by the calling device. | No |
| `VALIDATION_FAILED` | 400, 422 | Malformed body, schema violation or an invalid field (UUID, hash, missing value, bad filename, ...). | No |
| `PATH_OUTSIDE_STORAGE` | 400, 409 | A path does not resolve to a configured storage location or escapes one (409 on resolve-snapshot: timeline path not in virtual storage). | No |
| `READ_ONLY_TARGET` | 400, 403 | The target location is read-only (400 on rebase, 403 on upload). | No |
| `ARCHIVE_FILE_NOT_PRESENT` | 400 | A Tier-3 rebase target has no file yet. | Yes, once the archive copy has finished |
| `CHECKSUM_MISMATCH` | 400 | Uploaded bytes don't match `X-Blake3-Hash`. | Yes, re-send the bytes |
| `NODE_NOT_FOUND` | 404 | No live node with that UUID (an `ARCHIVED` superseded version or a `TRASHED` node included). | No |
| `HASH_CONFLICT` | 409 | The node already has a different `fullHash`, or (rebase of an unknown `nodeUuid`) another live node already holds that content; the body then also carries `existingNodeUuid`. | No -- adopt `existingNodeUuid` |
| `EVENT_UUID_CONFLICT` | 409 | `eventUuid` was already used with a different `eventType`. | No |
| `SNAPSHOT_CONFLICT` | 409 | A Resolve snapshot conflicts with existing graph state; nothing changed. | No (fix the snapshot) |
| `PAYLOAD_TOO_LARGE` | 413 | Body over the signed-body limit, a Resolve snapshot over 10 000 entries, or an upload over 50 GB. | No |
| `STORAGE_NOT_CONFIGURED` | 409, 500, 503 | The server lacks storage configuration the request needs (no storage guard, no virtual location, no writable location). | Yes, later (server-side fix) |
| `INTERNAL_ERROR` | 500 | Unexpected server-side failure (database, key lookup, a failed stat of a rebase target, ...). | Yes |
| `SERVICE_UNAVAILABLE`, `FORBIDDEN`, `NOT_FOUND`, `CONFLICT`, `ERROR` | 503, 403, 404, 409, other | Fallbacks for an error raised without a specific code (e.g. by the HTTP framework itself). | By status |

Event processing errors (§2) are not HTTP errors: `POST /api/v1/agent/events` answers `202` and the
outcome lands in `event_queue.error_log`.

---

## 1. Handshake (`POST /api/v1/agent/handshake`)

**Request (`AgentHandshakeInput`):**
```json
{
  "agentId": "workstation-macbook-01",
  "clientVersion": "0.1.0",
  "lastProcessedEventUuid": "018f2345-6789-7abc-def0-123456789abc",
  "currentKeyId": 3
}
```

`agentId` is required and must equal the agent id of the paired device that authenticated the
request (`403 agent id mismatch` otherwise). `currentKeyId` is optional: when supplied and a newer
active key exists for the device, the response carries `pendingRotation`.

**Response (`AgentHandshakeOutput`):**
```json
{
  "ok": true,
  "serverVersion": "dev",
  "serverTimeUnix": 1723985000,
  "acknowledgedEventUuid": "018f2345-6789-7abc-def0-123456789abc",
  "pendingEventsCount": 0,
  "namingTemplate": "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}",
  "pendingRotation": {"keyId": 4, "apiKey": "", "previousKeyExpiresAtUnix": 0}
}
```

`namingTemplate` is the effective `ingest.namingTemplate`. `pendingRotation` is omitted unless the
caller's `currentKeyId` is not the newest active key; it currently carries only `keyId` (the
plaintext `apiKey` is blank -- the device must re-pair), so treat it as a hint.

**Not a resume mechanism, despite the request fields' names.** `lastProcessedEventUuid` and
`clientVersion` are accepted and parsed but never read by the handler
(`internal/httpapi/routes.go`'s `handleAgentHandshake`) -- they have no effect on the response or
on server state. `pendingEventsCount` is a **server-global** PENDING count, not scoped to the
calling agent. `acknowledgedEventUuid` only ever names the agent's most recent `PROCESSED` row --
a `FAILED` row is silently skipped, so its presence does not mean "everything up to here
succeeded."

---

## 2. Event Submission (`POST /api/v1/agent/events`)

**Request (`AgentEventInput`):**
```json
{
  "eventUuid": "018f2346-789a-7bcd-ef01-23456789abcd",
  "agentId": "workstation-macbook-01",
  "eventType": "EVENT_NODE_CREATED",
  "payload": "{\"nodeUuid\":\"018f...\",\"filePath\":\"/storage/staging/clip.mov\",\"fastHash\":\"a1b2c3d4e5f60718\"}"
}
```

**Response (`AgentEventOutput` - Status 202 Accepted):**
```json
{
  "eventId": "018f2346-789a-7bcd-ef01-23456789abcd"
}
```

**Transport-level Idempotency:** Clients may supply a client-minted `eventUuid` (UUIDv7 recommended).
When `eventUuid` is provided, `POST /api/v1/agent/events` performs duplicate detection backed by the
`event_queue.event_uuid` unique constraint. If an event with the same `eventUuid` was already accepted,
the endpoint returns `202 Accepted` with the existing `eventId` without enqueuing a duplicate row.
If `eventUuid` is omitted (legacy callers), the server mints a new UUIDv7.

**Processing and retries.** `internal/agent.Drainer` applies queued events asynchronously,
oldest first. An event that fails with a *fatal* error (malformed payload, invalid identifier,
ownership refusal, cycle, read-only target, ...) is marked `FAILED` on the first attempt. Any
other failure is *retryable*: the row stays `PENDING` and is deferred via
`event_queue.next_attempt_at` by an exponential backoff -- 5s after the first failed attempt,
doubling per attempt, capped at 1 hour -- and is marked `FAILED` only after 20 attempts (roughly
ten hours). A deferred row does not block the events behind it. This is what lets a Tier-3
`EVENT_PATH_REBASED`/`EVENT_NODE_MOVED` sent while the agent is still copying the file into the
archive (`ErrArchiveFileNotYetPresent`) succeed once the copy lands, instead of failing within
seconds.

### 2.1. Resolve snapshot (`POST /api/v1/agent/resolve-snapshot`)

This is a synchronous, authenticated machine-agent operation, unlike the
asynchronous event queue. A `200 OK` confirms that all graph changes committed
in one transaction; a failed/incomplete request changes no edges. The body is
ordinary JSON (not the double-encoded `/events` envelope):

```json
{
  "agentId": "workstation-01",
  "scopeId": "<lowercase SHA-256 hex of credential-free database identity>",
  "timelines": [{
    "timelineId": "<Resolve timeline ID>",
    "nodeUuid": "<deterministic virtual UUID>",
    "filePath": "/virtual/resolve/<uuid>",
    "displayName": "Resolve: Master",
    "evidenceJson": {"timelineName": "Master"}
  }],
  "memberships": [{
    "timelineId": "<Resolve timeline ID>",
    "mediaFilePath": "D:\\Videos\\clip.mov",
    "sourceNodeUuid": "<indexed media UUID>",
    "evidenceJson": {
      "timelineId": "<Resolve timeline ID>",
      "mediaFilePath": "D:\\Videos\\clip.mov",
      "mediaFilePaths": ["D:\\Videos\\clip.mov", "D:\\Videos\\CLIP.mov"],
      "placements": [{"itemId": "<Resolve item ID>", "clipName": "clip.mov"}]
    }
  }],
  "legacyTimelineNodeUuids": [],
  "retireScopeId": ""
}
```

An unresolved membership omits `sourceNodeUuid` and `evidenceJson`; it remains
present in the database snapshot and protects its existing edge from removal.
For a previously resolved membership, `evidenceJson.mediaFilePaths` records
all original Resolve path aliases that were merged into the edge. An unresolved
membership protects the edge when its path matches either the primary
`mediaFilePath` or one of those recorded aliases. A path that has never been
recorded in the edge's evidence cannot be associated with that edge while it
is unresolved, so it does not suppress reconciliation of an unrelated removal.
Resolved memberships must be unique by `(timelineId, sourceNodeUuid)`; agents
merge path aliases and repeated placements into one membership. Duplicate
source memberships are rejected before the transaction begins.
The server creates/updates Resolve timeline nodes, creates or refreshes
`PROJECT_SIDECAR` edges owned by `resolve_project_db`, and makes absent
unreviewed edges inactive (`is_active=0`) while retaining rows for audit.
Timeline evidence uses the `resolve_snapshot_evidence` metadata source, kept
separate from legacy queued-event metadata so a draining old event cannot
overwrite the synchronous snapshot slot.
Human-reviewed `CONFIRMED`/`REJECTED` edges are never changed; the response's
`reviewedConflicts` count prompts manual resolution. `legacyTimelineNodeUuids`
claims processed virtual-node events from the same agent during migration.

A successful snapshot may contain no timelines or memberships. This is
authoritative for the agent scope: prior automatic Resolve edges are
deactivated while virtual timeline nodes and human-reviewed edges are retained.
Failures while reading or transporting the snapshot happen before this
reconciliation and must not be represented as an empty snapshot.

They are deliberately reconciled with an empty desired set: current timelines
are already present in `timelines`, while obsolete or old-identity nodes are
retired atomically after their replacement edges are established.
`retireScopeId` deactivates unreviewed edges from a previously synced database
scope after a database switch. At most 10,000 memberships/timelines/legacy IDs
are accepted; oversized snapshots fail closed rather than truncating and
removing edges.

The response fields are `created`, `refreshed`, `removed`, `unchanged`,
`unresolved`, and `reviewedConflicts`. The request's `agentId` must match the
machine principal attached to the paired device that authenticated the
request -- issue #453 PR F removed the legacy env-bootstrap carve-out, so
there is no remaining path where a single credential can drive an arbitrary
`agent_id`. Timeline paths must resolve through a virtual `storage.Guard`
location.

Migration 26 retains inactive edges for audit. Its down migration refuses to
run while any inactive edge exists because the older schema cannot represent
that state; restoring a pre-migration backup is the safe rollback in that case.


---

## 3. Event Payloads (`payload` JSON)

> **`storageLocationId` / `newStorageLocationId` / `targetStorageLocationId` are advisory and
> ignored.** `storage.Guard` exposes no lookup-by-ID, so a payload-supplied location ID is
> fundamentally unverifiable server-side; `storage_location_id` is always re-derived from the
> event's own file path via `storage.Guard.Resolve`. The fields are kept in the wire format so
> an existing agent payload still parses, but the server never trusts them. Similarly,
> `EVENT_EDGE_ATTACHED`'s `reviewState` is advisory and ignored for `AUTO_ACCEPTED`/empty (the
> server always derives `AUTO_ACCEPTED` vs. `NEEDS_REVIEW` from `confidence` and `tier` via the
> same per-tier threshold every other resolver uses, and from node ownership -- see §3.2) and is
> an outright error for `CONFIRMED`/`REJECTED` -- a human review decision is never the agent's to
> make.
>
> **A rebase targeting an `ARCHIVED` or `TRASHED` node_uuid always fails.** `EVENT_NODE_MOVED`,
> `EVENT_PATH_REBASED`, and `POST /api/v1/agent/rebase` all refuse to rebase a node whose
> `lifecycle_state` is `ARCHIVED` or `TRASHED`. An `ARCHIVED` node_uuid identifies a superseded
> version and a `TRASHED` one a deleted asset; a rebase sets the node `ACTIVE`, so it would
> resurrect either one. Restoring from trash is a human action in the web UI.
> `EVENT_NODE_MOVED`/`EVENT_PATH_REBASED` mark the event `FAILED` with `error_log` describing the
> refusal (`node is archived` / `node is trashed`); `POST /api/v1/agent/rebase` returns
> `404 Not Found` for both. An agent holding a stale `node_uuid` should treat either as terminal,
> not retry.
>
> **A device may only rebase, move or delete nodes it created itself.** The server records which
> paired device created each node (via `EVENT_NODE_CREATED`, `EVENT_VIRTUAL_NODE_CREATED`, an
> insert-on-unknown `EVENT_PATH_REBASED`/`POST /api/v1/agent/rebase`, or `POST /api/v1/agent/upload`
> that created a new node). `EVENT_NODE_MOVED`, `EVENT_NODE_DELETED`, `EVENT_PATH_REBASED` (on an
> existing node) and `POST /api/v1/agent/rebase` refuse any other node -- server-scanned nodes, web
> uploads and other devices' nodes: events are marked `FAILED` (not retried) and the HTTP endpoint
> returns `403 Forbidden`. `EVENT_EDGE_ATTACHED` into a child the device did not create is accepted
> but stored `NEEDS_REVIEW` (see §3.2). A content-dedup hit on upload points at somebody else's
> node and is not claimed by the uploader. Nodes created before this rule existed are attributed
> from their processed creation event when it is still in the queue; otherwise no device owns them.
>
> **Content dedup records a UUID alias.** An `EVENT_NODE_CREATED` (or an insert-on-unknown
> `EVENT_PATH_REBASED`) whose `fullHash` already belongs to a live (`ACTIVE`/`HIDDEN`) node does
> not create a second node. The device's `nodeUuid` is recorded instead as an alias of the
> existing node (`node_uuid_aliases`), so the device's follow-up calls naming its own UUID still
> resolve: `EVENT_EDGE_ATTACHED` attaches to the existing node; `EVENT_PATH_REBASED`,
> `EVENT_NODE_MOVED`, `EVENT_NODE_DELETED` and `POST /api/v1/agent/rebase` are acknowledged
> without changing the existing node (it is not the device's -- deleting a duplicate never trashes
> the original); `POST /api/v1/agent/node-status` reports the existing node's state. Only the
> device that recorded an alias can resolve it; for any other device the UUID is unknown.
>
> **Identifiers are validated.** `nodeUuid` must be a canonical UUID and `fullHash` 64 lowercase
> hex characters (`400 Bad Request` on the HTTP endpoint, a permanent event failure otherwise). A
> node's existing `full_hash` is never replaced by a different value (`409 Conflict` /
> permanent event failure); repeating the same value is accepted.

### 3.1. `EVENT_NODE_CREATED`

`internal/agent/types.go`'s `NodeCreatedPayload` is normative -- 19 fields in total (excluding
the deprecated, ignored `storageLocationId`). Every field past `fastHash` is optional (a normal
scan-side probe failure or a client that can't compute one simply omits it), but omitting all of
them just means an agent-ingested master carries none of the promoted metadata a scan would
have given it.

```json
{
  "nodeUuid": "018f...",
  "filePath": "/storage/staging/raw_001.arw",
  "fileName": "raw_001.arw",
  "fileExt": ".arw",
  "sizeBytes": 48291040,
  "mtimeUnix": 1723985000,
  "fastHash": "0123456789abcdef",
  "fullHash": "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a",
  "phash": 1152921504606846975,
  "cameraModel": "ILCE-7RM5",
  "cameraSerial": "4401923",
  "lensModel": "FE 24-70mm F2.8 GM II",
  "capturedAtUnix": 1723984900,
  "originalDocumentId": "xmp.did:018f2345-original",
  "documentId": "xmp.did:018f2345-current",
  "derivedFromId": "xmp.did:018f2345-parent",
  "filenameStem": "raw_001",
  "gpsLatitude": 48.858222,
  "gpsLongitude": 2.2945
}
```

### 3.2. `EVENT_EDGE_ATTACHED`

`sourceNodeUuid` is the parent and `targetNodeUuid` the child. Edges drive `graph_status`, metadata
inheritance and the Tier-3 ancestor walk that authorises prune, so the server holds a device to
these rules (issue #504). Every violation marks the event `FAILED` without retry:

- **UUIDs only.** Both nodes are named by canonical UUID. `sourceNodeId`/`targetNodeId` (integer
  row IDs) are refused if present at all, even next to valid UUIDs.
- **`relationshipType`** must be one of `DERIVED_FROM`, `FINAL_EXPORT`, `PROXY_OF`,
  `PROJECT_SIDECAR`, `DUPLICATE_OF`. Empty defaults to `DERIVED_FROM`.
- **`tier`** is clamped to 1 or 2 (0 or absent means 1, anything above 2 is stored as 2). Tier 3's
  lower auto-accept threshold belongs to the server's own heuristics.
- **`confidence`** is required, in `(0, 1]`, and must be `>=` the `needsReviewFloor` (0.50).

How the edge is stored depends on who created the **target (child)** node:

- **The device created the target:** `reviewState` is `AUTO_ACCEPTED` when `confidence` reaches
  the tier's threshold (0.90 for tiers 1 and 2) and `NEEDS_REVIEW` below it, whoever created the
  source. An agent-created derived file pointing at a server-scanned master works as before.
- **The device did not create the target** (a server-scanned file, a web upload, another device's
  node): the edge is still recorded, but always `NEEDS_REVIEW`, whatever its confidence. It shows
  up in the review queue and authorises nothing -- no prune (a `NEEDS_REVIEW` edge never
  authorises one), no metadata inheritance -- until a human confirms it.

An edge that already exists is never overwritten, so a human `CONFIRMED`/`REJECTED` decision
always stands.

```json
{
  "sourceNodeUuid": "018f...-parent",
  "targetNodeUuid": "018f...-child",
  "relationshipType": "DERIVED_FROM",
  "confidence": 0.95,
  "tier": 2,
  "resolver": "xmp",
  "evidenceJson": {"xmpDocumentId": "xmp.did:12345"}
}
```

### 3.3. `EVENT_NODE_MOVED`

```json
{
  "nodeUuid": "018f...",
  "newFilePath": "/storage/staging/renamed_clip.mov",
  "newFileName": "renamed_clip.mov",
  "mtimeUnix": 1723985100
}
```

### 3.4. `EVENT_NODE_DELETED`

Processing `EVENT_NODE_DELETED` triggers a safe, multi-step soft delete:
- Sets `media_nodes.lifecycle_state = 'TRASHED'` (user-initiated; `MISSING` is reserved for
  scan-detected disappearance) and purges `remote_sync_state`. Only the device that created the
  node may do this.
- Safely relocates the master file to `.trash/<rel_path-stem>.<node-uuid><ext>` in the storage
  location (the full node UUID keeps two nodes that trashed the same path distinct; copies
  trashed before this naming still restore), retaining it for 30 days (`trash.retentionDays`,
  counted from the trash time recorded in `node_trash_times`, not from the file's mtime)
  before automated prune unlinks it.
- Purges linked Tier 2 Immich exports immediately (moving export files into `.trash/` and deleting
  sync state) and triggers an Immich library rescan so the asset disappears from galleries right away.
  Only exports linked by an `AUTO_ACCEPTED` or `CONFIRMED` `FINAL_EXPORT` / `immich_export` edge are
  purged; a `NEEDS_REVIEW` or `REJECTED` link is never followed.

```json
{
  "nodeUuid": "018f..."
}
```

### 3.5. `EVENT_PATH_REBASED`

```json
{
  "nodeUuid": "018f...",
  "targetFilePath": "/storage/exports/final_cut.mov",
  "targetFileName": "final_cut.mov",
  "mtimeUnix": 1723985200
}
```

### 3.6. `EVENT_VIRTUAL_NODE_CREATED`

Creates a virtual media node representing an integration project timeline (Resolve, Premiere, FCPXML). The node's `file_path` uses a conventional absolute prefix under the `resolve-virtual` storage location; Guard resolves it lexically without filesystem I/O.

**Payload fields:**

| Field | Type | Required | Description |
|---|---|---|---|
| `nodeUuid` | string | yes | Deterministic UUID (SHA256 of agentId + timelineName + databaseURL) |
| `filePath` | string | yes | Must resolve to a `virtual` storage location. Agent convention: `/virtual/resolve/<agentID>/<sanitized-name>-<8-hex hash>` (e.g. `/virtual/resolve/workstation-01/My%20Documentary-a2bb3478`) |
| `displayName` | string | yes | Human-readable label shown in the asset graph (uses raw timeline name) |
| `projectType` | string | no | One of `resolve_project`, `premiere_project`, `fcpxml_bundle`. Empty accepted for backward compat |
| `evidenceJson` | string | no | Optional evidence JSON (timeline-level metadata) |

```json
{
  "nodeUuid": "018f...",
  "filePath": "/virtual/resolve/workstation-01/My%20Documentary-a2bb3478",
  "displayName": "Resolve: My Documentary",
  "projectType": "resolve_project",
  "evidenceJson": "{\"schemaMapping\":\"resolve-projectdb-1\",\"databaseUrl\":\"postgresql://...\",\"timelineName\":\"My Documentary\"}"
}
```

**Fatal errors (no retry):**
- `ErrVirtualPathNotVirtual`: `filePath` resolves to a non-virtual storage location
- `ErrCrossAgentCollision`: `filePath` already exists for a different agent
- `ErrMalformedPayload`: missing `nodeUuid` or `filePath`

---

## 4. Direct Streaming Upload (`POST /api/v1/agent/upload`)

Mobile companion apps and modern workstation agents stream raw binary media directly to the
server's Master Archive without intermediate staging mounts.

**Headers:**
- `X-Filename`: Original filename (e.g. `PXL_20260829_120000.jpg`).
- `X-Camera-Model`: Optional device model (e.g. `Pixel 9 Pro`).
- `X-Capture-Timestamp`: Optional EXIF capture Unix timestamp.
- `X-Blake3-Hash`: Optional pre-computed 64-character BLAKE3 hex digest for integrity verification.
- `X-API-Key`: Machine API key.

**Request Body:** Raw binary octet stream.

**Response (`AgentUploadResponse` - Status 201 Created, or `200 OK` on a content dedup):**
```json
{
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "status": "UPLOADED",
  "bytesWritten": 48291040,
  "blake3Hash": "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a",
  "relativePath": "2026/2026-08-29_Pixel-9-Pro/PXL_20260829_120000.jpg"
}
```

When the uploaded bytes match an existing node by content, the server stores nothing new and
returns `200 OK` with `status: "DEDUPLICATED"` (and an `X-Dedup: true` header) pointing at the
existing node; that node is not claimed by the uploader. The optional `X-Source-Path-Hash`
header is also read.

---

## 5. Path Rebase Endpoint (`POST /api/v1/agent/rebase`)

> **Rebasing a target inside Tier 3 (`TIER3_MASTER_ARCHIVE`) succeeds if and only if the file
> already exists there.** This is the required `LOCAL_STAGING → CENTRAL_TIER3` scenario,
> resolved in issue #167: the workstation agent copies the bytes into the archive itself, then
> calls this endpoint (or sends `EVENT_NODE_MOVED`/`EVENT_PATH_REBASED`) purely to update
> `media_nodes.file_path`/`storage_location_id`. This endpoint never performs the copy and never
> writes to the archive -- the existence check is a stat (`storage.Guard.Exists`), never a write.
> (Tier 3 itself is writable by default for server-governed ingest -- `POST /api/v1/agent/upload`
> and the `.trash/` buffer write there -- so this is about what *rebase* does, not a read-only
> archive guarantee.) A Tier 3 target whose file is not yet present is
> refused with `400 Bad Request` (HTTP) / retried with backoff (queue, see §2 "Processing and
> retries"; marked `FAILED` only if the file is still missing after the last attempt), so the
> agent must finish copying before calling the HTTP endpoint. Any other read-only tier has no such exemption and is
> always refused.

Errors (codes per [Errors](#errors)): `400` for an invalid `nodeUuid`/`fullHash`
(`VALIDATION_FAILED`), a target path outside storage (`PATH_OUTSIDE_STORAGE`), a read-only target
(`READ_ONLY_TARGET`) or a Tier-3 target whose file is not there yet (`ARCHIVE_FILE_NOT_PRESENT`);
`403` if the node was not created by the calling device (`NOT_NODE_OWNER`); `404` for an `ARCHIVED`
or `TRASHED` node (`NODE_NOT_FOUND`); `409` (`HASH_CONFLICT`) if the node already has a different
`fullHash`, or if an unknown `nodeUuid` would be inserted with a `fullHash` another live node
already holds. That second `409` carries the existing node's UUID in an extra `existingNodeUuid`
field of the envelope:

```json
{
  "title": "Conflict",
  "status": 409,
  "detail": "content already exists as node 018f2345-6789-7abc-def0-123456789abc",
  "code": "HASH_CONFLICT",
  "existingNodeUuid": "018f2345-6789-7abc-def0-123456789abc"
}
```

The response `status` is `REBASED` for an existing node, `CREATED` when an unknown `nodeUuid` was
inserted, or `ALIASED` when `nodeUuid` is this device's alias of an existing node (its
`EVENT_NODE_CREATED` was content-deduplicated, §3): nothing is changed, `id`/`nodeUuid` name the
existing node and `storageLocationId`/`filePath` are empty.

**Request (`AgentRebaseInput`):**
```json
{
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "targetPath": "/storage/exports/render.mov",
  "mtimeUnix": 1723985000,
  "fileName": "render.mov",
  "fileExt": ".mov",
  "sizeBytes": 104857600,
  "fastHash": "fedcba9876543210"
}
```

**Response (`AgentRebaseOutput` - Status 200 OK):**
```json
{
  "id": 42,
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "storageLocationId": 2,
  "filePath": "/storage/exports/render.mov",
  "status": "REBASED"
}
```

---

## 6. Node Status Endpoint (`POST /api/v1/agent/node-status`)

> **The first agent-reachable read endpoint.** Every other `/api/v1/agent/*` route is
> write-oriented (submit an event, record a rebase, or -- for `handshake` -- report watermarks
> about events the agent itself submitted). This one exists so an agent can ask the server what
> it currently knows about a batch of `NodeUUID`s it already has locally, without resolving a
> filesystem path and without touching `storage.Guard` at all -- it is a pure
> `media_nodes`/`storage_locations` read. Added to let `branchdam-agent`'s own `prune` subcommand
> decide whether it's safe to delete its local-edit mirror of a file it already durably archived:
> only once the server reports the node `ACTIVE`/`HIDDEN` and hash-verified.

**Request (`AgentNodeStatusInput`)**, must be non-empty and capped at 400 UUIDs per call (`400 Bad Request` otherwise):
```json
{
  "nodeUuids": ["018f2345-6789-7abc-def0-123456789abc", "018f2345-6789-7abc-def0-123456789abd"]
}
```

**Response (`AgentNodeStatusOutput` - Status 200 OK):**
```json
{
  "statuses": [
    {
      "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
      "found": true,
      "lifecycleState": "ACTIVE",
      "tier": "TIER3_MASTER_ARCHIVE",
      "verified": true,
      "fullHash": "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a"
    },
    {
      "nodeUuid": "018f2345-6789-7abc-def0-123456789abd",
      "found": false,
      "verified": false
    }
  ]
}
```

`verified` mirrors `ListPrunableNodes`' own eligibility predicate exactly: `full_hash` non-NULL,
64 hex characters (BLAKE3-256), and **server-computed** (#505). A `fullHash` a device supplied in
an `EVENT_NODE_CREATED`/`EVENT_PATH_REBASED` event or a `POST /agent/rebase` is only *asserted*:
`verified` stays `false` for it until the server's background re-hash worker (`internal/rehash`)
has re-read the file on the Tier-3 archive and matched it, which happens within minutes of the
node landing there (a hash that does not match never becomes verified). Bytes sent through
`POST /agent/upload` are hashed by the server while streaming and are verified at once. Clients
that gate local deletion on `verified` therefore wait a little longer after an event-based ingest;
nodes created before the upgrade by a paired device re-verify the same way. `fullHash` is the node's stored BLAKE3-256 `full_hash`, so a
client about to delete its local copy can compare it with the hash it computed itself rather than
trusting `verified` alone; it is omitted when the node is not found or has no full hash (added
alongside the existing fields -- older clients simply ignore it). `found: false` is not an error
-- it just means no `media_nodes` row currently has that `node_uuid`.

When a requested UUID is the calling device's alias of an existing node (content dedup, §3), the
entry keeps the requested `nodeUuid`, adds `canonicalNodeUuid` naming the existing node, and
reports that node's `lifecycleState`/`tier`/`verified`.

---

## 7. Telemetry Endpoint (`POST /api/v1/agent/telemetry`)

> **Workstation Scratch & Cache Telemetry.** Allows connected workstation agents to periodically
> report scratch storage health, capacity metrics (breakdown across render caches, ingest mirrors,
> proxies, and free space), and prune run statistics back to the server without exposing
> filesystem paths or mounting drives.

**Request (`AgentTelemetryInput`):**
```json
{
  "agentId": "workstation-macbook-01",
  "clientVersion": "1.1.0",
  "timestampUnix": 1724846400,
  "scratchStorage": {
    "mountPath": "D:\\ResolveScratch",
    "totalBytes": 2000398934016,
    "freeBytes": 450398934016,
    "usedBytes": 1550000000000,
    "mirrorsSizeBytes": 320000000000,
    "renderCacheSizeBytes": 850000000000,
    "proxiesSizeBytes": 280000000000,
    "prunableBytes": 410000000000
  },
  "pruneStats": {
    "lastPruneTimestampUnix": 1724842800,
    "lastReclaimedBytes": 125000000000,
    "lastPruneDurationMs": 3420,
    "prunedItemCounts": {
      "mirrors": 14,
      "renderCacheProjects": 3,
      "proxies": 8
    }
  }
}
```

`agentId` is required and must equal the agent id of the paired device that authenticated the
request (`403 agent id mismatch` otherwise), as on `/handshake` and `/events`: telemetry is stored
per `agentId`, so one device can't overwrite another device's row.

**Response (`AgentTelemetryOutput` - Status 200 OK):**
```json
{
  "ok": true,
  "acknowledgedAtUnix": 1724846401
}
```

*Persistence & Dashboard Integration:* The endpoint persists the latest agent telemetry
snapshot into the `agent_scratch_telemetry` table (upsert keyed on `agent_id`) and broadcasts
an SSE nudge to connected web dashboard clients. Telemetry is queried via
`GET /api/v1/storage-health` (augmented `agents` array) or `GET /api/v1/storage-health/agents`,
surfaced on the Storage Health dashboard with real-time capacity gauges, render cache/mirror
breakdown, prune run metrics, and low space / critical space / stale status alerts, and can be
dismissed via `DELETE /api/v1/storage-health/agents/{agentId}`.

---

## 8. Agent Discovery & Ping (`POST /api/v1/agent/hello`)

A lightweight connectivity and version check endpoint for agent clients. Does not query the database, touch the event queue, or record state.

**Authentication:** Requires an Agent Machine Principal (via Companion API key `Authorization: Bearer <key>`).

**Request:** Empty JSON object (`{}`).

**Response (Status 200 OK):**
```json
{
  "ok": true,
  "version": "v0.20.0"
}
```

---

## 9. Content Verification Pre-Flight (`GET /api/v1/agent/check-content`)

Lets companion apps and workstation agents ask, before copying or uploading a file, whether its
content is already in the library as a live node (`ACTIVE` or `HIDDEN`; `ARCHIVED`, `MISSING` and
`TRASHED` nodes don't count). It has two modes, chosen by which hash is sent.

**Authentication:** Requires an Agent Machine Principal.

**Query Parameters:**
- `fullHash` (optional): 64-character hex BLAKE3-256 digest of the file. When present, the server
  does the definitive lookup and `fastHash`/`sizeBytes` are format-checked but otherwise ignored.
- `fastHash` (optional): 16-character hex xxHash64 `FastHash` (the sampled hash of
  `internal/hashing.FastHash`). Sent **without** `fullHash`, it runs the fast pre-screen.
- `sizeBytes` (optional, `>= 0`): file size in bytes, narrowing the fast pre-screen. `0` or absent
  means "unknown"; a stored node whose `size_bytes` is `0` (an event that omitted the size) is also
  treated as unknown and can still match.

At least one of `fullHash` and `fastHash` is required (`400 Bad Request` otherwise). Hex is
accepted in either case.

### Fast pre-screen (`fastHash` without `fullHash`)

Answers whether a live node *may* have the same content -- a fast-hash hit, and a size hit when
both sides know the size -- using the `ix_media_nodes_fast_hash` index. It never says which node
matched: no `nodeUuid`, `filePath`, `lifecycleState` or `indexingStatus`, because a 64-bit fast
hash is not proof of identical content and the endpoint must not disclose other devices' paths on
the strength of one. `found` is always `false` in this mode.

```json
{
  "found": false,
  "possibleMatch": true
}
```

On `possibleMatch: true` the client computes the full hash (ideally while it streams the file,
so the source is read only once) and confirms with a `fullHash` request. On
`possibleMatch: false` there is no live node with that content and the client can proceed.

### Full-hash lookup (`fullHash` present)

Unchanged from earlier servers apart from the added `possibleMatch` field, which mirrors `found`.

**Response When Found (Status 200 OK):**
```json
{
  "found": true,
  "possibleMatch": true,
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "filePath": "/storage/archive/2026/2026-08-29_Pixel-9-Pro/PXL_20260829_120000.jpg",
  "lifecycleState": "ACTIVE",
  "indexingStatus": "INDEXED_FULL"
}
```

`filePath` is the node's absolute server-side path (`media_nodes.file_path`). `indexingStatus` is
one of `PENDING`, `INDEXED_SHALLOW`, `INDEXED_FULL` or `INDEX_FAILED`.

**Response When Not Found (Status 200 OK):**
```json
{
  "found": false,
  "possibleMatch": false
}
```

---

## 10. Source Path Status Check (`GET /api/v1/agent/source-status`)

Determines whether a local file path on a workstation or mobile device has already been ingested into the library. Relies on the client-supplied `X-Source-Path-Hash` recorded during streaming ingest.

**Authentication:** Requires an Agent Machine Principal.

**Query Parameters:**
- `sourcePathHash` (required): 64-character hex SHA-256 hash of the normalized workstation/device file path.
- `sourcePath`: Alias for `sourcePathHash` (supported for backwards compatibility; if both are specified, they must match).

**Response When Tracked (Status 200 OK):**
```json
{
  "tracked": true,
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "filePath": "/storage/archive/2026/2026-08-29_Pixel-9-Pro/PXL_20260829_120000.jpg",
  "indexingStatus": "INDEXED_FULL",
  "lifecycleState": "ACTIVE"
}
```

**Response When Not Tracked (Status 200 OK):**
```json
{
  "tracked": false
}
```

*Note:* Files ingested prior to migration `00015` or uploaded without the `X-Source-Path-Hash` header have a NULL `source_path_hash` column and will return `tracked: false`. Clients should fall back to `GET /api/v1/agent/check-content` (BLAKE3) when `tracked: false`.

---

## 11. Companion Device Pairing Management

Companion pairing mints per-device API keys for `/api/v1/agent/*` access without sharing user passwords or forward-proxy credentials.

All management endpoints require an authenticated administrator session (`auth.RequireAdmin`).

| Endpoint | Method | Description |
|---|---|---|
| `/api/v1/companion/pairings` | `POST` | Mint a new device pairing. Returns the plaintext API key (shown once) and QR pairing payload. Sets `owner_user_id` to the calling admin. |
| `/api/v1/companion/pairings` | `GET` | List all registered companion device pairings, their enabled state, last seen timestamps, and client names. |
| `/api/v1/companion/pairings/{id}` | `GET` | Retrieve details for a specific companion pairing. |
| `/api/v1/companion/pairings/{id}/rotate` | `POST` | Rotate device API key. Invalidates previous key and returns a new plaintext token. |
| `/api/v1/companion/pairings/{id}/revoke` | `POST` | Revoke device access immediately. Enforces a 401 Unauthorized response on subsequent agent calls. Errors: `404` unknown pairing. |
| `/api/v1/companion/pairings/{id}/rename` | `POST` | Update a pairing's operator-facing `friendlyLabel`. Allowed on revoked pairings too (history rename is harmless). Empty labels are rejected (Huma `minLength` → 422). Audited as `LABEL_RENAMED`. |
| `/api/v1/companion/pairings/{id}/credentials` | `GET` | Re-open the full credential set (QR SVG, pairing URL, parsed API key + preview) for an existing pairing's current key. Requires an authenticated admin session (`requireSettingsAdmin` — same gate as `GET /api/v1/settings`, stricter than the global `auth.RequireAdmin` GET bypass). Each successful read is audited fail-closed as `CREDENTIALS_REVEALED` before the response is served; response is `Cache-Control: private, no-store`. `keyPreview` is always the newest active key's last-4 from the DB (never key material). `apiKey`/`pairingUrl` are empty when the row has no sealed `pairing_url` (keyless server or pre-migration legacy row). `sealingEnabled` is whether a `BRANCHDAM_SECRET_KEY` box is configured on this server *right now* (not row provenance); with an empty `pairingUrl` the SPA uses it as `false` = keyless vs `true` = legacy row — rotate to seal. Errors: `404` unknown pairing; `410` revoked/expired (no active key); `409` sealed under a different `BRANCHDAM_SECRET_KEY`; `500` sealed credentials exist but no key is configured (same detail as `GET qr.svg` when the box is unset). |
| `/api/v1/companion/pairings/{id}` | `DELETE` | Delete device pairing record and its key history. Requires the pairing to be revoked first (`POST /revoke`), returning `409 Conflict` otherwise. |
| `/api/v1/companion/pairings/{id}/audit` | `GET` | Retrieve audit events (creation, key rotation, revocation, IP changes, label renames, credential reveals) for a pairing. |
| `/api/v1/companion/pairings/{id}/qr.svg` | `GET` | Raw `image/svg+xml` QR code for seamless optical onboarding from mobile companion cameras. Requires an authenticated admin session (`requireSettingsAdmin` — same gate as `GET /{id}/credentials`; this PR newly applies that gate, stricter than the global `auth.RequireAdmin` GET bypass). Also records a fail-closed `CREDENTIALS_REVEALED` audit row (channel `qr_svg`). Errors: `404` unknown pairing; `410` revoked/expired; `409` sealed under a different `BRANCHDAM_SECRET_KEY`; `500` sealed credentials exist but no key is configured (same actionable detail as `GET /{id}/credentials`). |
