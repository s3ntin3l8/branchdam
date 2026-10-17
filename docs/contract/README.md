# Agent API contract fixtures

`fixtures/` holds one JSON file per request and response body (and query
parameters) of every `/api/v1/agent/*` route, generated from the server's own DTOs by
`TestContractFixtures` (`internal/httpapi/contract_fixtures_test.go`):

| Fixture | What it pins |
|---|---|
| `hello_response` | `POST /api/v1/agent/hello` response body |
| `handshake_request` / `handshake_response` | `POST /api/v1/agent/handshake` (incl. `pendingRotation`) |
| `events_request` / `events_response` | `POST /api/v1/agent/events` envelope; `payload` is the double-encoded JSON string of an `event_*` fixture |
| `event_node_created`, `event_edge_attached`, `event_node_moved`, `event_node_deleted`, `event_path_rebased`, `event_virtual_node_created` | The inner payload of each `eventType` |
| `rebase_request` / `rebase_response` | `POST /api/v1/agent/rebase` |
| `node_status_request` / `node_status_response` | `POST /api/v1/agent/node-status` |
| `check_content_query` / `check_content_response` | `GET /api/v1/agent/check-content` query parameters and response |
| `source_status_query` / `source_status_response` | `GET /api/v1/agent/source-status` query parameters and response |
| `resolve_snapshot_request` / `resolve_snapshot_response` | `POST /api/v1/agent/resolve-snapshot` |
| `telemetry_request` / `telemetry_response` | `POST /api/v1/agent/telemetry` |
| `upload_request_headers` / `upload_response` | `POST /api/v1/agent/upload` headers (the body is the raw file) and response |

The fixtures pin bodies and query parameters, plus the upload route's own
headers. They do not pin the authentication headers every agent route
takes (`X-API-Key`, and `X-Timestamp`/`X-Nonce`/`X-Signature` for signed
requests): those are described in [`docs/agent-api.md`](../agent-api.md),
not here, so `upload_request_headers` is not the complete header contract.

Every fixture sets every field, so it shows the full wire shape; the test
fails when a DTO gains a field the fixture leaves at its zero value, and
every request, event and query fixture must decode strictly (no unknown
field) into the DTO or input struct the route reads. Example values are
placeholders, not real data.

Fields the server accepts but ignores stay pinned with example values,
because the clients still send them: `handshake_request.currentKeyId`,
`rebase_request.storageLocationId`, `event_node_created.storageLocationId`,
`event_node_moved.newStorageLocationId` and
`event_path_rebased.targetStorageLocationId` (the server re-derives the
location from the path; see `internal/agent/types.go`). Only a field whose
non-zero value is refused, or never sent, is left at its zero value
(`contractZeroOK` in the test), such as `event_edge_attached.reviewState`.

## Changing the contract

1. Change the DTO, then run `make contract-fixtures` and commit the
   regenerated `fixtures/`.
2. branchdam-agent and branchdam-mobile each pin a copy of `fixtures/` and
   run a conformance test that decodes every pinned fixture into their own
   DTO and re-encodes it. Refresh a client's copy with its
   `make contract-sync BRANCHDAM=<this checkout>` target in a follow-up
   PR; its test then shows exactly which fields drifted, and its
   `SOURCE` file names the server commit the copy came from.
