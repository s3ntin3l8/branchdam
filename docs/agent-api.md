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

## 1. Handshake (`POST /api/v1/agent/handshake`)

**Request (`AgentHandshakeInput`):**
```json
{
  "agentId": "workstation-macbook-01",
  "clientVersion": "0.1.0",
  "lastProcessedEventUuid": "018f2345-6789-7abc-def0-123456789abc"
}
```

**Response (`AgentHandshakeOutput`):**
```json
{
  "ok": true,
  "serverVersion": "dev",
  "serverTimeUnix": 1723985000,
  "acknowledgedEventUuid": "018f2345-6789-7abc-def0-123456789abc",
  "pendingEventsCount": 0
}
```

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


---

## 3. The Five Event Payloads (`payload` JSON)

> **`storageLocationId` / `newStorageLocationId` / `targetStorageLocationId` are advisory and
> ignored.** `storage.Guard` exposes no lookup-by-ID, so a payload-supplied location ID is
> fundamentally unverifiable server-side; `storage_location_id` is always re-derived from the
> event's own file path via `storage.Guard.Resolve`. The fields are kept in the wire format so
> an existing agent payload still parses, but the server never trusts them. Similarly,
> `EVENT_EDGE_ATTACHED`'s `reviewState` is advisory and ignored for `AUTO_ACCEPTED`/empty (the
> server always derives `AUTO_ACCEPTED` vs. `NEEDS_REVIEW` from `confidence` and `tier` via the
> same per-tier threshold every other resolver uses) and is an outright error for
> `CONFIRMED`/`REJECTED` -- a human review decision is never the agent's to make.
>
> **A rebase targeting an `ARCHIVED` node_uuid always fails.** `EVENT_NODE_MOVED`,
> `EVENT_PATH_REBASED`, and `POST /api/v1/agent/rebase` all refuse to rebase a node whose
> `lifecycle_state` is `ARCHIVED` -- that node_uuid identifies a superseded version, and rebasing
> it in place would resurrect it. `EVENT_NODE_MOVED`/`EVENT_PATH_REBASED` mark the event `FAILED`
> with `error_log` describing the refusal; `POST /api/v1/agent/rebase` returns `404 Not Found`. An
> agent holding a stale `node_uuid` from before a version collision should treat either as
> terminal, not retry.

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

`confidence` is required, in `(0, 1]`, and must be `>=` the `needsReviewFloor` (0.50) or the
event fails outright.

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
- Sets `media_nodes.lifecycle_state = 'MISSING'` and purges `remote_sync_state`.
- Safely relocates the master file to `.trash/<rel_path>` in the storage location, retaining
  it for 30 days (`trash.retentionDays`) before automated prune unlinks it.
- Purges linked Tier 2 Immich exports immediately (unlinking export files and deleting sync
  state) and triggers an Immich library rescan so the asset disappears from galleries right away.

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

**Response (`AgentUploadResponse` - Status 201 Created):**
```json
{
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "status": "UPLOADED",
  "bytesWritten": 48291040,
  "blake3Hash": "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a",
  "relativePath": "2026/2026-08-29_Pixel-9-Pro/PXL_20260829_120000.jpg"
}
```

---

## 5. Path Rebase Endpoint (`POST /api/v1/agent/rebase`)

> **Rebasing a target inside Tier 3 (`TIER3_MASTER_ARCHIVE`) succeeds if and only if the file
> already exists there.** This is the required `LOCAL_STAGING → CENTRAL_TIER3` scenario,
> resolved in issue #167: the workstation agent copies the bytes into the archive itself, then
> calls this endpoint (or sends `EVENT_NODE_MOVED`/`EVENT_PATH_REBASED`) purely to update
> `media_nodes.file_path`/`storage_location_id`. branchDAM never performs the copy and never
> writes, renames, or deletes anything under Tier 3 -- the existence check is a stat
> (`storage.Guard.Exists`), never a write. A Tier 3 target whose file is not yet present is
> refused with `400 Bad Request` (HTTP) / the event marked `FAILED` (queue), so the agent must
> finish copying before calling this. Any other read-only tier has no such exemption and is
> always refused.

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

**Request (`AgentNodeStatusInput`)**, capped at 200 UUIDs per call:
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
      "verified": true
    },
    {
      "nodeUuid": "018f2345-6789-7abc-def0-123456789abd",
      "found": false,
      "verified": false
    }
  ]
}
```

`verified` mirrors `ListPrunableNodes`' own eligibility predicate exactly: `full_hash` non-NULL
and 64 hex characters (BLAKE3-256). `found: false` is not an error -- it just means no
`media_nodes` row currently has that `node_uuid`.

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

Allows companion applications and workstation agents to perform a pre-flight content hash check before streaming multi-gigabyte uploads. If a file with the identical BLAKE3-256 hash already exists as an active or hidden node (excludes ARCHIVED and MISSING), the agent can skip the upload entirely.

**Authentication:** Requires an Agent Machine Principal.

**Query Parameters:**
- `fullHash` (required): 64-character lowercase hex BLAKE3-256 digest of the candidate file content.
- `fastHash` (optional): 16-character lowercase hex xxHash64 digest (accepted and format-validated for forward compatibility, but currently ignored; lookup is performed solely by `fullHash`).

**Response When Found (Status 200 OK):**
```json
{
  "found": true,
  "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
  "filePath": "2026/2026-08-29_Pixel-9-Pro/PXL_20260829_120000.jpg",
  "lifecycleState": "ACTIVE",
  "indexingStatus": "INDEXED"
}
```

**Response When Not Found (Status 200 OK):**
```json
{
  "found": false
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
  "filePath": "2026/2026-08-29_Pixel-9-Pro/PXL_20260829_120000.jpg",
  "indexingStatus": "INDEXED",
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
| `/api/v1/companion/pairings/{id}/revoke` | `POST` | Revoke device access immediately. Enforces a 401 Unauthorized response on subsequent agent calls. |
| `/api/v1/companion/pairings/{id}` | `DELETE` | Delete device pairing record and its key history. Requires the pairing to be revoked first (`POST /revoke`), returning `409 Conflict` otherwise. |
| `/api/v1/companion/pairings/{id}/audit` | `GET` | Retrieve audit events (creation, key rotation, revocation, IP changes) for a pairing. |
| `/api/v1/companion/pairings/{id}/qr.svg` | `GET` | Raw `image/svg+xml` QR code for seamless optical onboarding from mobile companion cameras. |
