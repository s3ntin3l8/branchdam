<!--
  PROVENANCE NOTE — read this before the document below.

  This is the design specification for branchDAM as originally received,
  committed VERBATIM (byte-for-byte, unedited body) so the reasoning behind
  every implementation decision stays auditable and its ASCII tier/flow
  diagrams remain the best single-page overview of the product.

  It is a HISTORICAL INPUT, not the live contract. The implementation
  deliberately diverges from this document's SQLite DDL (§6) and Traefik
  compose (§8) in nine places — an incoherent/incomplete storage-tier
  vocabulary, a lifecycle state machine that shares no values with the actual
  status column, a UNIQUE constraint that makes the document's own
  version-collision rule impossible, a relationship type that is actually a
  derived state, an unguarded self-recursive trigger, ON DELETE CASCADE where
  the product needs history preserved, no DAG enforcement, a "cryptographic"
  hash that isn't one, and a primary-key type that can't be minted offline by
  the workstation agent.

  Where this document disagrees with the code, THE CODE IS RIGHT.
  See /docs/schema.md for the itemized deviation ledger mapping each spec
  section to what actually shipped, and why.
-->

# System Specification & PRD: Media Graph DAM (BranchDAM)

**System Name:** Media Graph Digital Asset Management System (`branchdam`)

**Target Environment:** Docker Container on Linux NAS / Central Server + Desktop Workstation Agent (macOS / Windows)

**Database:** SQLite 3 in WAL Mode (Embedded, Local-First Node Graph)

**Reverse Proxy & Security:** Traefik v3 + Authentik ForwardAuth + Static API Keys

---

## 1. Executive Summary & Core Architectural Goals

Media Graph DAM (`branchdam`) is an open-source, local-first Digital Asset Management system designed to solve cross-platform media fragmentation across professional camera imports (Sony DSLMs, DJI Pocket 3, Pixel phones), non-destructive editing suites (DaVinci Resolve, Skylum Luminar), and multi-cloud delivery destinations (Immich, Google Photos).

Rather than managing files as flat directory lists, `branchdam` treats every media item as an immutable **Version Node Graph**. A master camera RAW file forms the root node, while project files (`.drp`, `.lmp`), intermediate proxies, sidecar edits, and final exports (`.mp4`, `.jpg`) exist as child nodes connected by directed, confidence-weighted lineage edges.

---

## 2. Storage Tier Architecture

Storage is explicitly partitioned into three operational tiers and one transient edge tier:

```
┌─────────────────────────────────────────────────────────────┐
│ TIER 0: LOCAL STAGING (Laptop NVMe / Removable Media)       │
│ - Volatile ingest buffer for travel/field work               │
│ - Managed by Workstation Agent + Local SQLite queue         │
└──────────────────────────────┬──────────────────────────────┘
                               │ Auto-Promote / Sync
                               ▼
┌─────────────────────────────────────────────────────────────┐
│ TIER 1: LOCAL SCRATCH (Workstation NVMe)                    │
│ - High-speed local SSD: DaVinci render caches, proxies      │
│ - Managed by Workstation Agent; auto-pruned on completion   │
└──────────────────────────────┬──────────────────────────────┘
                               │ Export Render / Sidecar Sync
                               ▼
┌─────────────────────────────────────────────────────────────┐
│ TIER 2: EXPORT & DELIVERY TIER (Central Server / Read-Write) │
│ - Path: /storage/exports/                                   │
│ - Final JPEGs/MP4s; indexed as Immich External Library      │
└──────────────────────────────┬──────────────────────────────┘
                               │ Ingest & Relink
                               ▼
┌─────────────────────────────────────────────────────────────┐
│ TIER 3: MASTER ARCHIVE TIER (Central NAS / STRICT READ-ONLY) │
│ - Path: /storage/archive/                                   │
│ - Immutable camera originals (ARW, Log MP4s); container :ro  │
└─────────────────────────────────────────────────────────────┘

```

---

## 3. The 6 Core System Pillars

### Pillar 1: Identity & Graph Matching Logic

