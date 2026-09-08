# Integrations & Manifests Guide

Comprehensive guide to branchDAM's ecosystem integrations, project manifests, NLE timeline parsers, and external catalog synchronizations.

---

## 1. DaVinci Resolve & Project Manifests (`.dam.json`)

The `.dam.json` manifest is branchDAM's native project manifest format. It allows video editing workflows to explicitly document source media references, proxy relationships, and rendered deliverable exports.

### Manifest Properties & Invariants
- **Filename convention:** The filename must end with `.dam.json` (e.g. `commercial_spot_final.dam.json`). Plain `.json` files are not ingested as manifests.
- **Lineage rating:** Resulting edges are classified as **Tier 1 (Deterministic)** with **Confidence 1.00** and relationship `PROJECT_SIDECAR`.
- **Directionality:** Source references listed in `media_references` become *parents* of the manifest/render node. Listing a rendered deliverable under `role: "export"` asserts the source media are parents of the render.

### JSON Schema Specification
```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "DAMProjectManifest",
  "type": "object",
  "properties": {
    "version": {
      "type": "string",
      "example": "1.0"
    },
    "project_name": {
      "type": "string",
      "example": "Commercial Spot 2026"
    },
    "media_references": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "raw_path": {
            "type": "string",
            "example": "D:\\Footage\\CameraA\\A001_C001.mov"
          },
          "path": {
            "type": "string",
            "description": "Accepted fallback if raw_path is absent."
          },
          "role": {
            "type": "string",
            "enum": ["media", "proxy", "export", "audio"],
            "default": "media"
          }
        },
        "required": ["raw_path"]
      }
    }
  },
  "required": ["version", "project_name", "media_references"]
}
```

### DaVinci Resolve Post-Render Hook
The workstation agent includes a Python post-render hook for DaVinci Resolve Studio:
[`hooks/resolve/branchdam_render_hook.py`](https://github.com/s3ntin3l8/branchdam-agent/tree/main/hooks/resolve)

When a timeline render completes, the hook queries DaVinci Resolve's scripting API, extracts all media pool clips present in the rendered timeline, and generates `<output_file>.dam.json` alongside the export deliverable.

---

## 2. NLE Timelines & Path Rewrites

branchDAM includes native timeline parsers that inspect project files directly:
- **DaVinci Resolve Projects (`.drp`)**
- **Final Cut Pro XML (`.fcpxml`)**
- **Edit Decision Lists (`.edl`)**

### Workstation-to-Container Path Rewriting
Project files reference media using host workstation paths (e.g. Windows `D:\Footage\Clip01.mov` or macOS `/Volumes/Video/Clip01.mov`). When the server container indexes these projects, it maps host paths to container paths using a 3-step resolution process:

1. **Exact Container Match:** If the referenced path already matches a live node's `file_path` inside the container storage mounts.
2. **Operator Prefix Rewrites (`pathRewrites` in `config.yaml`):**
   ```yaml
   pathRewrites:
     - from: "D:\\Footage\\"
       to: "/storage/projects/Footage/"
     - from: "/Volumes/Video/"
       to: "/storage/projects/Video/"
     - from: "/Users/editor/Movies/"
       to: "/storage/projects/Movies/"
   ```
   - Converts backslashes to forward slashes.
   - Evaluates prefix replacements in order.
3. **Basename Fallback:** If prefix rewriting yields no match, searches live nodes by exact filename stem and matches candidates.

---

## 3. Skylum Luminar Neo

Non-destructive photo editing in Skylum Luminar Neo is indexed via `branchdam-agent luminar-sync`:
- Opens the local SQLite catalog file (`catalog.db`) with `?mode=ro` (safe for concurrent access).
- Reads edit operations and links exported derivative files back to their source camera RAW masters.
- Emits `EVENT_EDGE_ATTACHED` edges at **Tier 2 (Confidence: 0.89)** into branchDAM's Audit Queue.

See the [Luminar Catalog Schema Reference](https://github.com/s3ntin3l8/branchdam-agent/blob/main/docs/luminar-catalog.md) for query mappings.

---

## 4. Immich (External Library Push)

branchDAM integrates with self-hosted [Immich](https://immich.app) instances to automatically trigger external library rescans upon discovering newly exported deliverables.

### Architecture
- **External Library Mount:** Immich is configured with an **External Library** pointed at branchDAM's Tier-2 exports directory (`/storage/exports`).
- **Rescan Trigger:** When branchDAM discovers new exported files in Tier 2, its sync worker triggers `POST /api/libraries/{id}/scan` to update Immich immediately.
- **Supervisor Safety:** The Immich sync worker supports live settings reload (`PUT /api/v1/settings`) and joins cleanly during graceful server shutdown.

### Prerequisites & caveats
- **Immich must use an external library, not a managed one.** An Immich library that is managed
  (internal ingest) rather than external is invisible to branchDAM permanently. The integration
  is exactly one call — `POST /api/libraries/{library_id}/scan` against an **external** library —
  and transfers no bytes. Existing managed-library assets do not appear in branchDAM's graph and
  never will without a separate migration; only assets that later land in the export path and get
  indexed via a new external library are covered.
- **`immich.exportPath` and Immich's external-library path must be the same string.** The sync
  worker filters live nodes by `exportPath` as branchDAM resolves it; Immich scans the library
  path as Immich resolves it. If the two containers mount the shared export directory at
  different paths, the sync worker enqueues nothing indefinitely, with no error surfaced.
  Mounting the same host directory at the same container path in both containers avoids this.
- **The sync worker is all-or-nothing.** An empty or unresolved `immich.apiUrl` or
  `immich.libraryId` disables it entirely (logged once at startup). An empty library ID would
  otherwise call `POST /api/libraries//scan` and 404 forever.

For the field-level `config.yaml` reference for `immich.*`, see [`configuration.md`](configuration.md).

---

## 5. Workstation Companion Agent

The [`branchdam-agent`](https://github.com/s3ntin3l8/branchdam-agent) companion CLI and menu bar application runs directly on ingestion workstations:
- **Bit-for-Bit Dual Ingest:** Reads camera SD cards once and streams simultaneously to local edit SSDs and the remote NAS master archive.
- **Cache-Busting Verification:** Performs unbuffered re-reads (`O_DIRECT` on Linux, `F_NOCACHE` on macOS, `FILE_FLAG_NO_BUFFERING` on Windows) before signaling safe card ejection.
- **Offline Field Mode:** Persists ingest records locally in `queue.db` when disconnected from LAN, draining and rebasing paths (`POST /api/v1/agent/rebase`) once reconnected.

For the mobile companion app (a separate agent), see [`mobile.md`](mobile.md).

---

## 6. Google Photos (no-go spike)

Google Photos sync was investigated as research spike issue #56. The verdict is **no-go**, with
two reasons:

1. **branchDAM's sync layer has no byte-transfer capability** — the Immich integration triggers a
   library rescan rather than uploading bytes (`integrations.md` §4 above), and there is no path
   to push media bytes to Google Photos from the server.
2. **No way to integrity-verify a Google Photos copy against `full_hash`** — Google Photos'
   re-encoding would invalidate any BLAKE3 comparison, and the platform offers no verifiable
   round-trip.

The push direction survived Google's 2025-04-01 Photos Library API cuts (quota is ample, OAuth
has a workable path pending confirmation), but the above two constraints make it impractical
for branchDAM's architecture.

For the full research spike, including the OAuth workaround, quota analysis, and a pending
8-day refresh-token test, see [`google-photos.md`](google-photos.md).
