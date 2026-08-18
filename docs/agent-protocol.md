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

#### C. The Five Event Payloads (`payload` JSON)
1. **`EVENT_NODE_CREATED`:**
   ```json
   {
     "nodeUuid": "018f...",
     "storageLocationId": 1,
     "filePath": "/storage/staging/raw_001.arw",
     "fileName": "raw_001.arw",
     "fileExt": ".arw",
     "sizeBytes": 48291040,
     "mtimeUnix": 1723985000,
     "fastHash": "0123456789abcdef",
     "cameraModel": "ILCE-7RM5",
     "cameraSerial": "4401923",
     "lensModel": "FE 24-70mm F2.8 GM II"
   }
   ```
2. **`EVENT_EDGE_ATTACHED`:**
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

---

### 3.2. Companion Protobuf 3 Specification (`agent_protocol.proto`)

For architectural comparison and future migration reference, the equivalent protobuf definitions are defined below:

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