* **Dual Fingerprinting Model:**
* `fast_hash`: `xxHash64` computed over the first 2 MB, middle 2 MB, last 2 MB, plus total byte size. Execution latency $< 5\text{ms}$. Used for instant path remapping during moves/renames.
* `full_hash`: Full-stream `xxHash64` for cryptographic duplicate detection.
* `phash`: 64-bit Discrete Cosine Transform (DCT) perceptual image hash for visual similarity matching.


* **Tiered Edge Resolution Pipeline:**
1. *Tier 1 (Confidence 1.00):* Project Introspection. Parsing `.drp` archives, `.fcpxml`, `.edl`, or Luminar `catalog.db` sidecars to extract explicit source file paths.
2. *Tier 2 (Confidence 0.90–0.99):* Deterministic Metadata. Regex matching on filename stems (`DSC01234.ARW` $\rightarrow$ `DSC01234_edited.jpg`) and EXIF `XMP:OriginalDocumentID` inspection.
3. *Tier 3 (Confidence 0.70–0.89):* Heuristic Spatial-Temporal Matching. Camera Serial Number + Lens Model pairing + Capture Timestamp match ($\pm 2\text{s}$) + pHash Hamming distance ($\le 10$). Matches with score $< 0.85$ are flagged `NEEDS_REVIEW`.



### Pillar 2: Storage & File Management Strategy

* **Mount Enforcements:**
* Tier 3 (Master Archive) is mounted to the Docker container as strictly **Read-Only** (`:ro`).
* The DAM never writes metadata or `.xmp` sidecars into Tier 3.


* **Hybrid File Watching:**
* Local NVMe paths use kernel events (`inotify`/`FSEvents`) via the Workstation Agent.
* SMB/NFS network shares use Agent-driven HTTP webhooks supplemented by a low-priority background differential directory sweeper (polling `mtime` every 10 minutes).


* **Cache Pruning Engine:** Proxy media and DaVinci `CacheClip` folders on Tier 1 are flagged with time-to-live (TTL) limits and can be purged via one-click UI commands, provided the corresponding Master Node has a verified `full_hash` on Tier 3.

### Pillar 3: Agent-Server Communication & Sync State

* **Transport Protocols:** Binary gRPC streams for high-speed file events; REST/JSON endpoints for Web UI interactions; WebSockets (`/ws/events`) for real-time dashboard progress.
* **Offline Queue & Rebase:** The laptop agent queues offline ingest events in a local SQLite file (`queue.db`). Upon reconnecting (LAN or Tailscale), it executes a `SYNC_HANDSHAKE`. Node relationships link to immutable `node_id` UUIDs rather than file paths, allowing path rebasing from `LOCAL_STAGING` to `CENTRAL_TIER3` without breaking lineage edges.
* **State Machine:** Nodes transition through `CREATED` $\rightarrow$ `INDEXED` $\rightarrow$ `GRAPH_LINKED` $\rightarrow$ `PENDING_CLOUD_PUSH` $\rightarrow$ `PUSHED`. Push idempotency is guaranteed via `SHA256(node_checksum + target_service)`.

### Pillar 4: Metadata Engine & Metadata Lineage

* **EXIF/XMP Inheritance Pipeline:** Prior to pushing child assets to Immich or Google Photos, the server executes an `exiftool` metadata transfer.
* *Inherited from Parent:* `DateTimeOriginal`, `OffsetTimeOriginal` (preserving timezone offset), `GPSLatitude`, `GPSLongitude`, `Make`, `Model`, `LensModel`, `SerialNumber`.
* *Injected Tags:* `XMP-dc:Identifier` (`node_id`), `XMP-xmpMM:DerivedFrom` (`parent_node_id`).


* **Archive Protection:** Metadata is never written into Tier 3 files.

### Pillar 5: Edge Case Handling & Conflict Resolution

* **Moves vs. Deletions:** Missing files are set to `MISSING`. When a new file matches an existing `MISSING` node's `fast_hash`, the path updates without breaking child edges.
* **Version Collisions:** Re-exporting over an existing filename updates the `xxHash64`, creates a new `media_node`, and archives the previous export node into the version history tree.
* **Orphaned Branches:** If a RAW file is removed, child exports transition to `DERIVED_FROM_MISSING_PARENT`, displaying an unlinked alert badge in the UI.

### Pillar 6: Interface & Control Plane

