# Admin Personal Access Tokens (PATs)

Admin PATs are long-lived Bearer tokens an operator (or operator tooling
such as Ansible) can use to call the branchDAM admin API without a
browser session. They are the mechanism behind the issue #453 plan's
"unattended provisioning" story: pair workstations, rotate keys, and
drive the API from configuration management.

## Token format

```
bdam_pat_<43 base64-url characters>
```

The prefix makes tokens grep-able in logs and rotation tooling. The 43
characters encode 32 random bytes (256 bits).

Only the HMAC-SHA256 of the token is stored
(`user_pats.hashed_key`, hex, UNIQUE), keyed with the same pepper
pairing uses for device keys. A database-only compromise cannot
recover any token without the pepper. The plaintext is shown to the
operator exactly once — at mint time — and never re-served.

## Bootstrap mechanism

The kubeadm-init pattern: an env var mints a one-shot wildcard admin
PAT on boot, writes it to a file, and refuses to ever mint again while
the file exists.

```yaml
# config.yaml
admin:
  bootstrapPAT: ${ADMIN_BOOTSTRAP_PAT}   # any non-empty value enables it
```

or directly:

```sh
ADMIN_BOOTSTRAP_PAT=1 branchdam
```

Semantics:

- On boot, if the env var is non-empty **and**
  `<dataDir>/bootstrap-pat.txt` does not exist, the server claims the
  file path first (`O_CREATE|O_EXCL|O_NOFOLLOW`, mode 0600), then
  creates a dedicated bootstrap user (promoted to admin) and mints a
  PAT with scopes `["*"]` in a single transaction, and finally writes
  the plaintext to the already-claimed handle. The exclusive create
  closes both races: a symlink planted at the sentinel path is refused
  rather than followed, and two concurrent boots can't both mint.
  If the DB half fails after the claim, the empty file is removed so
  the failure can't block re-bootstrap.
- If the file already exists, the boot logs a skip and continues —
  the env var is consumed once. Delete the file (and the
  `user_pats` row, if you also want the token dead) to re-bootstrap.
- The bootstrap PAT satisfies every scope-gated route.

For Ansible, the playbook reads the file once, distributes the token
to the target's agent config, and the file stays on the server as the
consume-once sentinel.

## API endpoints

All endpoints require an admin identity (session cookie, forward-auth,
or a PAT). A PAT-authenticated request carries its owner's `users.id`,
so the endpoints work identically regardless of which identity path
authenticated the request. Mint additionally requires the resolved
owner to be a **live admin** (`users.is_admin = 1`, `disabled_at`
NULL) — the same predicate the token lookup enforces — and returns
403 otherwise, rather than issuing a well-formed token that would
401 on first use.

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/users/me/pats` | Mint. Body: `{"name": "...", "scopes": ["..."], "expiresAt": 0}`. Returns the **plaintext once** plus metadata. |
| `GET` | `/api/v1/users/me/pats` | List (limit/offset query params). Never includes plaintext — only the first 8 hex chars of the stored hash (`hashedKeyPrefix`). |
| `POST` | `/api/v1/users/me/pats/{id}/revoke` | Soft-delete (sets `revoked_at`). Revoked tokens fail closed with 401. Returns **404** when the id matches no live PAT owned by the caller — revoking an unknown, foreign, or already-revoked id is never a silent success. |

Mint and revoke are written to `actor_audit` (`pat.minted` /
`pat.revoked`) with the hash prefix, never the plaintext or full hash.

## Scope model

A token's `scopes` is a JSON array of strings. The wildcard `"*"`
satisfies every scope; otherwise the requested scope must appear as an
exact match. An empty scope list satisfies nothing (fail-closed
default).

Scopes are enforced **per admin route group** by the request-routing
wrapper (`patScopeFor` in `internal/httpapi/server.go`):

| Route group | Required scope |
|---|---|
| `/api/v1/users/me/pats*` | `pats:write` |
| `/api/v1/companion/pairings*` | `pairings:write` |
| everything else (non-agent) | `admin` |

So a `["pairings:write"]` token drives the pairing API but gets 403 at
the PAT-management endpoints, and the bootstrap PAT's `["*"]` passes
everywhere. A new route group gains its own scope by adding a prefix
to `patScopeFor` and an entry in the handler map.

`is_admin` on a PAT principal is not frozen at mint time: the lookup
query joins the owner's `users` row and filters on `is_admin = 1 AND
disabled_at IS NULL`. Demoting or disabling the owner invalidates all
of their live tokens on the next request (401, identical to a revoked
token) — the same live-authority posture the session middleware takes.

`last_used_at` is bumped asynchronously, throttled to one write per
60 seconds per token: an in-process per-token gate (`shouldTouch`)
collapses a request burst before any goroutine is spawned, and the SQL
`WHERE` in `TouchUserPAT` is the backstop for the same window.

## Request routing

`Authorization: Bearer bdam_pat_...` on any non-agent path is
authenticated by the PAT middleware and bypasses the forward-auth /
session-cookie identity extraction — a request can't carry both a PAT
and a session, and the PAT path never consults `X-Authentik-*`
headers. Requests without a PAT header, and **all** requests under
`/api/v1/agent/*`, continue through the existing chains unchanged: a
PAT header on an agent route never short-circuits the agent key
validation.
