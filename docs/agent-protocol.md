# ADR: Agent Transport Protocol — REST vs. gRPC

**Status:** Accepted
**Date:** 2026-08-18
**Context:** Phase 8 / Issue #27, Sub-issue #59 (Spike: gRPC vs REST for the agent event stream)

---

## 1. Executive Summary & Decision

**Decision: Default to REST + JSON over HTTP/2 with static API key authentication and SQLite replay buffering.**

While Spec Pillar 3 casually referenced "Binary gRPC streams for high-speed file events", evaluating the concrete architectural requirements and operational trade-offs reveals that **REST is the optimal protocol** for branchDAM's workstation agent:

1. **Delivery Semantics:** The combination of `auth.AgentChain` (`X-API-Key`), client-side local queuing (`queue.db`), and the server-side `event_queue` replay buffer already guarantees at-least-once delivery, deterministic replayability, and offline disconnect tolerance. A continuous persistent TCP/HTTP/2 stream provides zero additional reliability over discrete idempotent HTTP POST requests with exponential backoff.
2. **Throughput Requirements:** Ingest events occur on file import, render completion, and filesystem renames (tens to hundreds of events per second at peak), which standard HTTP/2 multiplexed REST handles with negligible overhead (sub-millisecond latency per batched request). branchDAM's bottleneck is media hashing and metadata probing, never JSON serialization or HTTP framing.
3. **Operational Simplicity:** Adopting REST leverages the existing Huma OpenAPI router, unified middleware pipeline (`auth.Route`, `securityHeaders`, logging, recovery), and single-port Traefik ForwardAuth routing. Introducing gRPC would require protoc toolchains, CI codegen pipelines, dual HTTP/gRPC listeners, and separate Traefik gRPC routing rules.

---

## 2. Protocol Comparison Matrix

| Dimension | REST (Adopted) | gRPC (Evaluated) |
|---|---|---|
| **Transport** | HTTP/1.1 & HTTP/2 (TLS) | HTTP/2 (h2c/TLS) |
| **Serialization** | JSON (Huma / OpenAPI 3.1) | Protocol Buffers v3 |
| **Authentication** | `X-API-Key` via `auth.AgentChain` | Metadata credentials (`x-api-key`) or mTLS |
| **Disconnection Handling** | Replay buffer + `POST /agent/handshake` | Replay buffer + bidirectional stream reconnect |
| **Server Listener** | Single unified `http.Server` (:8080) | Dual listeners or `cmux` connection multiplexer |
| **Traefik Proxy Config** | Single rule: `PathPrefix('/api/v1/agent')` | Requires `h2c` backend scheme & Traefik gRPC config |
| **Toolchain & CI** | Standard Go toolchain; zero external codegen | `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` in CI/local |
| **Client Ergonomics** | Simple HTTP client in any language (Go, Python, TS, Rust) | Generated protobuf client bindings required |
| **Throughput / Latency** | ~10k req/sec; < 1ms localhost latency | ~25k req/sec; < 0.5ms localhost latency |

---

## 3. Specification of the Message Set

Below is the complete message set specified both as **REST DTOs (JSON Schema)** and as **Protocol Buffers (`.proto`)**.

### 3.1. REST DTO Specifications

#### A. Handshake (`POST /api/v1/agent/handshake`)
- **Request (`AgentHandshakeInput`):**
  ```json
  {
    "agentId": "workstation-macbook-01",
    "clientVersion": "0.1.0",
    "lastProcessedEventUuid": "018f2345-6789-7abc-def0-123456789abc"
  }
  ```
- **Response (`AgentHandshakeOutput`):**
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

#### B. Event Submission (`POST /api/v1/agent/events`)
- **Request (`AgentEventInput`):**
  ```json
  {
    "agentId": "workstation-macbook-01",
    "eventType": "EVENT_NODE_CREATED",
    "payload": "{\"nodeUuid\":\"018f...\",\"filePath\":\"/storage/staging/clip.mov\",\"fastHash\":\"a1b2c3d4e5f60718\"}"
  }
  ```