* **Control Surfaces:** Central Web Dashboard (SPA) + System Tray Workstation Agent (Tauri/Rust).
* **Capabilities:** Visual node-graph inspector, manual edge override tool, batch review queue for low-confidence matches, and storage pruning controls.

---

## 4. Software & Integration Specifications

```
                       ┌─────────────────────────────────────┐
                       │     SD Card Ingest / Camera Import  │
                       └──────────────────┬──────────────────┘
                                          │ Bit-for-Bit Hash Verification
                                          ▼
┌───────────────────────────────────────────────────────────────────────────────────┐
│                          TIER 3: MASTER ARCHIVE (NAS)                             │
│               Path: /storage/archive/YYYY/YYYY-MM-DD_[Camera_Model]/              │
└───────────────┬───────────────────────────────────────────────────┬───────────────┘
                │                                                   │
                │ Edit / Source Footage                             │ Project Introspection
                ▼                                                   ▼
┌──────────────────────────────────────┐          ┌─────────────────────────────────┐
│     DaVinci Resolve / Luminar        │          │    Workstation Agent Engine     │
│ (Generates Render + .dam.json)       │─────────>│  - Parses .drp, catalog.db      │
└──────────────────┬───────────────────┘          │  - Injects EXIF lineage headers │
                   │                              └────────────────┬────────────────┘
                   │ Renders Output                                │
                   ▼                                               │
┌──────────────────────────────────────────────────────┐           │
│              TIER 2: EXPORT & DELIVERY               │◄──────────┘
│               Path: /storage/exports/                │
└───────────────┬──────────────────────────────────────┘
                │
                ├──────────────────────────────────────┐
                │ Native Index                         │ REST API Push
                ▼                                      ▼
┌───────────────────────────────┐      ┌────────────────────────────────┐
│   Immich External Library     │      │   Google Photos Library API    │
│    (Zero-copy disk scan)      │      │  (Targeted Desktop Sync)       │
└───────────────────────────────┘      └────────────────────────────────┘

```

### DaVinci Resolve Integration

* **Post-Render Hook:** Installs a Python script into Resolve's `Scripts/Utility/` directory (`DaVinciResolveScript`). Upon completion of a render queue job, it extracts media pool clip paths and outputs a `render_name.dam.json` manifest alongside the exported file in Tier 2.
* **`.drp` Introspection:** Unpacks `.drp` archives in memory, parses `project.xml` for media pool asset paths, and creates `PROJECT_SIDECAR` edges to Master Nodes.

### Skylum Luminar Integration

* **SQLite Reader:** The Workstation Agent opens `LuminarCatalog.luminarcatalog/catalog.db` in **Read-Only** mode (`file:catalog.db?mode=ro`). Upon detecting a new export, it queries `catalog.db` to identify the source RAW UUID/path, transferring EXIF timestamps to the export JPEG.

### Immich Integration

* **External Library Mode:** Renders written to `/storage/exports/immich/` are indexed natively by Immich without duplicating bytes. The DAM server sends an HTTP POST request to trigger an Immich library scan:
`POST /api/libraries/{library_id}/scan`

### Google Photos Integration

* **API Ingest:** Uses `mediaItems:batchCreate` with OAuth2 refresh tokens.
* **Mobile Loop Safeguard:** Mobile photo backups from Pixel phones remain restricted to the camera roll (`DCIM/Camera`). Desktop exports pushed via API are tagged with remote asset IDs in `remote_sync_state` to block duplicate uploads.

### SD Card Auto-Ingest Engine

* Auto-detects volume mounts, validates camera file structures, copies media to `/storage/archive/YYYY/YYYY-MM-DD_[Camera_Model]/`, and performs a bit-for-bit `xxHash64` check before signaling safe ejection.

---

## 5. Security, Traefik & Authentik Integration

* **Authentication Architecture:**
* **Browser UI Access:** Routed through Traefik using Authentik ForwardAuth middleware. User session details are passed to the DAM via headers (`X-Authentik-Username`, `X-Authentik-Email`, `X-Authentik-Uid`).
* **Agent / API Access:** Bypasses ForwardAuth using Traefik path rules. Authenticated via a static HTTP header: `X-API-Key: <secret_token>`.



---

## 6. Database Schema (SQLite DDL)

