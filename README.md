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
- **Storage Safety by Design**: The Tier-3 master archive is mounted strictly read-only (`:ro`) and write-guarded at the application level. branchDAM will never modify, rename, or delete your camera RAW masters.
- **Delivery & Ecosystem Sync**:
  - Automatically triggers external library rescans in **Immich** upon export discovery.
  - Metadata inheritance (`POST /api/v1/assets/{id}/inherit-metadata`) propagates parent EXIF, camera tags, and GPS coordinates down to exported deliverables.
  - Verified Tier-1 cache pruning safely purges local scratch copies only when verified Tier-3 archive masters exist on disk.

---

## Integrations & Ecosystem

branchDAM connects across cameras, workstations, editing suites, and home server targets:

| Integration / Tool | Layer | Mechanism | Lineage Confidence | Reference & Guides |
|---|---|---|---|---|
| **DaVinci Resolve** | Workstation Hook | Python render hook generating `<render>.dam.json` | Tier 1 (1.00) | [`hooks/resolve`](https://github.com/s3ntin3l8/branchdam-agent/tree/main/hooks/resolve), [`docs/dam-manifest.md`](docs/dam-manifest.md) |
| **Skylum Luminar Neo** | Workstation Agent | Read-only SQLite `catalog.db` inspection (`luminar-sync`) | Tier 2 (0.89) | [`branchdam-agent` Luminar Docs](https://github.com/s3ntin3l8/branchdam-agent/blob/main/docs/luminar-catalog.md) |
| **Immich** | Server Push | HTTP external-library scan trigger (`POST /api/libraries/{id}/scan`) | Sync State | [`docs/workflow-coverage.md` §6](docs/workflow-coverage.md#6-immich-integration), [`docs/configuration.md`](docs/configuration.md#immich) |
| **Workstation Agent** | Desktop (Win/macOS/Linux) | SD card ingest, dual-copy verified write, offline queue (`queue.db`) | REST Events | [`s3ntin3l8/branchdam-agent`](https://github.com/s3ntin3l8/branchdam-agent) |
| **NLE Timelines** | Server Parser | Native `.drp`, `.fcpxml`, `.edl` project file introspection | Tier 1 (1.00) | [`docs/project-paths.md`](docs/project-paths.md) |

---

## Quick Start

### 1. Production Bring-Up (Docker Compose)

The standard deployment runs behind Traefik v3 and Authentik ForwardAuth:

1. Copy configuration templates:
   ```sh
   cp config.example.yaml config.yaml
   cp .env.example .env
   ```
2. Create `compose.override.yaml` to configure your storage mounts (mount your master archive as `:ro`):
   ```yaml
   services:
     branchdam:
       volumes:
         - ./config.yaml:/config/config.yaml:ro
         - /path/to/archive:/storage/archive:ro   # Tier 3 Master Archive (Read-Only)
         - /path/to/exports:/storage/exports:rw   # Tier 2 Exports (Read-Write)
   ```
3. Start the container:
   ```sh
   docker compose up -d
   ```

See [`docs/deploy.md`](docs/deploy.md) for the full bring-up runbook and [`docs/forward-auth.md`](docs/forward-auth.md) for the Authentik/Traefik configuration.

---

## Documentation

### Getting Started & Setup
- [`docs/deploy.md`](docs/deploy.md) — First bring-up runbook and container configuration.
- [`docs/configuration.md`](docs/configuration.md) — Field-by-field reference for `config.yaml` and environment variables.
- [`docs/forward-auth.md`](docs/forward-auth.md) — Traefik v3 and Authentik ForwardAuth configuration.
- [`docs/deploy-topology.md`](docs/deploy-topology.md) — Worked multi-machine deployment topology (NAS archive + workstation editing).

### Workflows & Lineage
- [`docs/workflow-coverage.md`](docs/workflow-coverage.md) — Step-by-step SD card to Immich workflow audit.
- [`docs/dam-manifest.md`](docs/dam-manifest.md) — `.dam.json` project manifest specification.
- [`docs/project-paths.md`](docs/project-paths.md) — Tier-1 project-file path resolution rules.

### Operations & Architecture
- [`docs/operations.md`](docs/operations.md) — Upgrades, database backups, cache pruning, and troubleshooting.
- [`docs/schema.md`](docs/schema.md) — SQLite schema design decisions and invariants.
- [`docs/agent-protocol.md`](docs/agent-protocol.md) — Workstation agent REST/SSE transport protocol ADR.
- [`docs/roadmap.md`](docs/roadmap.md) — Phasing and architecture history.

---

## Development

```sh
make help          # list all targets
make build         # go build ./... (stubs web/dist first if the SPA hasn't been built)
make test          # go test -race ./...
make lint          # pre-commit run --all-files
make dev-all       # runs Go API server (:8080) and Vite dev server (:5173) together
```

See [`CLAUDE.md`](CLAUDE.md) for detailed architecture notes, key invariants, and developer commands.

---

## License

AGPL-3.0