- **Response (`AgentEventOutput` - Status 202 Accepted):**
  ```json
  {
    "eventId": "018f2346-789a-7bcd-ef01-23456789abcd"
  }
  ```

  **Submission is not idempotent at the transport level, despite §3.2's proto sketch.** The proto
  below shows an `AgentEventRequest.event_uuid` field commented "client-minted UUIDv7 for
  idempotency" and a matching `event_queue.event_uuid` column comment in
  `internal/db/migrations/00001_init.sql`. Neither is true of the real REST wire format:
  `AgentEventInput` has no `eventUuid` field at all, and the server mints `eventId` itself
  (`uuid.NewV7()` in `handleAgentEvent`, `internal/httpapi/routes.go`) on every call, including a
  retry of an identical request. A timed-out request that actually succeeded server-side and is
  retried therefore enqueues a **second**, distinct row -- there is no request-level dedup. The
  only idempotency available today is entity-level: re-sending `EVENT_NODE_CREATED` for a
  `nodeUuid` that already exists is a no-op in the drainer, but a retry that also *corrects* a
  field is silently ignored, since the first write already won. A real `eventUuid` field closing
  this gap is a possible follow-up, not implemented today.

#### C. The Five Event Payloads (`payload` JSON)

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

1. **`EVENT_NODE_CREATED`:** `internal/agent/types.go`'s `NodeCreatedPayload` is normative --
   19 fields in total (excluding the deprecated, ignored `storageLocationId`). The example below
   is the full set; every field past `fastHash` is optional (a normal scan-side probe failure or a
   client that can't compute one simply omits it), but omitting all of them just means an
   agent-ingested master carries none of the promoted metadata a scan would have given it.
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
2. **`EVENT_EDGE_ATTACHED`:** `confidence` is required, in `(0, 1]`, and must be `>=` the
   `needsReviewFloor` (0.50) or the event fails outright.
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
3. **`EVENT_NODE_MOVED`:**
   ```json
   {
     "nodeUuid": "018f...",
     "newFilePath": "/storage/staging/renamed_clip.mov",
     "newFileName": "renamed_clip.mov",
     "mtimeUnix": 1723985100
   }
   ```
4. **`EVENT_NODE_DELETED`:**
   ```json
   {
     "nodeUuid": "018f..."
   }
   ```
5. **`EVENT_PATH_REBASED`:**
   ```json
   {
     "nodeUuid": "018f...",
     "targetFilePath": "/storage/exports/final_cut.mov",
     "targetFileName": "final_cut.mov",
     "mtimeUnix": 1723985200
   }
   ```

#### D. Path Rebase Endpoint (`POST /api/v1/agent/rebase`)

> **Rebasing a target inside Tier 3 (`TIER3_MASTER_ARCHIVE`) succeeds if and only if the file
> already exists there.** This is spec §9's required `LOCAL_STAGING → CENTRAL_TIER3` scenario,
> resolved in issue #167: the workstation agent copies the bytes into the archive itself, then
> calls this endpoint (or sends `EVENT_NODE_MOVED`/`EVENT_PATH_REBASED`) purely to update
> `media_nodes.file_path`/`storage_location_id`. branchDAM never performs the copy and never
> writes, renames, or deletes anything under Tier 3 -- the existence check is a stat
> (`storage.Guard.Exists`), never a write. A Tier 3 target whose file is not yet present is
> refused with `400 Bad Request` (HTTP) / the event marked `FAILED` (queue), so the agent must
> finish copying before calling this. Any other read-only tier has no such exemption and is
> always refused.

- **Request (`AgentRebaseInput`):**
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
- **Response (`AgentRebaseOutput` - Status 200 OK):**
  ```json
  {
    "id": 42,
    "nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
    "storageLocationId": 2,
    "filePath": "/storage/exports/render.mov",
    "status": "REBASED"
  }
  ```

#### E. Node Status Endpoint (`POST /api/v1/agent/node-status`)

> **The first agent-reachable read endpoint.** Every other `/api/v1/agent/*` route is
> write-oriented (submit an event, record a rebase, or -- for `handshake` -- report watermarks
> about events the agent itself submitted). This one exists so an agent can ask the server what
> it currently knows about a batch of `NodeUUID`s it already has locally, without resolving a
> filesystem path and without touching `storage.Guard` at all -- it is a pure
> `media_nodes`/`storage_locations` read. Added to let `branchdam-agent`'s own `prune` subcommand
> decide whether it's safe to delete its local-edit mirror of a file it already durably archived:
> only once the server reports the node `ACTIVE`/`HIDDEN` and hash-verified. See
> `docs/workflow-coverage.md` item 12 for how this differs from (and doesn't solve) real Tier-1
> scratch pruning (`branchdam#230`).

- **Request (`AgentNodeStatusInput`)**, capped at 200 UUIDs per call:
  ```json
  {
    "nodeUuids": ["018f2345-6789-7abc-def0-123456789abc", "018f2345-6789-7abc-def0-123456789abd"]
  }
  ```
- **Response (`AgentNodeStatusOutput` - Status 200 OK):**
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

#### F. Telemetry Endpoint (`POST /api/v1/agent/telemetry`)

> **Workstation Scratch & Cache Telemetry.** Allows connected workstation agents to periodically
> report scratch storage health, capacity metrics (breakdown across render caches, ingest mirrors,
> proxies, and free space), and prune run statistics back to the server without exposing
> filesystem paths or mounting drives.

- **Request (`AgentTelemetryInput`):**
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
- **Response (`AgentTelemetryOutput` - Status 200 OK):**
  ```json
  {
    "ok": true,
    "acknowledgedAtUnix": 1724846401
  }
  ```

  *Persistence & Dashboard Integration:* The endpoint persists the latest agent telemetry snapshot into
  the `agent_scratch_telemetry` table (upsert keyed on `agent_id`) and broadcasts an SSE nudge to connected
  web dashboard clients. Telemetry is queried via `GET /api/v1/storage-health` (augmented `agents` array) or
  `GET /api/v1/storage-health/agents`, surfaced on the Storage Health dashboard with real-time capacity gauges,
  render cache/mirror breakdown, prune run metrics, and low space / critical space / stale status alerts, and
  can be dismissed via `DELETE /api/v1/storage-health/agents/{agentId}`.

---

### 3.2. Companion Protobuf 3 Specification (`agent_protocol.proto`)

For architectural comparison and future migration reference, the equivalent protobuf definitions are defined below.

**This proto was never implemented, and two of its fields describe behavior the real REST API
doesn't have -- treat §3.1 above as normative, this section as a hypothetical sketch only:**
`AgentEventRequest.event_uuid` ("Client-minted UUIDv7 for idempotency") has no counterpart on the
real `AgentEventInput` -- see the idempotency note under §3.1.B. And `AgentEventResponse`'s
`{event_uuid, accepted}` shape doesn't match the real `AgentEventOutput`, which is just
`{"eventId": "..."}` with no `accepted` field (a 202 response is itself the acceptance signal).

```protobuf
syntax = "proto3";

package branchdam.agent.v1;

option go_package = "github.com/s3ntin3l8/branchdam/proto/agent/v1;agentv1";

service AgentService {
  // Handshake to synchronize watermarks upon reconnection
  rpc SyncHandshake(HandshakeRequest) returns (HandshakeResponse);

  // Submit a single event into the server replay buffer
  rpc SubmitEvent(AgentEventRequest) returns (AgentEventResponse);

  // Bidirectional event stream for high-throughput batching
  rpc StreamEvents(stream AgentEventRequest) returns (stream AgentEventResponse);

  // Direct path rebase from LOCAL_STAGING to server storage
  rpc RebasePath(RebaseRequest) returns (RebaseResponse);
}

enum EventType {
  EVENT_TYPE_UNSPECIFIED = 0;
  EVENT_NODE_CREATED = 1;
  EVENT_EDGE_ATTACHED = 2;
  EVENT_NODE_MOVED = 3;
  EVENT_NODE_DELETED = 4;
  EVENT_PATH_REBASED = 5;
}

message HandshakeRequest {
  string agent_id = 1;
  string client_version = 2;
  string last_processed_event_uuid = 3;
}

message HandshakeResponse {
  bool ok = 1;
  string server_version = 2;
  int64 server_time_unix = 3;
  string acknowledged_event_uuid = 4;
  int64 pending_events_count = 5;
}

message AgentEventRequest {
  string agent_id = 1;
  string event_uuid = 2; // Client-minted UUIDv7 for idempotency
  EventType event_type = 3;
  oneof payload {
    NodeCreatedPayload node_created = 4;
    EdgeAttachedPayload edge_attached = 5;
    NodeMovedPayload node_moved = 6;
    NodeDeletedPayload node_deleted = 7;
    PathRebasedPayload path_rebased = 8;
  }
}

message NodeCreatedPayload {
  string node_uuid = 1;
  string file_path = 2;
  string file_name = 3;
  string file_ext = 4;
  int64 size_bytes = 5;
  int64 mtime_unix = 6;
  optional string fast_hash = 7;
  optional string full_hash = 8;
  optional int64 phash = 9;
  optional string camera_model = 10;
  optional string camera_serial = 11;
  optional string lens_model = 12;
  optional int64 captured_at_unix = 13;
  int64 storage_location_id = 14;
}

message EdgeAttachedPayload {
  string source_node_uuid = 1;
  string target_node_uuid = 2;
  string relationship_type = 3;
  double confidence = 4;
  int64 tier = 5;
  string resolver = 6;
  string evidence_json = 7;
  string review_state = 8;
}

message NodeMovedPayload {
  string node_uuid = 1;
  string new_file_path = 2;
  string new_file_name = 3;
  int64 new_storage_location_id = 4;
  int64 mtime_unix = 5;
}

message NodeDeletedPayload {
  string node_uuid = 1;
}

message PathRebasedPayload {
  string node_uuid = 1;
  string target_file_path = 2;
  string target_file_name = 3;
  int64 target_storage_location_id = 4;
  int64 mtime_unix = 5;
  optional string fast_hash = 6;
  int64 size_bytes = 7;
}

message AgentEventResponse {
  string event_uuid = 1;
  bool accepted = 2;
}

message RebaseRequest {
  string node_uuid = 1;
  string target_path = 2;
  int64 mtime_unix = 3;
  string file_name = 4;
  string file_ext = 5;
  int64 size_bytes = 6;
  optional string fast_hash = 7;
  int64 storage_location_id = 8;
}

message RebaseResponse {
  int64 id = 1;
  string node_uuid = 2;
  int64 storage_location_id = 3;
  string file_path = 4;
  string status = 5; // "REBASED" or "CREATED"
}

message AgentScratchStorage {
  string mount_path = 1;
  int64 total_bytes = 2;
  int64 free_bytes = 3;
  int64 used_bytes = 4;
  int64 mirrors_size_bytes = 5;
  int64 render_cache_size_bytes = 6;
  int64 proxies_size_bytes = 7;
  int64 prunable_bytes = 8;
}

message AgentPruneStats {
  int64 last_prune_timestamp_unix = 1;
  int64 last_reclaimed_bytes = 2;
  int64 last_prune_duration_ms = 3;
  map<string, int32> pruned_item_counts = 4;
}

message AgentTelemetryRequest {
  string agent_id = 1;
  string client_version = 2;
  int64 timestamp_unix = 3;
  AgentScratchStorage scratch_storage = 4;
  optional AgentPruneStats prune_stats = 5;
}

message AgentTelemetryResponse {
  bool ok = 1;
  int64 acknowledged_at_unix = 2;
}
```

---

## 4. Scoping Requirements for a Future gRPC Adoption

If higher event volume or binary stream requirements ever demand adopting gRPC in the future, the following toolchain and infrastructure changes would be required:

1. **Toolchain & Local Environment:**
   - Install `protoc` compiler binary (v25+) in dev environments and CI containers.
   - Install `protoc-gen-go` (v1.32+) and `protoc-gen-go-grpc` (v1.3+).
   - Add `make proto-generate` and `make proto-diff` targets to `Makefile` and `.pre-commit-config.yaml`.
2. **Continuous Integration (GitHub Actions):**
   - Update `.github/workflows/ci.yml` to install `protoc` or check in generated code under `proto/gen/go/`.
   - Ensure lint and vet checks include gRPC service interfaces and generated struct tags.
3. **Server Architecture:**
   - Either run a separate gRPC listener on a distinct port (e.g. `:50051`) or use `soheilhy/cmux` / `grpcweb` on the existing HTTP listener.
   - Implement gRPC authentication interceptors mirroring `auth.AgentChain` (`x-api-key` header verification and X-Authentik header stripping).
4. **Traefik & Ingress Routing:**
   - Update `docs/forward-auth.md` and `docker-compose.yml` to configure Traefik `h2c` scheme or dedicated gRPC routers with `websecure` entrypoints.

---

## 5. Conclusion

REST + JSON provides full feature parity with lower complexity, immediate compatibility with the shipped HTTP API stack, and zero friction for workstation agent implementations. The contract established in Phase 8 (#57, #58) serves as the stable, production foundation for the branchDAM agent ecosystem.