```sql
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;

-- Storage Locations & Tiers
CREATE TABLE IF NOT EXISTS storage_locations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    root_path TEXT UNIQUE NOT NULL,
    tier TEXT NOT NULL CHECK (tier IN ('LOCAL_STAGING', 'TIER2_EXPORTS', 'TIER3_MASTER_ARCHIVE', 'PROJECTS')),
    access_mode TEXT NOT NULL DEFAULT 'READ_WRITE' CHECK (access_mode IN ('READ_ONLY', 'READ_WRITE')),
    is_active BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Media Asset Nodes
CREATE TABLE IF NOT EXISTS media_nodes (
    id TEXT PRIMARY KEY,                       -- UUIDv7
    file_path TEXT UNIQUE NOT NULL,
    file_name TEXT NOT NULL,
    file_size INTEGER NOT NULL,
    mime_type TEXT NOT NULL,
    fast_hash TEXT,                            -- xxHash64 over 6MB sample
    full_hash TEXT,                            -- xxHash64 stream hash
    phash TEXT,                                -- DCT 64-bit hex
    location_type TEXT NOT NULL DEFAULT 'CENTRAL_TIER3'
        CHECK (location_type IN ('LOCAL_STAGING', 'CENTRAL_TIER3', 'TIER2_EXPORTS', 'EXTERNAL')),
    status TEXT NOT NULL DEFAULT 'INDEXED_SHALLOW'
        CHECK (status IN ('INDEXED_SHALLOW', 'INDEXED_FULL', 'MISSING', 'ARCHIVED', 'HIDDEN')),
    metadata JSON,                             -- EXIF, FFprobe, Camera Serial payload
    storage_location_id INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (storage_location_id) REFERENCES storage_locations(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_nodes_fast_hash ON media_nodes(fast_hash);
CREATE INDEX IF NOT EXISTS idx_nodes_full_hash ON media_nodes(full_hash);
CREATE INDEX IF NOT EXISTS idx_nodes_file_path ON media_nodes(file_path);
CREATE INDEX IF NOT EXISTS idx_nodes_status ON media_nodes(status);

-- Version Graph Lineage Edges
CREATE TABLE IF NOT EXISTS media_edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_node_id TEXT NOT NULL,              -- Parent Node ID
    target_node_id TEXT NOT NULL,              -- Child Node ID
    relationship_type TEXT NOT NULL
        CHECK (relationship_type IN ('DERIVED_FROM', 'PROJECT_SIDECAR', 'PROXY_OF', 'FINAL_EXPORT', 'DERIVED_FROM_MISSING_PARENT')),
    confidence_score REAL NOT NULL DEFAULT 1.00 CHECK (confidence_score BETWEEN 0.00 AND 1.00),
    matching_mechanism TEXT NOT NULL
        CHECK (matching_mechanism IN ('PROJECT_INTROSPECTION', 'EXIF_EXACT', 'REGEX_NAME', 'HEURISTIC_TIME_PHASH', 'MANUAL_OVERRIDE')),
    status TEXT NOT NULL DEFAULT 'CONFIRMED'
        CHECK (status IN ('CONFIRMED', 'NEEDS_REVIEW', 'USER_VERIFIED')),
    manual_override BOOLEAN NOT NULL DEFAULT 0,
    metadata JSON,                             -- Render params, timeline clip details
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_node_id) REFERENCES media_nodes(id) ON DELETE CASCADE,
    FOREIGN KEY (target_node_id) REFERENCES media_nodes(id) ON DELETE CASCADE,
    UNIQUE (source_node_id, target_node_id, relationship_type)
);

CREATE INDEX IF NOT EXISTS idx_edges_source ON media_edges(source_node_id);
CREATE INDEX IF NOT EXISTS idx_edges_target ON media_edges(target_node_id);
CREATE INDEX IF NOT EXISTS idx_edges_status ON media_edges(status);

-- Remote Sync Delivery Tracking
CREATE TABLE IF NOT EXISTS remote_sync_state (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id TEXT NOT NULL,
    target_service TEXT NOT NULL CHECK (target_service IN ('IMMICH', 'GOOGLE_PHOTOS')),
    remote_asset_id TEXT,                      -- Target API ID
    file_checksum TEXT NOT NULL,               -- xxHash64 at time of upload
    sync_status TEXT NOT NULL DEFAULT 'PENDING' CHECK (sync_status IN ('PENDING', 'SYNCED', 'FAILED')),
    error_message TEXT,
    pushed_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (node_id) REFERENCES media_nodes(id) ON DELETE CASCADE,
    UNIQUE (node_id, target_service)
);

CREATE INDEX IF NOT EXISTS idx_sync_node_service ON remote_sync_state(node_id, target_service);
CREATE INDEX IF NOT EXISTS idx_sync_checksum ON remote_sync_state(file_checksum);

-- Event Queue & Replay Buffer
CREATE TABLE IF NOT EXISTS event_queue (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT UNIQUE NOT NULL,             -- Client UUIDv7
    agent_id TEXT NOT NULL,                    -- Workstation identifier
    event_type TEXT NOT NULL
        CHECK (event_type IN ('EVENT_NODE_CREATED', 'EVENT_EDGE_ATTACHED', 'EVENT_NODE_MOVED', 'EVENT_NODE_DELETED', 'EVENT_PATH_REBASED')),
    payload JSON NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'PROCESSED', 'FAILED')),
    error_log TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    processed_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_event_queue_status_time ON event_queue(status, created_at);

-- Triggers for Integrity and Automated Status Transitions
CREATE TRIGGER IF NOT EXISTS trg_media_nodes_updated_at
AFTER UPDATE ON media_nodes
FOR EACH ROW
BEGIN
    UPDATE media_nodes SET updated_at = CURRENT_TIMESTAMP WHERE id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS trg_media_nodes_missing_parent
AFTER UPDATE OF status ON media_nodes
FOR EACH ROW WHEN NEW.status = 'MISSING'
BEGIN
    UPDATE media_edges
    SET relationship_type = 'DERIVED_FROM_MISSING_PARENT'
    WHERE source_node_id = OLD.id AND relationship_type = 'DERIVED_FROM';
END;

CREATE TRIGGER IF NOT EXISTS trg_media_nodes_restore_parent
AFTER UPDATE OF status ON media_nodes
FOR EACH ROW WHEN OLD.status = 'MISSING' AND NEW.status != 'MISSING'
BEGIN
    UPDATE media_edges
    SET relationship_type = 'DERIVED_FROM'
    WHERE source_node_id = OLD.id AND relationship_type = 'DERIVED_FROM_MISSING_PARENT';
END;

```

