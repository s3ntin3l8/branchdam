# branchDAM

A self-hosted Digital Asset Management server that models media as a
**version node graph**: a camera RAW is a root node, and project files,
proxies, and exported JPEG/MP4 hang off it as child nodes joined by
confidence-weighted lineage edges. Runs behind Traefik v3 + Authentik
ForwardAuth, on SQLite in WAL mode.

- **Design spec (original):** [`docs/spec/original-spec.md`](docs/spec/original-spec.md)
  — committed as received; the implementation deliberately diverges from it
  in nine places. See [`docs/schema.md`](docs/schema.md) for the itemized
  deviations and why. Where they disagree, the code is right.
- **Deploying:** [`docs/deploy.md`](docs/deploy.md) (first bring-up),
  [`docs/configuration.md`](docs/configuration.md) (every `config.yaml` field),
  [`docs/operations.md`](docs/operations.md) (upgrades, backups, troubleshooting)
- **Forward-auth / Authentik setup:** [`docs/forward-auth.md`](docs/forward-auth.md)
- **Roadmap (increment 2+):** [`docs/roadmap.md`](docs/roadmap.md)
- **Design docs:** [`docs/agent-protocol.md`](docs/agent-protocol.md) (REST vs. gRPC ADR for the
  agent contract), [`docs/project-paths.md`](docs/project-paths.md) (Tier-1 path resolution),
  [`docs/dam-manifest.md`](docs/dam-manifest.md) (`.dam.json` spec),
  [`docs/google-photos.md`](docs/google-photos.md) (feasibility spike, resolved no-go)

## Status

Roadmap phases 0–9 are landed: the corrected schema, storage-tier write guard,
dual-hash fingerprinting, directory indexing + fsnotify watching + differential
sweeping, EXIF extraction, the scan pipeline (including move-detection —
`MISSING`/rebase), Tier-2 and Tier-3 lineage resolution, Tier-1 project-file
introspection, bounded multi-hop lineage traversal, group-based authorization,
the agent-server contract (`event_queue` drain, handshake, path rebase),
Authentik/API-key auth, the full REST + SSE API, the React dashboard, Immich
push, TTL cache pruning, and a production Docker image — all built, tested,
and CI-verified. See [`CLAUDE.md`](CLAUDE.md) for the architecture and package
map, [`docs/schema.md`](docs/schema.md) for the schema decisions and deviation
ledger, and [`docs/roadmap.md`](docs/roadmap.md) for phase-by-phase detail.

The one open roadmap item is phase 10, the workstation agent (Tauri/Rust) —
filed as a placeholder tracking issue (#62), its repo location still an open
decision. Everything else the original spec asked for either shipped or was
resolved as a deliberate no-go (Google Photos push, see
[`docs/google-photos.md`](docs/google-photos.md)).

Ready to test against real media? Start with [`docs/deploy.md`](docs/deploy.md).

## Development

```sh
make help          # list all targets
make build          # go build ./... (stubs web/dist first if the SPA hasn't been built)
make test           # go test -race ./...
make lint            # pre-commit run --all-files
make dev            # go run ./cmd/branchdam -config config.yaml
```

Copy `config.example.yaml` to `config.yaml` and `.env.example` to `.env`
before running locally. See [`CLAUDE.md`](CLAUDE.md) for frontend and Docker
commands, and [`docs/forward-auth.md`](docs/forward-auth.md) for the
Authentik/Traefik setup.

Run at most one branchdam instance per database file. Startup reconciles any
`scan_jobs` row left `RUNNING` by a crashed prior process, on the assumption
that this process is the only writer -- `make dev` pointed at a database a
running Docker container already owns would mark that container's live jobs
as failed.
