# AGENTS.md — branchDAM

<!-- mullion:briefing:start -->

Self-hosted Digital Asset Management server. Models media assets as a version
node graph with confidence-weighted lineage edges. Go backend (Huma v2, SQLite
WAL, sqlc, goose) + React 19 SPA (Vite, @xyflow/react, TanStack Query, Tailwind).

Key commands: `make check` (backend), `make check-web` (frontend), `sqlc generate`
(after schema edits — always inspect the diff for corruption). Full invariants
and architecture in `CLAUDE.md`.

<!-- mullion:briefing:end -->
