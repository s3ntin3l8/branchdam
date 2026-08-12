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
- **Forward-auth / Authentik setup:** [`docs/forward-auth.md`](docs/forward-auth.md)

## Status

Increment 1 (server + SPA) is complete: a corrected schema, storage-tier write
guard, dual-hash fingerprinting, directory indexing + fsnotify watching,
EXIF/ffprobe extraction, the scan pipeline, Tier-2 lineage resolution,
Authentik/API-key auth, the full REST + SSE API, the React dashboard, and a
production Docker image are all built, tested, and CI-verified — see
[`CLAUDE.md`](CLAUDE.md) for the architecture and package map, and
`docs/schema.md` for the schema decisions and deviation ledger against the
original spec.

The workstation agent (Tauri/Rust), DaVinci Resolve hook, Luminar catalog
reader, Immich push, and Google Photos push are deferred to later increments
against the schema and OpenAPI contract this one establishes.

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
