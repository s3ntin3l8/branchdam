<p align="center">
  <img src="web/public/favicon.svg" alt="branchDAM logo" width="64" height="64">
</p>

# branchDAM

A self-hosted Digital Asset Management server that models media as a
**version node graph**: a camera RAW is a root node, and project files,
proxies, and exported JPEG/MP4 hang off it as child nodes joined by
confidence-weighted lineage edges. Runs behind Traefik v3 + Authentik
ForwardAuth, on SQLite in WAL mode.

---

## Key Capabilities

- **Version Node Graph**: Represents your entire media library as a directed graph. Camera RAW originals act as immutable root nodes; edits, proxies, color grades, and exported deliverables connect as child nodes.
- **Multi-Tier Lineage Resolution**:
  - **Tier 1 (Deterministic)**: Native `.dam.json` project manifests and NLE timeline introspection (`.drp`, `.fcpxml`, `.edl`) yielding confidence-1.00 edges.
  - **Tier 2 (Identifiers & Stems)**: XMP `OriginalDocumentID` tracking and normalized filename stem matching.
  - **Tier 3 (Spatial-Temporal Heuristics)**: Links derived exports back to camera masters via camera serial numbers, lens models, capture timestamps (±2s window), and perceptual hash (pHash Hamming distance ≤ 10).
- **Human-in-the-Loop Audit Queue**: Low-confidence candidate links (< 0.85/0.90 thresholds) route directly to an interactive Audit Queue where human confirmations/rejections permanently override automated algorithms.
- **Storage Safety & Lifecycle Governance**: Ingested camera masters are stored safely in the Master Archive. Deletion events trigger soft-delete buffer isolation under `.trash/` with a 30-day safety retention window before automated pruning, preventing accidental loss while keeping active archives tidy.
- **Storage Architecture & Ingest**:
  - Direct streaming ingest (`POST /api/v1/agent/upload`) for mobile companion apps and workstation agents.
  - Server-evaluated folder and naming templates (e.g. `{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}`).
  - 30-day soft-delete trash buffer (`.trash/` isolation) on file deletion with automated retention pruning.
  - Zero-storage hardlink export into Immich for instant gallery browsing.
- **Delivery & Ecosystem Sync**:
  - Automatically triggers external library rescans in **Immich** upon export discovery or deletion.
  - Metadata inheritance (`POST /api/v1/assets/{id}/inherit-metadata`) propagates parent EXIF, camera tags, and GPS coordinates down to exported deliverables.
  - Verified Tier-1 cache pruning safely purges local scratch copies only when verified Tier-3 archive masters exist on disk.

---

## Integrations & Ecosystem

branchDAM connects across cameras, workstations, mobile devices, editing suites, and home server targets:

| Integration / Tool | Layer | Mechanism | Lineage Confidence | Reference & Guides |
|---|---|---|---|---|
| **Mobile Companion App** | Android / iOS | Background daemon, streaming upload, Safe Space reclaim | Tier 3 Direct / Export | [`s3ntin3l8/branchdam-mobile`](https://github.com/s3ntin3l8/branchdam-mobile), [`docs/mobile.md`](docs/mobile.md) |
| **DaVinci Resolve** | Workstation Hook | Python render hook generating `<render>.dam.json` | Tier 1 (1.00) | [`hooks/resolve`](https://github.com/s3ntin3l8/branchdam-agent/tree/main/hooks/resolve), [`docs/integrations.md`](docs/integrations.md) |
| **Skylum Luminar Neo** | Workstation Agent | Read-only SQLite `catalog.db` inspection (`luminar-sync`) | Tier 2 (0.89) | [`docs/integrations.md` §3](docs/integrations.md#3-skylum-luminar-neo) |
| **Immich** | Server Push | HTTP external-library scan trigger (`POST /api/libraries/{id}/scan`) | Sync State | [`docs/integrations.md` §4](docs/integrations.md#4-immich-external-library-push), [`docs/configuration.md`](docs/configuration.md#immich) |
| **Workstation Agent** | Desktop (Win/macOS/Linux) | SD card ingest, streaming upload, offline queue (`queue.db`) | REST Events | [`s3ntin3l8/branchdam-agent`](https://github.com/s3ntin3l8/branchdam-agent), [`docs/integrations.md` §5](docs/integrations.md#5-workstation-companion-agent) |
| **NLE Timelines** | Server Parser | Native `.drp`, `.fcpxml`, `.edl` project file introspection | Tier 1 (1.00) | [`docs/integrations.md` §2](docs/integrations.md#2-nle-timelines--path-rewrites) |

---

## Deployment (Docker Compose)

The standard branchDAM server deployment runs as a container behind Traefik v3 and Authentik ForwardAuth:

1. **Copy configuration templates**:
   ```sh
   cp config.example.yaml config.yaml
   cp .env.example .env
   ```
2. **Configure your volume mounts** in `compose.override.yaml`:
   ```yaml
   services:
     branchdam:
       volumes:
         - ./config.yaml:/config/config.yaml:ro
         - /path/to/archive:/storage/archive:rw   # Tier 3 Master Archive (Read-Write Ingest)
         - /path/to/exports:/storage/exports:rw   # Tier 2 Exports (Read-Write)
   ```
3. **Start the server**:
   ```sh
   docker compose up -d
   ```

For detailed bring-up instructions and reverse proxy configurations, see [`docs/deploy.md`](docs/deploy.md) and [`docs/forward-auth.md`](docs/forward-auth.md).

---

## Documentation Directory

### Mobile & Companion Apps
- [`docs/mobile.md`](docs/mobile.md) — Mobile companion app architecture, background ingest, and Safe Space reclaim.

### Getting Started & Deployment
- [`docs/deploy.md`](docs/deploy.md) — First bring-up runbook and Docker setup.
- [`docs/configuration.md`](docs/configuration.md) — Field-by-field reference for `config.yaml` and environment variables.
- [`docs/forward-auth.md`](docs/forward-auth.md) — Traefik v3 and Authentik ForwardAuth configuration.
- [`docs/deploy-topology.md`](docs/deploy-topology.md) — Multi-machine deployment topology (NAS archive + workstation editing).

### Workflows & Lineage
- [`docs/integrations.md`](docs/integrations.md) — Comprehensive guide to DaVinci Resolve, Luminar Neo, Immich, NLE timelines, and manifests.
- [`docs/workflow-coverage.md`](docs/workflow-coverage.md) — Step-by-step SD card to Immich workflow audit.

### Operations & Architecture
- [`docs/operations.md`](docs/operations.md) — Upgrades, database backups, cache pruning, and troubleshooting.
- [`docs/schema.md`](docs/schema.md) — SQLite schema design decisions and invariants.
- [`docs/agent-protocol.md`](docs/agent-protocol.md) — Workstation & mobile agent REST/SSE transport protocol ADR.
- [`docs/roadmap.md`](docs/roadmap.md) — Architecture history and roadmap records.

---

## Development & Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for local development setup, prerequisites, automated verification commands (`make check`, `make check-web`), and pull request guidelines.

---

## License

AGPL-3.0