---

## 7. UI/UX Architecture & Layout Specifications

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ MEDIA GRAPH DAM - CENTRAL DASHBOARD                                               │
├───────────────────┬──────────────────────────────────────────┬───────────────────┤
│ NAVIGATION        │ MAIN GRAPH CANVAS / TIMELINE             │ ASSET INSPECTOR   │
│ - Node Graph      │ ┌──────────────────────────────────────┐ │ ┌───────────────┐ │
│ - Audit Queue (3) │ │  [Sony RAW] ───> [DaVinci .drp]      │ │ │ Thumbnail     │ │
│ - Ingest Jobs     │ │      │                               │ │ ├───────────────┤ │
│ - Storage Health  │ │      └──> [Export v1] ──> (Immich)   │ │ │ Metadata      │ │
│                   │ └──────────────────────────────────────┘ │ │ - Camera: Sony│ │
│ FILTERS           │ AUDIT REVIEW (Confidence < 0.85)         │ │ - Lens: 24-70 │ │
│ [x] Sony A7IV     │ ┌──────────────────┐ ┌─────────────────┐ │ │ - Hashes      │ │
│ [x] DJI Pocket 3  │ │ Master RAW Candidate│ │ Export Render   │ │ ├───────────────┤ │
│ [ ] Unlinked Only │ │ [DSC0123.ARW]    │ │ [Render_v1.jpg] │ │ │ Actions       │ │
│                   │ └──────────────────┘ └─────────────────┘ │ │ [Re-Sync]     │ │
│                   │ [Confirm Edge (0.82)] [Reject Match]     │ │ [Purge Cache] │ │
└───────────────────┴──────────────────────────────────────────┴───────────────────┘

