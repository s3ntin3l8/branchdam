# Mobile Companion App (`branchdam-mobile`)

The [branchDAM Mobile Companion App](https://github.com/s3ntin3l8/branchdam-mobile) provides automated photo and video ingest from mobile devices (Android and iOS) directly into branchDAM's Master Archive.

---

## 1. Overview & Architecture

```mermaid
graph TD
    subgraph MobileDevice["Mobile Companion App (Flutter / Dart)"]
        CameraRoll["Camera Roll / DCIM"]
        QueueDB["Local Queue DB (SQLite)"]
        Daemon["Background Ingest Daemon"]
        ReclaimEngine["Safe Space Reclaimer"]
    end

    subgraph Server["branchDAM Server"]
        UploadAPI["POST /api/v1/agent/upload"]
        HandshakeAPI["GET /api/v1/agent/handshake"]
        NamingEngine["Server Naming Engine"]
        MasterArchive["Tier 3: Master Archive (/storage/archive)"]
        ImmichExports["Tier 2: Immich Exports (/storage/exports/immich)"]
        TrashBuffer[".trash/ Buffer & Retention Engine"]
        Immich["Immich Photo Server"]
    end

    CameraRoll -->|Discover New Media| QueueDB
    QueueDB --> Daemon
    Daemon -->|Handshake & Naming Config| HandshakeAPI
    Daemon -->|Raw-Octet Streaming Upload| UploadAPI
    UploadAPI --> NamingEngine
    NamingEngine -->|Persist Master File| MasterArchive
    NamingEngine -.->|Zero-Storage Hardlink| ImmichExports
    UploadAPI -->|BLAKE3 Checksum Verified| Daemon
    Daemon -->|Proof of Backup| ReclaimEngine
    ReclaimEngine -.->|Safely Free Space| CameraRoll
    TrashBuffer -->|Immediate Purge & Rescan| Immich
```

---

## 2. Ingest Lifecycle

1. **Local Discovery & Queueing**:
   - The background daemon detects newly captured photos and videos from device storage.
   - Files are indexed in the local SQLite queue (`queue.db`) with an initial `PENDING_UPLOAD` status.

2. **Handshake & Configuration Sync**:
   - On connection, the client calls `GET /api/v1/agent/handshake` to verify machine credentials, server version, and server-configured naming templates (`ingest.namingTemplate`).

3. **Direct-to-Archive Streaming Upload**:
   - The device streams binary payloads directly to `POST /api/v1/agent/upload` using HTTP headers:
     - `X-Filename`: original media filename (e.g. `PXL_20260829_120000.jpg`).
     - `X-Camera-Model`: device model (e.g. `Pixel 9 Pro`).
     - `X-Capture-Timestamp`: EXIF capture Unix timestamp.
     - `X-Blake3-Hash`: optional pre-computed BLAKE3 hash for end-to-end integrity.
   - The server streams bytes directly to disk in `TIER3_MASTER_ARCHIVE`, calculates its BLAKE3 hash during the stream write, resolves the folder structure according to the active naming template, records the `media_nodes` row, and returns `201 UPLOADED` with the verified `blake3Hash`.

4. **Instant Zero-Storage Immich Display**:
   - Non-RAW displayable images and videos (`.jpg`, `.heic`, `.png`, `.webp`, `.mp4`, `.mov`) are automatically hardlinked into `TIER2_EXPORTS/immich/` under the matching folder structure.
   - A `FINAL_EXPORT` edge is registered in `media_edges`, giving instant access in Immich galleries without taking up duplicate disk space.

5. **Safe Space Reclaim**:
   - Once the server responds with `201 UPLOADED` and matching BLAKE3 hash confirmation, the local file is marked `BACKED_UP`.
   - The Safe Space engine can automatically delete local media from device storage after verified archival backup.

---

## 3. Deletion & 30-Day Trash Buffer

When a user deletes a photo or video:
- The mobile companion app enqueues an `EVENT_NODE_DELETED` agent event.
- Upon processing the event:
  1. **Instant Gallery Purge**: Any linked Immich export file is unlinked immediately, its remote sync state is purged, and an Immich library rescan is triggered so the deleted item vanishes from galleries right away.
  2. **Soft-Delete Isolation**: The master file is safely moved into `.trash/<rel_path>` inside the storage location rather than permanently unlinked.
  3. **30-Day Automated Retention**: A background pruning worker regularly scans `.trash/` across all storage locations and unlinks files older than `trash.retentionDays` (configurable via web UI settings, default: 30 days).

---

## 4. Pairing & Authentication

### 4.1. Overview

Per-device pairing keys (`device_pairings` / `device_pairing_keys`) replaced the
single shared `BRANCHDAM_AGENT_API_KEY` as of #companion-pairing. The
env-var key remains valid indefinitely as a bootstrap path so legacy
workstation agents and previously-paired mobile apps continue to work
without re-pairing. Operators can migrate at their own pace; once no
device or workstation agent depends on the env-var key, it can be unset
in `.env` (no in-server flag — operator-driven).

Authentication as a paired device attaches `Principal{Name: <agent_id>, Kind: KindMachine}`.
The env-var bootstrap path attaches `Principal{Name: "env-bootstrap", Kind: KindMachine}`.
Cross-checks against body.agent_id in `/agent/handshake` and `/agent/events`
block a paired device from impersonating another device's identity in
either direction.

### 4.2. Pairing flow

1. Open the branchDAM web UI at **Companion Pairing** (sidebar link under "Storage Health" and "Settings"; also reachable from the Settings page's "Companion Pairing" card).
2. Click **Pair new device**, enter a friendly label (e.g. "Björn's iPhone 16 Pro"), submit.
3. The server mints a unique `agent_id` (e.g. `dev-abc12345`) and an initial API key. Both are shown exactly once in the modal: a QR code (scannable by the mobile app) and the plaintext key as a copy-to-clipboard widget.
4. In the mobile app, scan the QR or enter the server URL, agent ID, and API key manually. The app stores them in the OS keychain (iOS `AppleKeychain` / Android `EncryptedSharedPreferences`).
5. The app calls `POST /api/v1/agent/handshake` with the new key. The server authenticates via the device-pairing path, returns the naming template, and (if a rotation is pending) a `pendingRotation` hint the mobile app reads on its next handshake.

### 4.3. Key rotation

Operators rotate keys from the same **Companion Pairing** page, per-pairing:

1. Click **Rotate** on the pairing row, choose a grace period (default 24h, max 7d).
2. The server mints a new key, sets `expires_at = now + graceMinutes` on every currently-active key for the pairing, returns the new plaintext once.
3. Both the old and new keys authenticate for the grace window. The mobile app's next `/agent/handshake` is told about the new key via `pendingRotation` in the response; the app stores it in the OS keychain and uses it for subsequent requests.
4. After the grace window, the old key stops authenticating. The pairing's detail panel shows the `expires_at` countdown for in-progress rotations.

This means rotation is non-disruptive: a rotating phone never sees an outage as long as it picks up the new key within the grace window. If it doesn't (e.g. the device is powered off for two weeks), the operator can shorten or extend the grace window per rotation, or revoke the device and re-pair from scratch.

### 4.4. Revocation

**Revoke** on a pairing row terminates the pairing and every key for it.
The mobile app's next request returns 401; the keychain entry is
cleared on the device, the operator re-pairs via QR.

The env-var key can be rotated (or unset) separately from any paired
device. Rotating the env-var key forces every device or workstation
agent that authenticates with it to update simultaneously — useful
when a workstation is decommissioned but the operator hasn't yet
deployed paired devices.

### 4.5. QR payload format

The QR encodes the standard `branchdam://?server=…&key=…&agent=…` URL.
The mobile app's existing `QrParser` accepts both `branchdam://?…` and
`branchdam://…` forms, so old and new QR codes are backward-compatible.

### 4.6. CLI generation (advanced)

Programmatic pairing is via the same HTTP API the SPA uses:
`POST /api/v1/companion/pairings` with `{ "friendlyLabel": "..." }`. The
response includes the plaintext API key in `apiKey`, the rendered QR
SVG in `qrSvg`, and the device's `agentId`. Standard `X-Authentik-*`
headers (admin via Traefik ForwardAuth) are required, same as the
SPA.