```

### 1. Central Web Dashboard (SPA - React/Svelte + Flow Canvas)

* **Node Graph View:** Visual canvas rendering interactive version trees. Color-coded nodes (Blue = Master RAW, Orange = Project Sidecar, Green = Export, Purple = Immich Synced).
* **Audit Queue Panel:** Side-by-side visual diff tool showing candidate RAW files alongside unlinked exports. Displays EXIF timestamp delta, pHash distance, and matching mechanism. Offers one-click `Confirm`, `Reject`, or `Manual Link`.
* **System Health Monitor:** Real-time gauges for storage capacity across Tiers 1–3 and queue status for background hashing, metadata extraction, and cloud syncing.

### 2. Desktop System Tray Agent (Tauri / Rust)

* **System Tray Menu:** Displays active watch directories, NVMe scratch usage, and sync queue status.
* **SD Card Auto-Ingest Dialog:** Pop-up modal upon inserting a camera card. Shows target directory preview (`/nas/archive/YYYY/YYYY-MM-DD_Camera/`), byte progress, and a bit-for-bit `xxHash64` validation status bar.

---

## 8. Deployment Blueprint

### `docker-compose.yml`

```yaml
version: "3.8"

services:
  branchdam-server:
    image: branchdam/server:latest
    container_name: branchdam_server
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    environment:
      - TZ=UTC
      - DAM_DATABASE_PATH=/var/lib/branchdam/data/dam.db
      - DAM_API_KEY=${DAM_API_KEY}
      - IMMICH_API_URL=http://immich-server:2283
      - IMMICH_API_KEY=${IMMICH_API_KEY}
    volumes:
      # Database State
      - dam_data:/var/lib/branchdam/data
      # Tier 3: Master Archive (STRICTLY READ-ONLY)
      - /mnt/nas/media_archive:/storage/archive:ro
      # Tier 2: Exports & Renders (READ-WRITE)
      - /mnt/nas/exports:/storage/exports:rw
      # Projects Directory
      - /mnt/nas/projects:/storage/projects:rw
    networks:
      - proxy_network
      - internal_network
    labels:
      - "traefik.enable=true"
      # Web UI Router (Protected by Authentik ForwardAuth)
      - "traefik.http.routers.branchdam-web.rule=Host(`dam.local.domain`)"
      - "traefik.http.routers.branchdam-web.entrypoints=websecure"
      - "traefik.http.routers.branchdam-web.tls=true"
      - "traefik.http.routers.branchdam-web.middlewares=authentik-auth@docker"
      - "traefik.http.services.branchdam-web.loadbalancer.server.port=8080"
      # Agent & Webhook API Router (Bypasses ForwardAuth, Authenticated via Static X-API-Key)
      - "traefik.http.routers.branchdam-api.rule=Host(`dam.local.domain`) && PathPrefix(`/api/v1/agent`)"
      - "traefik.http.routers.branchdam-api.entrypoints=websecure"
      - "traefik.http.routers.branchdam-api.tls=true"
      - "traefik.http.services.branchdam-api.loadbalancer.server.port=8080"

volumes:
  dam_data:

networks:
  proxy_network:
    external: true
  internal_network:
    external: false

```

---

## 9. AI Coding Agent Instructions & Implementation Checklist

When implementing this codebase, adhere strictly to the following directives:

1. **Database Layer:** Implement the database using SQLite via the provided DDL script. Ensure `PRAGMA foreign_keys = ON;` and `PRAGMA journal_mode = WAL;` are executed on every connection initialization.
2. **File Hashing Safety:** Never compute full-stream hashes on the main HTTP or file-watcher threads. All `full_hash` (`xxHash64`) jobs must be dispatched to bounded background worker pools.
3. **Master Archive Safety:** All file operations against `/storage/archive/` (Tier 3) must be strictly read-only. Throw an explicit error if any module attempts write, rename, or delete operations on Tier 3 paths.
4. **Metadata Extraction:** Use `exiftool` (via child process execution or native bindings) and `ffprobe` to extract EXIF and video stream JSON payloads. Fall back gracefully to `fast_hash` indexing if metadata parsing fails.
5. **Unit & Integration Testing:** Write unit tests covering:
* Fast-hash collision handling.
* Path rebasing when an offline node's location changes from `LOCAL_STAGING` to `CENTRAL_TIER3`.
* Graph edge creation for `.dam.json` manifests.
* Traefik `X-API-Key` header authentication middleware verification.
